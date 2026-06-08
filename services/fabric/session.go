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
	"devicecode-go/services/otadiag"
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
	exportMaxPerTick  = 1
	errPayloadMarshal = "payload_marshal_failed"
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
	id         string
	topic      []string
	localTopic bus.Topic
	payload    json.RawMessage
	sub        *bus.Subscription
	deadline   time.Time
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
	exportDrainAt  time.Time
	exportsEnabled bool

	criticalExportSubs          []*bus.Subscription
	criticalExportReplayPending []bool
	criticalExportPendingMsgs   []*bus.Message
	exportPendingMsgs           []*bus.Message
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
	pendingTargetCall           *pendingTargetCall
	beginTransfer               func(transferMeta) (transferSink, error)
	busSubs                     *bus.SubscriptionSet
	ctx                         context.Context
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
	s.ctx = ctx
	s.busSubs = s.conn.NewSubscriptionSet()
	lines := make(chan readResult, lineQueueSize)

	go func() {
		defer close(lines)
		lastLineAt := time.Now()
		for {
			started := time.Now()
			line, err := s.tr.ReadLine()
			now := time.Now()
			readDur := now.Sub(started)
			sinceLine := now.Sub(lastLineAt)
			if err != nil {
				if errors.Is(err, ErrLineTooLong) {
					otadiag.Event(
						"[fabric-rx]", "read_error", otadiag.XferNone,
						otadiag.KV("reason", "line_too_long"),
						otadiag.KV("read_ms", int(readDur/time.Millisecond)),
						otadiag.KV("since_line_ms", int(sinceLine/time.Millisecond)),
					)
					s.log("oversized line dropped")
					continue
				}
				otadiag.Event(
					"[fabric-rx]", "read_error", otadiag.XferNone,
					otadiag.KV("reason", err.Error()),
					otadiag.KV("read_ms", int(readDur/time.Millisecond)),
					otadiag.KV("since_line_ms", int(sinceLine/time.Millisecond)),
				)
				select {
				case lines <- readResult{err: err}:
				case <-ctx.Done():
				}
				return
			}
			t := protoType(line)
			if shouldLogFabricRead(t, readDur, sinceLine) {
				otadiag.Event(
					"[fabric-rx]", "read_line", protoXferID(line),
					otadiag.KV("type", t),
					otadiag.KV("line_len", len(line)),
					otadiag.KV("read_ms", int(readDur/time.Millisecond)),
					otadiag.KV("since_line_ms", int(sinceLine/time.Millisecond)),
				)
			}
			lastLineAt = now
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
	defer func() {
		if s.busSubs != nil {
			s.busSubs.Close()
		}
	}()
	defer s.teardownExports()
	defer s.teardownInbound()
	defer s.teardownOutbound(reasonLinkDown)
	defer s.cancelTargetCall(reasonLinkDown)
	defer s.abortTransfer(reasonLinkDown)
	defer s.log("run stop")

	stale := time.NewTimer(s.cfg.LivenessTimeout)
	defer stale.Stop()

	waitTick := time.NewTicker(waitLogEvery)
	defer waitTick.Stop()

	// Bus subscriptions and pending transfer/updater operations wake the
	// reactor directly. Timers below cover deadlines and periodic liveness only.

	pendingDeadline := time.NewTimer(time.Hour)
	if !pendingDeadline.Stop() {
		<-pendingDeadline.C
	}
	defer pendingDeadline.Stop()

	s.publishLinkState("", "")
	s.log("run start")

	for {
		pendingAt, pendingOK := s.nextPendingDeadline(time.Now())
		pendingDeadlineCh := resetOptionalTimer(pendingDeadline, pendingAt, pendingOK)
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

		case res := <-s.pendingChunkReady():
			s.finishChunkWrite(time.Now(), res)

		case res := <-s.pendingCommitReady():
			s.finishTransferCommit(time.Now(), res)

		case rep, ok := <-s.pendingTargetReady():
			s.finishTargetReply(rep, ok)

		case <-pendingDeadlineCh:
			s.handlePendingDeadline(time.Now())

		case _, ok := <-s.busReady():
			if !ok {
				return
			}
			s.drainBusEvents(time.Now())

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

func shouldLogFabricRead(msgType string, _, _ time.Duration) bool {
	switch msgType {
	case msgHello, msgHelloAck, msgCall, msgReply, msgXferBegin, msgXferCommit, msgXferAbort:
		return true
	}
	return false
}

func resetOptionalTimer(t *time.Timer, deadline time.Time, ok bool) <-chan time.Time {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	if !ok || deadline.IsZero() {
		return nil
	}
	d := time.Until(deadline)
	if d < 0 {
		d = 0
	}
	t.Reset(d)
	return t.C
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
	s.exportDrainAt = time.Time{}
	s.exportsEnabled = false
	s.rpcReady = false
	s.teardownExports()
	s.teardownInbound()
	s.teardownOutbound(pendingReason)
	s.teardownImportedRetained()
	s.cancelTargetCall(pendingReason)
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
		s.cancelTargetCall(reason)
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
	s.exportDrainAt = time.Time{}
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
		s.scheduleExportDrain(now)
		return
	}
	s.rpcReady = true
	s.publishLinkState("", "")
	s.drainQueuedExports()
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
		otadiag.Event(
			"[fabric-xfer]", "begin_route_start", protoXferID(line),
			otadiag.KV("line_len", len(line)),
		)
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
		if msgType == msgXferBegin {
			otadiag.Event(
				"[fabric-xfer]", "begin_decode_error", protoXferID(line),
				otadiag.KV("err", err.Error()),
				otadiag.KV("line_len", len(line)),
			)
			otadiag.StopUpdateWindow("begin_decode_error")
		}
		s.logMalformed(line, err)
		s.retryMalformedTransferFrame(msgType, line)
		return
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("trailing_json")
		}
		if msgType == msgXferBegin {
			otadiag.Event(
				"[fabric-xfer]", "begin_decode_error", protoXferID(line),
				otadiag.KV("err", err.Error()),
				otadiag.KV("line_len", len(line)),
			)
			otadiag.StopUpdateWindow("begin_decode_error")
		}
		s.logMalformed(line, err)
		s.retryMalformedTransferFrame(msgType, line)
		return
	}
	handler(&msg)
	if msgType == msgXferBegin {
		otadiag.Event("[fabric-xfer]", "begin_route_done", protoXferID(line))
	}
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

func busTopicPath(topic bus.Topic) string {
	if topic == nil {
		return ""
	}
	parts := make([]string, 0, topic.Len())
	for i := 0; i < topic.Len(); i++ {
		if s, ok := topic.At(i).(string); ok {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "/")
}

func traceRPCDiagTopic(topic []string) bool {
	return wireTopicEquals(topic, wireUpdaterPrepare) || wireTopicEquals(topic, wireUpdaterCommit)
}

func rawJSONScalar(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return strconvx.FormatFloat(n, 'f', -1, 64)
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		if b {
			return "true"
		}
		return "false"
	}
	return ""
}

func rpcPayloadField(payload json.RawMessage, key string) string {
	if len(payload) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return ""
	}
	return rawJSONScalar(obj[key])
}

