package fabric

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"devicecode-go/bus"
	"devicecode-go/types"
	"devicecode-go/x/strconvx"
)

// ---- link state ----

type linkState int

const (
	linkDown linkState = iota
	linkUp
)

// ---- link status strings (published in link state payload) ----

const (
	statusReady   = "ready"
	statusOpening = "opening"
	statusDown    = "down"
	lineQueueSize = 32
)

// ---- timeouts (local policy) ----
//
// LinkConfig drives the ping cadence (PingInterval) and liveness-stale
// detection (LivenessTimeout). Mirrors session_ctl.lua at
// devicecode-lua@2c88090: pings fire unconditionally every
// ping_interval_s; the link is torn down if no frame arrives within
// liveness_timeout_s. Exports are enabled immediately on link-up
// (after exportStartHoldoff).

const (
	callTimeoutDef     = 5 * time.Second
	waitLogEvery       = 2 * time.Second
	exportStartHoldoff = 1 * time.Second
	// exportMaxPerTick caps the total export messages sent per drain
	// cycle across all subscriptions, keeping UART throughput within
	// the 115200-baud link capacity.
	exportMaxPerTick   = 1
	exportTickInterval = 50 * time.Millisecond
	errPayloadMarshal  = "payload_marshal_failed"

	// Temporary transport recovery policy: the USB/UART path used during OTA
	// can echo MCU-originated JSONL back into the MCU receiver. If exported
	// retained state is in flight while CM5 starts an OTA transfer, the echoed
	// line can contain CM5's xfer_begin spliced into the middle of the state pub.
	// Hold exports quiet from prepare until either xfer_begin arrives or this
	// window expires. Revisit after CM5 update-admission hardening so this does
	// not become OTA semantics.
	transferPrepareQuiet = 10 * time.Second
	// Temporary transport recovery policy: keep telemetry/state exports quiet
	// long enough for the host to send the follow-up updater commit call after
	// xfer_done. On echo-prone UART links, retained export backlog can otherwise
	// splice into the commit JSONL frame.
	transferCompleteQuiet = 10 * time.Second
)

// ---- link reasons and error strings ----

const (
	reasonLinkDown       = "link_down"
	reasonPeerStale      = "peer_stale"
	reasonPeerReset      = "peer_reset"
	reasonSessionReset   = "session_reset"
	reasonHelloRejected  = "hello_rejected"
	reasonTransportDown  = "transport_down"
	reasonTransportWrite = "transport_write_failed"
	reasonNoRoute        = "no_route"
	reasonBusy           = "busy"
	reasonTimeout        = "timeout"
)

// ---- types ----

type inboundCall struct {
	id              string
	topic           []string
	sub             *bus.Subscription
	deadline        time.Time
	transferPrepare bool
}

type outboundCall struct {
	id       string
	req      *bus.Message
	deadline time.Time
}

type readResult struct {
	line []byte
	err  error
}

type linkStatePayload struct {
	LinkID            string `json:"link_id"`
	Status            string `json:"status"`
	Ready             bool   `json:"ready"`
	Established       bool   `json:"established"`
	PeerID            string `json:"peer_id"`
	LocalSID          string `json:"local_sid"`
	PeerSID           string `json:"peer_sid,omitempty"`
	PeerNode          string `json:"peer_node,omitempty"`
	PeerProto         string `json:"peer_proto,omitempty"`
	LastRxUnixMilli   int64  `json:"last_rx_unix_ms,omitempty"`
	LastTxUnixMilli   int64  `json:"last_tx_unix_ms,omitempty"`
	LastPongUnixMilli int64  `json:"last_pong_unix_ms,omitempty"`
	InboundCalls      int    `json:"inbound_calls"`
	OutboundCalls     int    `json:"outbound_calls"`
	Reason            string `json:"reason,omitempty"`
	Err               string `json:"err,omitempty"`
}

// session manages the fabric link state machine over a Transport.
//
// All bus access happens in the main loop goroutine only. TinyGo's
// cooperative scheduler panics if multiple goroutines contend on
// the bus's internal sync.Mutex.
type session struct {
	linkID   string
	nodeID   string
	peerID   string
	localSID string
	tr       Transport
	conn     *bus.Connection
	cfg      LinkConfig

	link           linkState
	peerNode       string
	peerSID        string
	peerProto      string
	lastRxAt       time.Time
	lastTxAt       time.Time
	lastPongAt     time.Time
	exportReadyAt  time.Time
	exportsEnabled bool

	criticalExportSubs          []*bus.Subscription
	criticalExportReplayPending []bool
	exportSubs                  []*bus.Subscription
	exportCallSubs              []*bus.Subscription
	inboundCalls                []*inboundCall
	outboundCalls               []*outboundCall
	nextOutboundID              uint64
	nextPingAt                  time.Time
	txControl                   txLane
	txRPC                       txLane
	txBulk                      txLane
	importedRetained            []bus.Topic // local topics currently retained on the bus due to wire imports
	rpcReady                    bool        // bridge replay complete; gates linkStatePayload.Ready
	incomingTransfer            *incomingTransfer
	completedTransfers          []completedTransfer
	transferQuietUntil          time.Time
	transferQuietReason         string
	beginTransfer               func(transferMeta) (transferSink, error)
}

func (s *session) log(msg string) {
	if !fabricTraceEnabled {
		return
	}
	println("[fabric]", "sid", s.localSID, msg)
}

func (s *session) logKV(msg, key, value string) {
	if !fabricTraceEnabled {
		return
	}
	println("[fabric]", "sid", s.localSID, msg, key, value)
}

// run is the main loop. Blocks until ctx is cancelled.
func (s *session) run(ctx context.Context) {
	s.cfg.applyDefaults()
	lines := make(chan readResult, lineQueueSize)

	go func() {
		defer close(lines)
		for {
			line, err := s.tr.ReadLine()
			if err != nil {
				if errors.Is(err, ErrLineTooLong) {
					s.log("oversized line dropped")
					continue
				}
				select {
				case lines <- readResult{err: err}:
				case <-ctx.Done():
				}
				return
			}
			cp := make([]byte, len(line))
			copy(cp, line)
			select {
			case lines <- readResult{line: cp}:
			case <-ctx.Done():
				return
			}
		}
	}()

	defer s.tr.Close()
	defer s.teardownExports()
	defer s.teardownInbound()
	defer s.teardownOutbound(reasonLinkDown)
	defer s.abortTransfer(reasonLinkDown)
	defer s.log("run stop")

	stale := time.NewTimer(s.cfg.LivenessTimeout)
	defer stale.Stop()

	waitTick := time.NewTicker(waitLogEvery)
	defer waitTick.Stop()

	// Poll subscription channels periodically. Needed because select
	// blocks until a line/timer fires; without this, exported bus
	// messages and async call replies would sit in subscription channels.
	exportTick := time.NewTicker(exportTickInterval)
	defer exportTick.Stop()

	s.publishLinkState("", "")
	s.log("run start")

	for {
		select {
		case <-ctx.Done():
			return

		case res, ok := <-lines:
			if !ok {
				return
			}
			if res.err != nil {
				s.handleLinkDown(reasonTransportDown, res.err.Error())
				return
			}
			beforeRx := s.lastRxAt
			s.dispatch(res.line)
			if s.lastRxAt.After(beforeRx) {
				resetTimer(stale, s.cfg.LivenessTimeout)
			}

		case <-exportTick.C:
			now := time.Now()
			s.drainExports()
			s.drainInbound(now)
			s.drainOutbound(now)
			s.checkTransferTimeout(now)
			s.tickPing(now)
			s.tickReady(now)

		case <-waitTick.C:
			s.logWaiting()

		case <-stale.C:
			if s.link == linkUp {
				s.handleLinkDown(reasonPeerStale, "")
			} else {
				stale.Reset(s.cfg.LivenessTimeout)
			}
		}
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func unixMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func (s *session) currentStatus() string {
	if s.link == linkUp && s.rpcReady {
		return statusReady
	}
	return statusOpening
}

func (s *session) publishLinkState(reason, err string) {
	if s.conn == nil {
		return
	}
	status := s.currentStatus()
	if s.link != linkUp && (reason != "" || err != "") {
		status = statusDown
	}
	s.conn.Publish(s.conn.NewMessage(
		bus.T("state", "fabric", "link", s.linkID),
		linkStatePayload{
			LinkID:            s.linkID,
			Status:            status,
			Ready:             s.link == linkUp && s.rpcReady,
			Established:       s.link == linkUp,
			PeerID:            s.peerID,
			LocalSID:          s.localSID,
			PeerSID:           s.peerSID,
			PeerNode:          s.peerNode,
			PeerProto:         s.peerProto,
			LastRxUnixMilli:   unixMilli(s.lastRxAt),
			LastTxUnixMilli:   unixMilli(s.lastTxAt),
			LastPongUnixMilli: unixMilli(s.lastPongAt),
			InboundCalls:      len(s.inboundCalls),
			OutboundCalls:     len(s.outboundCalls),
			Reason:            reason,
			Err:               err,
		},
		true,
	))
}

func (s *session) markRx() {
	s.lastRxAt = time.Now()
}

func (s *session) markTx() {
	s.lastTxAt = time.Now()
}

func (s *session) handleLinkDown(reason, err string) {
	pendingReason := reason
	if pendingReason == "" {
		pendingReason = reasonLinkDown
	}
	s.link = linkDown
	s.peerNode = ""
	s.peerSID = ""
	s.peerProto = ""
	s.exportReadyAt = time.Time{}
	s.exportsEnabled = false
	s.rpcReady = false
	s.transferQuietUntil = time.Time{}
	s.transferQuietReason = ""
	s.teardownExports()
	s.teardownInbound()
	s.teardownOutbound(pendingReason)
	s.teardownImportedRetained()
	s.abortTransfer(pendingReason)
	s.clearCompletedTransfers()
	s.publishLinkState(reason, err)
	if err != "" {
		s.logKV("link down", "err", err)
	} else if reason != "" {
		s.logKV("link down", "reason", reason)
	}
}

// promoteLink transitions to linkUp, tearing down any prior session state.
// `reason` carries the link-state telemetry tag (e.g. session_reset) and
// is also used as the err string on any pending outbound calls cancelled
// by the transition, matching rpc_bridge.lua's session-replace behaviour.
//
// On a session-reset transition (re-promote with the link already up),
// imported retained facts are unretained locally so consumers don't see
// stale data from the previous CM5 session — mirrors rpc_bridge.lua's
// invalidate_imported_retained on generation bump. rpcReady is held low
// until the export holdoff elapses (see tickReady), gating
// linkStatePayload.Ready.
func (s *session) promoteLink(reason string) {
	if s.link == linkUp {
		if reason == "" {
			reason = reasonPeerReset
		}
		s.abortTransfer(reason)
		s.teardownExports()
		s.teardownInbound()
		s.teardownOutbound(reason)
		s.teardownImportedRetained()
		s.clearCompletedTransfers()
	}
	s.link = linkUp
	s.rpcReady = false
	s.setupExports()
	s.exportsEnabled = true
	s.exportReadyAt = time.Now().Add(exportStartHoldoff)
	s.nextPingAt = time.Now().Add(s.cfg.PingInterval)
	s.log("exports enabled")
	s.publishLinkState(reason, "")
}

// teardownImportedRetained clears every local retained slot we populated
// from a wire import. Mirrors rpc_bridge.lua's invalidate_imported_retained.
func (s *session) teardownImportedRetained() {
	for _, t := range s.importedRetained {
		s.conn.Publish(s.conn.NewMessage(t, nil, true))
	}
	s.importedRetained = nil
}

func (s *session) trackImportedRetain(t bus.Topic) {
	for _, ex := range s.importedRetained {
		if topicEquals(ex, t) {
			return
		}
	}
	s.importedRetained = append(s.importedRetained, t)
}

func (s *session) untrackImportedRetain(t bus.Topic) {
	for i, ex := range s.importedRetained {
		if topicEquals(ex, t) {
			s.importedRetained = append(s.importedRetained[:i], s.importedRetained[i+1:]...)
			return
		}
	}
}

// tickReady promotes rpcReady once the post-handshake export holdoff has
// elapsed and the initial exact critical retained replay has been drained.
// Re-publishes link state so consumers observe the ready edge.
func (s *session) tickReady(now time.Time) {
	if s.link != linkUp || s.rpcReady {
		return
	}
	if s.exportReadyAt.IsZero() || now.Before(s.exportReadyAt) {
		return
	}
	if !s.criticalExportReplayDrained() {
		return
	}
	s.rpcReady = true
	s.publishLinkState("", "")
}

// ---- dispatch ----

func (s *session) dispatch(line []byte) {
	t := protoType(line)
	if t == "" {
		s.logMalformed(line, nil)
		return
	}

	switch t {
	case msgHello:
		typedDispatch(s, t, line, s.onHello)
		return
	case msgHelloAck:
		typedDispatch(s, t, line, s.onHelloAck)
		return
	}

	if !s.requireLinkUp(t) {
		return
	}

	switch t {
	case msgPing:
		typedDispatch(s, t, line, s.onPing)
	case msgPong:
		typedDispatch(s, t, line, s.onPong)
	case msgPub:
		typedDispatch(s, t, line, s.onPub)
	case msgUnretain:
		typedDispatch(s, t, line, s.onUnretain)
	case msgCall:
		typedDispatch(s, t, line, s.onCall)
	case msgReply:
		typedDispatch(s, t, line, s.onReply)
	case msgXferBegin:
		typedDispatch(s, t, line, s.onTransferBegin)
	case msgXferChunk:
		typedDispatch(s, t, line, s.onTransferChunk)
	case msgXferCommit:
		typedDispatch(s, t, line, s.onTransferCommit)
	case msgXferAbort:
		typedDispatch(s, t, line, s.onTransferAbort)
	case msgXferReady, msgXferNeed, msgXferDone:
		s.logKV("echoed transfer control ignored", "type", t)
	default:
		s.logKV("unknown message type dropped", "type", t)
	}
}

func typedDispatch[T any](s *session, msgType string, line []byte, handler func(*T)) {
	var msg T
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&msg); err != nil {
		s.logMalformed(line, err)
		s.retryMalformedTransferFrame(msgType, line)
		return
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("trailing_json")
		}
		s.logMalformed(line, err)
		s.retryMalformedTransferFrame(msgType, line)
		return
	}
	handler(&msg)
}