func (s *session) rpcDiag(event string, msg *protoCall, localTopic bus.Topic, reason string, fields ...otadiag.Field) {
	if msg == nil || !traceRPCDiagTopic(msg.Topic) {
		return
	}
	base := []otadiag.Field{
		otadiag.KV("call_id", msg.ID),
		otadiag.KV("topic", wireTopicString(msg.Topic)),
		otadiag.KV("local_sid", s.localSID),
		otadiag.KV("peer_sid", s.peerSID),
		otadiag.KV("payload_len", len(msg.Payload)),
	}
	if local := busTopicPath(localTopic); local != "" {
		base = append(base, otadiag.KV("local_topic", local))
	}
	if jobID := rpcPayloadField(msg.Payload, "job_id"); jobID != "" {
		base = append(base, otadiag.KV("job_id", jobID))
	}
	if imageID := rpcPayloadField(msg.Payload, "expected_image_id"); imageID != "" {
		base = append(base, otadiag.KV("expected_image_id", imageID))
	}
	if reason != "" {
		base = append(base, otadiag.KV("reason", reason))
	}
	base = append(base, fields...)
	otadiag.Event("[fabric-rpc]", event, otadiag.XferNone, base...)
}

func (s *session) rpcDiagInbound(event string, call *inboundCall, ok bool, err string, fields ...otadiag.Field) {
	if call == nil || !traceRPCDiagTopic(call.topic) {
		return
	}
	base := []otadiag.Field{
		otadiag.KV("call_id", call.id),
		otadiag.KV("topic", wireTopicString(call.topic)),
		otadiag.KV("local_sid", s.localSID),
		otadiag.KV("peer_sid", s.peerSID),
		otadiag.KV("payload_len", len(call.payload)),
		otadiag.KV("ok", ok),
	}
	if local := busTopicPath(call.localTopic); local != "" {
		base = append(base, otadiag.KV("local_topic", local))
	}
	if jobID := rpcPayloadField(call.payload, "job_id"); jobID != "" {
		base = append(base, otadiag.KV("job_id", jobID))
	}
	if imageID := rpcPayloadField(call.payload, "expected_image_id"); imageID != "" {
		base = append(base, otadiag.KV("expected_image_id", imageID))
	}
	if err != "" {
		base = append(base, otadiag.KV("err", err))
	}
	base = append(base, fields...)
	otadiag.Event("[fabric-rpc]", event, otadiag.XferNone, base...)
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
		s.rpcDiag("call_reject", msg, nil, "missing_id")
		s.log("incoming call dropped: missing_id")
		return
	}
	if !validWireTopic(msg.Topic) {
		s.rpcDiag("call_reject", msg, nil, "bad_topic")
		s.log("incoming call dropped: bad_topic")
		s.sendRPC(marshal(protoReply{Type: msgReply, Corr: msg.ID, OK: false, Err: "bad_topic"}))
		return
	}
	s.rpcDiag("call_rx", msg, nil, "",
		otadiag.KV("timeout_ms", strconvx.Itoa(msg.TimeoutMs)),
	)
	for _, call := range s.inboundCalls {
		if call.id == msg.ID {
			s.rpcDiag("call_reject", msg, nil, "duplicate_call_id")
			s.logKV("incoming call dropped", "err", "duplicate_call_id")
			s.sendRPC(marshal(protoReply{Type: msgReply, Corr: msg.ID, OK: false, Err: "duplicate_call_id"}))
			return
		}
	}
	if len(s.inboundCalls) >= s.cfg.MaxInboundHelpers {
		s.rpcDiag("call_reject", msg, nil, reasonBusy)
		s.log("incoming call dropped: busy")
		s.sendRPC(marshal(protoReply{Type: msgReply, Corr: msg.ID, OK: false, Err: reasonBusy}))
		return
	}

	localTopic := importCallTopic(msg.Topic)
	if localTopic == nil {
		s.rpcDiag("call_reject", msg, nil, reasonNoRoute)
		s.log("incoming call dropped: no_route")
		s.sendRPC(marshal(protoReply{Type: msgReply, Corr: msg.ID, OK: false, Err: reasonNoRoute}))
		return
	}
	s.rpcDiag("call_route_ok", msg, localTopic, "")

	s.markRx()
	timeout := callTimeoutDef
	if msg.TimeoutMs > 0 {
		timeout = time.Duration(msg.TimeoutMs) * time.Millisecond
	}
	busMsg := s.conn.NewMessage(localTopic, msg.Payload, false)
	s.rpcDiag("call_dispatch_start", msg, localTopic, "",
		otadiag.KV("timeout_ms", strconvx.Itoa(int(timeout/time.Millisecond))),
	)
	sub := s.requestBus(busMsg)
	topicCopy := append([]string(nil), msg.Topic...)
	call := &inboundCall{
		id:         msg.ID,
		topic:      topicCopy,
		localTopic: localTopic,
		payload:    append(json.RawMessage(nil), msg.Payload...),
		sub:        sub,
		deadline:   time.Now().Add(timeout),
	}
	s.inboundCalls = append(s.inboundCalls, call)
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