func (s *session) retryMalformedTransferFrame(msgType string, line []byte) {
	if msgType != msgXferChunk {
		return
	}
	cur := s.incomingTransfer
	if cur == nil {
		return
	}
	id := protoXferID(line)
	if id == "" {
		s.logKV("malformed xfer_chunk dropped", "why", "missing_xfer_id")
		return
	}
	if id != cur.meta.ID {
		s.logKV("malformed xfer_chunk dropped", "id", id)
		return
	}
	s.retryCorruptTransferFrame("bad_message")
}

func (s *session) requireLinkUp(t string) bool {
	if s.link != linkUp {
		s.logKV("dropped before handshake", "type", t)
		return false
	}
	return true
}

func (s *session) logMalformed(line []byte, err error) {
	errStr := ""
	if err != nil {
		errStr = err.Error()
	}
	if fabricTraceEnabled {
		println(
			"[fabric]", "sid", s.localSID,
			"malformed frame dropped",
			"line_len", strconvx.Itoa(len(line)),
			"line_xxhash32", traceHash(line),
			"line_head", tracePreview(line),
			"line_tail", traceTail(line),
			"err", errStr,
		)
	}

	// Transfer retry signaling is handled by typedDispatch only for
	// malformed active xfer_chunk frames. Other malformed frames are logged
	// and dropped without consuming the transfer corruption budget.
}

// notePeerIdentity records the remote peer's node, SID, and protocol name.
// If the SID changes mid-session, the returned reason triggers a full
// teardown of exports and pending calls on the Go side. Note: the Lua
// side only tears down pending calls on SID change, not exports — this
// asymmetry is intentional since the CM5 re-subscribes on reconnect.
func (s *session) notePeerIdentity(node, sid string, proto string) string {
	reason := ""
	if s.link == linkUp && s.peerSID != "" && sid != "" && s.peerSID != sid {
		reason = reasonSessionReset
	}
	if node != "" {
		s.peerNode = node
	}
	if sid != "" {
		s.peerSID = sid
	}
	if proto != "" {
		s.peerProto = proto
	}
	return reason
}

func (s *session) isSelfControlFrame(node, sid string) bool {
	if sid != "" && sid == s.localSID {
		return true
	}
	if node != "" && node == s.nodeID {
		return true
	}
	return false
}

func hasWirePrefix(topic, prefix []string) bool {
	if len(topic) < len(prefix) {
		return false
	}
	for i := range prefix {
		if topic[i] != prefix[i] {
			return false
		}
	}
	return true
}

func wireTopicEquals(topic, want []string) bool {
	if len(topic) != len(want) {
		return false
	}
	for i := range want {
		if topic[i] != want[i] {
			return false
		}
	}
	return true
}

func wireTopicString(topic []string) string {
	if len(topic) == 0 {
		return ""
	}
	return strings.Join(topic, "/")
}

func validWireTopic(topic []string) bool {
	if len(topic) == 0 {
		return false
	}
	for _, part := range topic {
		if part == "" {
			return false
		}
	}
	return true
}

func (s *session) extendTransferQuiet(reason string, d time.Duration) {
	now := time.Now()
	until := now.Add(d)
	if until.After(s.transferQuietUntil) {
		s.transferQuietUntil = until
		s.transferQuietReason = reason
	}
}

func (s *session) transferQuiet(now time.Time) (bool, string) {
	if cur := s.incomingTransfer; cur != nil {
		return true, "incoming_transfer:" + cur.meta.ID
	}
	if !s.transferQuietUntil.IsZero() && now.Before(s.transferQuietUntil) {
		reason := s.transferQuietReason
		if reason == "" {
			reason = "quiet_window"
		}
		return true, reason
	}
	return false, ""
}

func quietAllowsCriticalExports(reason string) bool {
	switch reason {
	case "xfer_commit_target", "xfer_target_rejected", "xfer_done":
		return true
	default:
		return false
	}
}