func (s *session) busReady() <-chan struct{} {
	if s.busSubs == nil {
		return nil
	}
	return s.busSubs.Ready()
}

func (s *session) subscribeBus(tp bus.Topic) *bus.Subscription {
	if s.busSubs == nil {
		return s.conn.Subscribe(tp)
	}
	return s.busSubs.Subscribe(tp)
}

func (s *session) requestBus(msg *bus.Message) *bus.Subscription {
	if s.busSubs == nil {
		return s.conn.Request(msg)
	}
	return s.busSubs.Request(msg)
}

func (s *session) drainBusEvents(now time.Time) {
	// A single bus readiness edge may cover several subscriptions.  Drain each
	// class without blocking; export drains retain their quota and will be
	// resumed by the export-ready timer when work remains.
	s.drainExports()
	s.drainOutboundMessages(now)
	s.drainInbound(now)
	s.tickReady(now)
	s.drainQueuedExports()
}

func (s *session) setupExports() {
	if s.conn == nil {
		return
	}
	for _, p := range criticalExportTopics {
		sub := s.subscribeBus(p)
		s.criticalExportSubs = append(s.criticalExportSubs, sub)
		s.criticalExportReplayPending = append(s.criticalExportReplayPending, true)
		s.criticalExportPendingMsgs = append(s.criticalExportPendingMsgs, nil)
	}
	for _, p := range exportPatterns() {
		sub := s.subscribeBus(p)
		s.exportSubs = append(s.exportSubs, sub)
	}
	for _, p := range exportCallPatterns() {
		sub := s.subscribeBus(p)
		s.exportCallSubs = append(s.exportCallSubs, sub)
	}
}