func (s *session) onHello(msg *protoHello) {
	if msg.Proto != protocolName {
		s.log("hello dropped: unsupported proto")
		return
	}
	if msg.SID == "" || msg.Node == "" {
		s.log("hello dropped: missing identity")
		return
	}
	if s.peerID != "" && msg.Node != s.peerID {
		s.log("hello dropped: wrong node")
		return
	}
	s.markRx()
	reason := s.notePeerIdentity(msg.Node, msg.SID, msg.Proto)
	s.logKV("hello rx", "peer_sid", msg.SID)

	if !s.sendControl(marshal(protoHelloAck{
		Type:  msgHelloAck,
		Proto: protocolName,
		SID:   s.localSID,
		Node:  s.nodeID,
	})) {
		return
	}
	s.log("hello_ack tx")
	if s.link != linkUp || reason != "" {
		s.promoteLink(reason)
	}
}

func (s *session) onHelloAck(msg *protoHelloAck) {
	if s.isSelfControlFrame(msg.Node, msg.SID) {
		s.log("echoed hello_ack ignored")
		return
	}
	if msg.Proto != protocolName {
		s.log("hello_ack dropped: unsupported proto")
		return
	}
	if msg.SID == "" || msg.Node == "" {
		s.log("hello_ack dropped: missing identity")
		return
	}
	if s.peerID != "" && msg.Node != s.peerID {
		s.log("hello_ack dropped: wrong node")
		return
	}
	s.markRx()
	reason := s.notePeerIdentity(msg.Node, msg.SID, msg.Proto)
	s.logKV("hello_ack rx", "peer_sid", msg.SID)
	if s.link != linkUp || reason != "" {
		s.promoteLink(reason)
	}
}

func (s *session) onPing(msg *protoPing) {
	if s.link != linkUp || msg.SID != s.peerSID {
		return
	}
	if s.isSelfControlFrame("", msg.SID) {
		s.log("echoed ping ignored")
		return
	}
	s.markRx()
	s.logKV("ping rx", "peer_sid", msg.SID)
	if !s.sendControl(marshal(protoPong{Type: msgPong, SID: s.localSID})) {
		return
	}
	s.log("pong tx")
}

// tickPing sends an outbound ping if the link is established and the
// PingInterval cadence has elapsed. Mirrors session_ctl.lua: pings fire
// unconditionally every ping_interval after each send (NOT TX-activity-based).
func (s *session) tickPing(now time.Time) {
	if s.link != linkUp {
		return
	}
	if quiet, _ := s.transferQuiet(now); quiet {
		// Keep the UART quiet while CM5 is preparing or streaming a firmware
		// image; chunk recovery depends on xfer_need being the only periodic
		// MCU-originated frame on the fabric link.
		s.nextPingAt = now.Add(s.cfg.PingInterval)
		return
	}
	if s.nextPingAt.IsZero() || now.Before(s.nextPingAt) {
		return
	}
	if !s.sendControl(marshal(protoPing{Type: msgPing, SID: s.localSID})) {
		return
	}
	s.nextPingAt = now.Add(s.cfg.PingInterval)
}

func (s *session) onPong(msg *protoPong) {
	if s.link != linkUp || msg.SID != s.peerSID {
		return
	}
	if s.isSelfControlFrame("", msg.SID) {
		s.log("echoed pong ignored")
		return
	}
	s.markRx()
	s.lastPongAt = s.lastRxAt
}

func (s *session) onPub(msg *protoPub) {
	if !validWireTopic(msg.Topic) {
		s.log("incoming pub dropped: bad_topic")
		return
	}
	localTopic := importPublishTopic(msg.Topic)
	if localTopic == nil {
		if hasWirePrefix(msg.Topic, []string{"state"}) {
			s.log("echoed state pub ignored")
			return
		}
		s.log("incoming pub dropped: no_route")
		return
	}

	s.markRx()
	s.conn.Publish(s.conn.NewMessage(localTopic, msg.Payload, msg.Retain))
	if msg.Retain {
		s.trackImportedRetain(localTopic)
	}
	// A non-retained pub on the same topic must NOT untrack: the bus
	// retain store is only cleared by an explicit unretain (or a
	// retained-nil publish), so the prior retained value is still live
	// and must be cleaned up on session reset. Mirrors rpc_bridge.lua,
	// which only mutates imported_retained on retain set/clear.
}

func (s *session) onUnretain(msg *protoUnretain) {
	if !validWireTopic(msg.Topic) {
		s.log("incoming unretain dropped: bad_topic")
		return
	}
	localTopic := importPublishTopic(msg.Topic)
	if localTopic == nil {
		s.log("incoming unretain dropped: no_route")
		return
	}
	s.markRx()
	s.conn.Publish(s.conn.NewMessage(localTopic, nil, true))
	s.untrackImportedRetain(localTopic)
}