func (s *session) teardownExports() {
	for _, sub := range s.criticalExportSubs {
		s.conn.Unsubscribe(sub)
	}
	s.criticalExportSubs = nil
	s.criticalExportReplayPending = nil
	s.criticalExportPendingMsgs = nil
	s.exportPendingMsgs = nil
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

func (s *session) exportCanSend(now time.Time) bool {
	return s.link == linkUp && s.exportsEnabled && (s.exportReadyAt.IsZero() || !now.Before(s.exportReadyAt))
}

func (s *session) queueCriticalExport(idx int, m *bus.Message) {
	if idx < 0 || idx >= len(s.criticalExportPendingMsgs) {
		return
	}
	s.criticalExportPendingMsgs[idx] = m
}

func (s *session) queueExport(m *bus.Message) {
	if m == nil {
		return
	}
	// Keep the queue bounded. The bus subscription itself coalesces retained
	// changes, but once a watcher has handed the event to the session we still
	// avoid unbounded growth during handshake holdoff.
	const maxPendingExports = 32
	if len(s.exportPendingMsgs) >= maxPendingExports {
		copy(s.exportPendingMsgs, s.exportPendingMsgs[1:])
		s.exportPendingMsgs[len(s.exportPendingMsgs)-1] = m
		return
	}
	s.exportPendingMsgs = append(s.exportPendingMsgs, m)
}

func (s *session) handleCriticalExportEvent(idx int, m *bus.Message) {
	if idx < 0 || idx >= len(s.criticalExportReplayPending) {
		return
	}
	if !s.exportCanSend(time.Now()) {
		s.queueCriticalExport(idx, m)
		return
	}
	sent, ok := s.sendExportMessage(m)
	if !ok {
		s.queueCriticalExport(idx, m)
		return
	}
	if sent && s.criticalExportReplayPending[idx] {
		s.criticalExportReplayPending[idx] = false
	}
	s.criticalExportPendingMsgs[idx] = nil
	s.drainQueuedExports()
}

func (s *session) handleExportEvent(m *bus.Message) {
	if !s.exportCanSend(time.Now()) || !s.criticalExportReplayDrained() {
		s.queueExport(m)
		return
	}
	if len(s.criticalExportSubs) > 0 && isCriticalExportTopic(m.Topic) {
		return
	}
	_, ok := s.sendExportMessage(m)
	if !ok {
		s.queueExport(m)
	}
}

func (s *session) drainQueuedExports() {
	now := time.Now()
	if !s.exportCanSend(now) {
		return
	}
	for i, m := range s.criticalExportPendingMsgs {
		if m == nil || (i < len(s.criticalExportReplayPending) && !s.criticalExportReplayPending[i]) {
			continue
		}
		sent, ok := s.sendExportMessage(m)
		if !ok {
			return
		}
		if sent && i < len(s.criticalExportReplayPending) {
			s.criticalExportReplayPending[i] = false
			s.criticalExportPendingMsgs[i] = nil
		}
	}
	if !s.criticalExportReplayDrained() {
		s.scheduleExportDrain(now)
		return
	}
	for len(s.exportPendingMsgs) > 0 {
		m := s.exportPendingMsgs[0]
		s.exportPendingMsgs = s.exportPendingMsgs[1:]
		if m == nil || (len(s.criticalExportSubs) > 0 && isCriticalExportTopic(m.Topic)) {
			continue
		}
		_, ok := s.sendExportMessage(m)
		if !ok {
			s.exportPendingMsgs = append([]*bus.Message{m}, s.exportPendingMsgs...)
			return
		}
	}
}

func (s *session) scheduleExportDrain(now time.Time) {
	when := now.Add(time.Millisecond)
	if s.exportDrainAt.IsZero() || when.Before(s.exportDrainAt) {
		s.exportDrainAt = when
	}
}

// drainExports does a non-blocking read of each export subscription
// and writes any messages to the wire. Called from the main loop.
func (s *session) drainExports() {
	now := time.Now()
	s.exportDrainAt = time.Time{}
	if s.link != linkUp {
		return
	}
	if !s.exportsEnabled {
		return
	}
	if !s.exportReadyAt.IsZero() && now.Before(s.exportReadyAt) {
		return
	}
	total := 0
	if !s.drainCriticalExports(&total) {
		return
	}
	if !s.criticalExportReplayDrained() {
		s.scheduleExportDrain(now)
		return
	}
	for _, sub := range s.exportSubs {
		for {
			if total >= exportMaxPerTick {
				s.scheduleExportDrain(now)
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

func (s *session) findInboundCall(id string) (*inboundCall, int) {
	for i, call := range s.inboundCalls {
		if call.id == id {
			return call, i
		}
	}
	return nil, -1
}

func (s *session) removeInboundCall(idx int) {
	if idx < 0 || idx >= len(s.inboundCalls) {
		return
	}
	s.inboundCalls = append(s.inboundCalls[:idx], s.inboundCalls[idx+1:]...)
}

func (s *session) handleInboundReplyEvent(id string, reply *bus.Message, closed bool) {
	call, idx := s.findInboundCall(id)
	if call == nil {
		return
	}
	if call.sub != nil {
		s.conn.Unsubscribe(call.sub)
		call.sub = nil
	}
	s.removeInboundCall(idx)
	if closed || reply == nil {
		sent := s.sendRPC(marshal(protoReply{Type: msgReply, Corr: call.id, OK: false, Err: reasonTimeout}))
		s.rpcDiagInbound("call_reply_tx", call, false, reasonTimeout, otadiag.KV("sent", sent))
		return
	}
	if errStr := checkBusError(reply.Payload); errStr != "" {
		sent := s.sendRPC(marshal(protoReply{Type: msgReply, Corr: call.id, OK: false, Err: errStr}))
		s.rpcDiagInbound("call_reply_tx", call, false, errStr, otadiag.KV("sent", sent))
		return
	}
	payload, err := marshalPayload(reply.Payload)
	if err != nil {
		sent := s.sendRPC(marshal(protoReply{Type: msgReply, Corr: call.id, OK: false, Err: errPayloadMarshal}))
		s.rpcDiagInbound("call_reply_tx", call, false, errPayloadMarshal, otadiag.KV("sent", sent))
		return
	}
	sent := s.sendRPC(marshal(protoReply{Type: msgReply, Corr: call.id, OK: true, Payload: payload}))
	s.rpcDiagInbound("call_reply_tx", call, true, "", otadiag.KV("sent", sent))
}

func (s *session) expireInbound(now time.Time) {
	if len(s.inboundCalls) == 0 {
		return
	}
	keep := s.inboundCalls[:0]
	for _, call := range s.inboundCalls {
		if !now.Before(call.deadline) {
			if call.sub != nil {
				s.conn.Unsubscribe(call.sub)
				call.sub = nil
			}
			sent := s.sendRPC(marshal(protoReply{Type: msgReply, Corr: call.id, OK: false, Err: reasonTimeout}))
			s.rpcDiagInbound("call_reply_tx", call, false, reasonTimeout, otadiag.KV("sent", sent))
			continue
		}
		keep = append(keep, call)
	}
	s.inboundCalls = keep
}

func (s *session) drainInbound(now time.Time) {
	// Reactor path: bus readiness is coalesced by SubscriptionSet, then this
	// reducer drains all ready inbound replies without blocking. Direct calls
	// still let unit tests exercise the reducer without running the event loop.
	calls := append([]*inboundCall(nil), s.inboundCalls...)
	for _, call := range calls {
		if call == nil || call.sub == nil {
			continue
		}
		select {
		case reply, ok := <-call.sub.Channel():
			s.handleInboundReplyEvent(call.id, reply, !ok)
		default:
		}
	}
	s.expireInbound(now)
}

func (s *session) handleOutboundCallEvent(now time.Time, msg *bus.Message) {
	if s.link != linkUp || msg == nil {
		return
	}
	wireTopic := exportCallTopic(msg.Topic)
	if wireTopic == nil {
		return
	}
	payload, err := marshalPayload(msg.Payload)
	if err != nil {
		s.logKV("outgoing call dropped", "err", err.Error())
		if msg.CanReply() {
			s.conn.Reply(msg, types.ErrorReply{OK: false, Error: errPayloadMarshal}, false)
		}
		return
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
	_ = s.sendRPC(marshal(protoCall{
		Type:      msgCall,
		ID:        corr,
		Topic:     wireTopic,
		Payload:   payload,
		TimeoutMs: int(callTimeoutDef / time.Millisecond),
	}))
}

func (s *session) expireOutbound(now time.Time) {
	if len(s.outboundCalls) == 0 {
		return
	}
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

func (s *session) drainOutboundMessages(now time.Time) {
	for _, sub := range s.exportCallSubs {
		for {
			select {
			case msg, ok := <-sub.Channel():
				if !ok || msg == nil {
					goto nextSub
				}
				s.handleOutboundCallEvent(now, msg)
			default:
				goto nextSub
			}
		}
	nextSub:
	}
	s.expireOutbound(now)
}

func (s *session) drainOutbound(now time.Time) { s.drainOutboundMessages(now) }

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