func (s *session) onCall(msg *protoCall) {
	if msg.ID == "" {
		s.log("incoming call dropped: missing_id")
		return
	}
	if !validWireTopic(msg.Topic) {
		s.log("incoming call dropped: bad_topic")
		s.sendRPC(marshal(protoReply{Type: msgReply, Corr: msg.ID, OK: false, Err: "bad_topic"}))
		return
	}
	for _, call := range s.inboundCalls {
		if call.id == msg.ID {
			s.logKV("incoming call dropped", "err", "duplicate_call_id")
			s.sendRPC(marshal(protoReply{Type: msgReply, Corr: msg.ID, OK: false, Err: "duplicate_call_id"}))
			return
		}
	}
	if len(s.inboundCalls) >= s.cfg.MaxInboundHelpers {
		s.log("incoming call dropped: busy")
		s.sendRPC(marshal(protoReply{Type: msgReply, Corr: msg.ID, OK: false, Err: reasonBusy}))
		return
	}

	localTopic := importCallTopic(msg.Topic)
	if localTopic == nil {
		s.log("incoming call dropped: no_route")
		s.sendRPC(marshal(protoReply{Type: msgReply, Corr: msg.ID, OK: false, Err: reasonNoRoute}))
		return
	}

	s.markRx()
	isTransferPrepare := wireTopicEquals(msg.Topic, wireUpdaterPrepare)
	if isTransferPrepare {
		s.extendTransferQuiet("prepare_call_rx", transferPrepareQuiet)
	}

	timeout := callTimeoutDef
	if msg.TimeoutMs > 0 {
		timeout = time.Duration(msg.TimeoutMs) * time.Millisecond
	}
	busMsg := s.conn.NewMessage(localTopic, msg.Payload, false)
	sub := s.conn.Request(busMsg)
	topicCopy := append([]string(nil), msg.Topic...)
	s.inboundCalls = append(s.inboundCalls, &inboundCall{
		id:              msg.ID,
		topic:           topicCopy,
		sub:             sub,
		deadline:        time.Now().Add(timeout),
		transferPrepare: isTransferPrepare,
	})
}

func (s *session) onReply(msg *protoReply) {
	if msg.Corr == "" {
		s.log("reply dropped: missing_id")
		return
	}
	for i, call := range s.outboundCalls {
		if call.id != msg.Corr {
			continue
		}
		s.markRx()
		s.outboundCalls = append(s.outboundCalls[:i], s.outboundCalls[i+1:]...)
		if !call.req.CanReply() {
			return
		}
		if !msg.OK {
			s.conn.Reply(call.req, types.ErrorReply{OK: false, Error: msg.Err}, false)
			return
		}
		s.conn.Reply(call.req, decodePayload(msg.Payload), false)
		return
	}

	s.logKV("unexpected reply dropped", "corr", msg.Corr)
}

func checkBusError(payload any) string {
	if e, ok := payload.(types.ErrorReply); ok && !e.OK && e.Error != "" {
		return e.Error
	}
	// Fall back to JSON probe for handlers that reply with ad-hoc structs.
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	var probe struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &probe) == nil && !probe.OK && probe.Error != "" {
		return probe.Error
	}
	return ""
}

func topicEquals(t bus.Topic, expected bus.Topic) bool {
	if t.Len() != expected.Len() {
		return false
	}
	for i := 0; i < t.Len(); i++ {
		a, _ := t.At(i).(string)
		b, _ := expected.At(i).(string)
		if a != b {
			return false
		}
	}
	return true
}

func marshalPayload(payload any) (json.RawMessage, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// ---- export lifecycle ----
//
// Exports are drained inline in the main loop (no extra goroutines)
// to avoid TinyGo cooperative scheduler mutex panics.

func (s *session) setupExports() {
	if s.conn == nil {
		return
	}
	for _, p := range criticalExportTopics {
		s.criticalExportSubs = append(s.criticalExportSubs, s.conn.Subscribe(p))
		s.criticalExportReplayPending = append(s.criticalExportReplayPending, true)
	}
	for _, p := range exportPatterns() {
		s.exportSubs = append(s.exportSubs, s.conn.Subscribe(p))
	}
	for _, p := range exportCallPatterns() {
		s.exportCallSubs = append(s.exportCallSubs, s.conn.Subscribe(p))
	}
}

func (s *session) teardownExports() {
	for _, sub := range s.criticalExportSubs {
		s.conn.Unsubscribe(sub)
	}
	s.criticalExportSubs = nil
	s.criticalExportReplayPending = nil
	for _, sub := range s.exportSubs {
		s.conn.Unsubscribe(sub)
	}
	s.exportSubs = nil
	for _, sub := range s.exportCallSubs {
		s.conn.Unsubscribe(sub)
	}
	s.exportCallSubs = nil
}

func (s *session) teardownInbound() {
	for _, call := range s.inboundCalls {
		if call.sub != nil {
			s.conn.Unsubscribe(call.sub)
			call.sub = nil
		}
	}
	s.inboundCalls = nil
}

func (s *session) teardownOutbound(reason string) {
	for _, call := range s.outboundCalls {
		if call.req != nil && call.req.CanReply() {
			s.conn.Reply(call.req, types.ErrorReply{OK: false, Error: reason}, false)
		}
	}
	s.outboundCalls = nil
}

func (s *session) sendExportMessage(m *bus.Message) (bool, bool) {
	if m == nil {
		return false, true
	}
	wire := exportTopic(m.Topic)
	if wire == nil {
		return false, true
	}
	if m.Retained && m.Payload == nil {
		if !s.sendRPC(marshal(protoUnretain{
			Type:  msgUnretain,
			Topic: wire,
		})) {
			return false, false
		}
		return true, true
	}
	payload, err := marshalPayload(m.Payload)
	if err != nil {
		s.logKV("export payload dropped", "err", err.Error())
		return false, true
	}
	if fabricTraceEnabled {
		println(
			"[fabric]", "sid", s.localSID,
			"export pub tx",
			"topic", wireTopicString(wire),
			"retain", m.Retained,
			"payload_len", strconvx.Itoa(len(payload)),
		)
	}
	if !s.sendRPC(marshal(protoPub{
		Type:    msgPub,
		Topic:   wire,
		Payload: payload,
		Retain:  m.Retained,
	})) {
		return false, false
	}
	return true, true
}

func latestSubscriptionMessage(sub *bus.Subscription) *bus.Message {
	var latest *bus.Message
	for {
		select {
		case m, ok := <-sub.Channel():
			if !ok {
				return latest
			}
			if m != nil {
				latest = m
			}
		default:
			return latest
		}
	}
}

func (s *session) criticalExportReplayDrained() bool {
	for _, pending := range s.criticalExportReplayPending {
		if pending {
			return false
		}
	}
	return true
}

func (s *session) drainCriticalExports(total *int) bool {
	for i, sub := range s.criticalExportSubs {
		if *total >= exportMaxPerTick {
			return true
		}
		m := latestSubscriptionMessage(sub)
		if m == nil {
			if i < len(s.criticalExportReplayPending) && s.criticalExportReplayPending[i] {
				return true
			}
			continue
		}
		sent, ok := s.sendExportMessage(m)
		if !ok {
			return false
		}
		if sent && i < len(s.criticalExportReplayPending) && s.criticalExportReplayPending[i] {
			s.criticalExportReplayPending[i] = false
		}
		if sent {
			(*total)++
		}
	}
	return true
}

// drainExports does a non-blocking read of each export subscription
// and writes any messages to the wire. Called from the main loop.
func (s *session) drainExports() {
	if s.link != linkUp {
		return
	}
	now := time.Now()
	quiet, quietReason := s.transferQuiet(now)
	if !s.exportsEnabled {
		return
	}
	if !s.exportReadyAt.IsZero() && now.Before(s.exportReadyAt) {
		return
	}
	if quiet && !quietAllowsCriticalExports(quietReason) {
		// Avoid colliding telemetry/state exports with prepare/xfer traffic on
		// echo-prone links. Post-transfer quiet allows critical facts below so
		// state=rebooting can reach CM5 before the reboot arm.
		return
	}
	total := 0
	if !s.drainCriticalExports(&total) {
		return
	}
	if quiet {
		return
	}
	if !s.criticalExportReplayDrained() {
		return
	}
	for _, sub := range s.exportSubs {
		for {
			if total >= exportMaxPerTick {
				return
			}
			select {
			case m, ok := <-sub.Channel():
				if !ok || m == nil {
					goto nextSub
				}
				if len(s.criticalExportSubs) > 0 && isCriticalExportTopic(m.Topic) {
					continue
				}
				sent, ok := s.sendExportMessage(m)
				if !ok {
					return
				}
				if sent {
					total++
				}
			default:
				goto nextSub
			}
		}
	nextSub:
	}
}

func (s *session) drainInbound(now time.Time) {
	if len(s.inboundCalls) == 0 {
		return
	}

	keep := s.inboundCalls[:0]
	for _, call := range s.inboundCalls {
		select {
		case reply, ok := <-call.sub.Channel():
			s.conn.Unsubscribe(call.sub)
			call.sub = nil // prevent double-unsubscribe in teardownInbound
			if !ok || reply == nil {
				if call.transferPrepare {
					s.extendTransferQuiet("prepare_reply_timeout", transferPrepareQuiet)
				}
				if !s.sendRPC(marshal(protoReply{Type: msgReply, Corr: call.id, OK: false, Err: reasonTimeout})) {
					return
				}
				continue
			}
			if errStr := checkBusError(reply.Payload); errStr != "" {
				if call.transferPrepare {
					s.extendTransferQuiet("prepare_reply_error", transferPrepareQuiet)
				}
				if !s.sendRPC(marshal(protoReply{Type: msgReply, Corr: call.id, OK: false, Err: errStr})) {
					return
				}
				continue
			}
			payload, err := marshalPayload(reply.Payload)
			if err != nil {
				if call.transferPrepare {
					s.extendTransferQuiet("prepare_reply_marshal_failed", transferPrepareQuiet)
				}
				if !s.sendRPC(marshal(protoReply{Type: msgReply, Corr: call.id, OK: false, Err: errPayloadMarshal})) {
					return
				}
				continue
			}
			if call.transferPrepare {
				s.extendTransferQuiet("prepare_reply_ok", transferPrepareQuiet)
			}
			if !s.sendRPC(marshal(protoReply{Type: msgReply, Corr: call.id, OK: true, Payload: payload})) {
				return
			}
			continue
		default:
		}

		if !now.Before(call.deadline) {
			s.conn.Unsubscribe(call.sub)
			call.sub = nil
			if call.transferPrepare {
				s.extendTransferQuiet("prepare_call_timeout", transferPrepareQuiet)
			}
			if !s.sendRPC(marshal(protoReply{Type: msgReply, Corr: call.id, OK: false, Err: reasonTimeout})) {
				return
			}
			continue
		}

		keep = append(keep, call)
	}

	s.inboundCalls = keep
}

func (s *session) drainOutbound(now time.Time) {
	// Forward new outgoing calls from the local bus onto the wire.
	if s.link == linkUp && len(s.exportCallSubs) > 0 {
		for _, sub := range s.exportCallSubs {
			for {
				select {
				case msg, ok := <-sub.Channel():
					if !ok || msg == nil {
						goto nextSub
					}

					wireTopic := exportCallTopic(msg.Topic)
					if wireTopic == nil {
						continue
					}

					payload, err := marshalPayload(msg.Payload)
					if err != nil {
						s.logKV("outgoing call dropped", "err", err.Error())
						if msg.CanReply() {
							s.conn.Reply(msg, types.ErrorReply{OK: false, Error: errPayloadMarshal}, false)
						}
						continue
					}
					id := s.nextOutboundID
					s.nextOutboundID++
					corr := "wire-" + strconvx.Utoa64(id)
					if msg.CanReply() {
						s.outboundCalls = append(s.outboundCalls, &outboundCall{
							id:       corr,
							req:      msg,
							deadline: now.Add(callTimeoutDef),
						})
					}
					if !s.sendRPC(marshal(protoCall{
						Type:      msgCall,
						ID:        corr,
						Topic:     wireTopic,
						Payload:   payload,
						TimeoutMs: int(callTimeoutDef / time.Millisecond),
					})) {
						return
					}
				default:
					goto nextSub
				}
			}
		nextSub:
		}
	}

	// Expire outbound calls that have timed out waiting for a remote reply.
	if len(s.outboundCalls) > 0 {
		keep := s.outboundCalls[:0]
		for _, call := range s.outboundCalls {
			if !now.Before(call.deadline) {
				if call.req != nil && call.req.CanReply() {
					s.conn.Reply(call.req, types.ErrorReply{OK: false, Error: reasonTimeout}, false)
				}
				continue
			}
			keep = append(keep, call)
		}
		s.outboundCalls = keep
	}
}

// ---- transport write ----

// sendControl, sendRPC, sendBulk are the lane-tagged enqueue entry
// points used at every send site. They wrap enqueueFrame (defined in
// writer.go) so the lane intent is explicit at the call site.
//
// Lane assignment per protocol.lua's FRAME_CLASS:
//
//	control: hello, hello_ack, ping, pong, xfer_{begin,ready,need,commit,done,abort}
//	rpc:     pub, unretain, call, reply
//	bulk:    xfer_chunk (MCU does not originate; bulk lane unused on MCU)
func (s *session) sendControl(data []byte) bool { return s.enqueueFrame(laneControl, data) }
func (s *session) sendRPC(data []byte) bool     { return s.enqueueFrame(laneRPC, data) }

func (s *session) logWaiting() {
	if s.peerSID != "" {
		return
	}
	s.log("waiting for connection start")
}
