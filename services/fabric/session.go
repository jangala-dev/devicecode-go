package fabric

import (
	"bytes"
	"context"
	"devicecode-go/utilities/diag"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"devicecode-go/bus"
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
	// lineQueueSize is deliberately tiny on MCU builds. The UART RX ring is
	// already the byte-level shock absorber; keeping only two fully decoded
	// JSONL frames avoids reserving 32 * maxLineLen bytes of static RAM. This
	// preserves allocation discipline without starving the reactor behind a
	// large preallocated line queue.
	lineQueueSize = 2
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
	maxPendingExports = 32
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

type readResult struct {
	line []byte
	slot int
	err  error
}

type exportItem struct {
	topic    bus.Topic
	payload  any
	retained bool
	unset    bool
}

type linkStatePayload struct {
	LinkID            string         `json:"link_id"`
	Status            string         `json:"status"`
	Ready             bool           `json:"ready"`
	Established       bool           `json:"established"`
	PeerID            string         `json:"peer_id"`
	LocalSID          string         `json:"local_sid"`
	PeerSID           string         `json:"peer_sid,omitempty"`
	PeerNode          string         `json:"peer_node,omitempty"`
	PeerProto         string         `json:"peer_proto,omitempty"`
	LastRxUnixMilli   int64          `json:"last_rx_unix_ms,omitempty"`
	LastTxUnixMilli   int64          `json:"last_tx_unix_ms,omitempty"`
	LastPongUnixMilli int64          `json:"last_pong_unix_ms,omitempty"`
	Reason            string         `json:"reason,omitempty"`
	Err               string         `json:"err,omitempty"`
	Counters          FabricCounters `json:"counters"`
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

	exportPendingItems    [maxPendingExports]exportItem
	exportPendingHead     int
	exportPendingLen      int
	exportRetainedWatches []*bus.RetainedWatch
	exportEventSubs       []*bus.Subscription
	nextPingAt            time.Time
	txControl             txLane
	txRPC                 txLane
	txBulk                txLane
	importedRetained      []bus.Topic // local topics currently retained on the bus due to wire imports
	rpcReady              bool        // bridge replay complete; gates linkStatePayload.Ready
	incomingTransfer      *incomingTransfer
	completedTransfers    []completedTransfer
	pendingTargetCall     *pendingTargetCall
	targetCallResults     chan targetCallResult
	beginTransfer         func(transferMeta) (transferSink, error)
	stageController       StageController
	buffers               *FabricBuffers
	counters              FabricCounters
	busSubs               *bus.SubscriptionSet
	linkStateTopic        bus.Topic
	fastFrameBuf          []byte
	ctx                   context.Context
}

func (s *session) log(msg string) {
	diag.Println("[fabric]", "sid", s.localSID, msg)
}

func (s *session) logKV(msg, key, value string) {
	diag.Println("[fabric]", "sid", s.localSID, msg, key, value)
}

// run is the main loop. Blocks until ctx is cancelled.
func (s *session) run(ctx context.Context) {
	s.cfg.applyDefaults()
	s.buffers = ensureFabricBuffers(s.buffers)
	s.linkStateTopic = bus.T("state", "fabric", "link", s.linkID)
	s.ctx = ctx
	if s.targetCallResults == nil {
		s.targetCallResults = make(chan targetCallResult, 1)
	}
	s.busSubs = s.conn.NewSubscriptionSet()
	lines := make(chan readResult, lineQueueSize)
	freeSlots := make(chan int, lineQueueSize)
	for i := 0; i < lineQueueSize; i++ {
		freeSlots <- i
	}

	go s.readLoop(ctx, lines, freeSlots)

	defer s.tr.Close()
	defer func() {
		if s.busSubs != nil {
			s.busSubs.Close()
		}
	}()
	defer s.teardownExports()
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
				s.releaseReadSlot(freeSlots, res.slot)
				if errors.Is(res.err, ErrLineTooLong) {
					s.counters.RXLineTooLong++
					s.logActiveTransferRXLoss("line_too_long", maxLineLen+1, "", "", res.err.Error())
					s.requestTransferRetry("line_too_long", false)
					continue
				}
				s.handleLinkDown(reasonTransportDown, res.err.Error())
				return
			}
			s.counters.RXLines++
			beforeRx := s.lastRxAt
			s.dispatch(res.line)
			s.releaseReadSlot(freeSlots, res.slot)
			if s.lastRxAt.After(beforeRx) {
				resetTimer(stale, s.cfg.LivenessTimeout)
			}

		case <-pendingDeadlineCh:
			s.handlePendingDeadline(time.Now())

		case result := <-s.targetCallResults:
			s.handleTargetCallResult(result)

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

func (s *session) readLoop(ctx context.Context, lines chan<- readResult, freeSlots <-chan int) {
	defer close(lines)
	lastLineAt := time.Now()
	_ = lastLineAt
	for {
		var slot int
		select {
		case slot = <-freeSlots:
		case <-ctx.Done():
			return
		}
		started := time.Now()
		_ = started
		buf := s.buffers.RXLines[slot][:]
		n, err := s.readTransportLine(buf)
		now := time.Now()
		_ = now
		if err != nil {
			select {
			case lines <- readResult{slot: slot, err: err}:
			case <-ctx.Done():
				return
			}
			if !errors.Is(err, ErrLineTooLong) {
				return
			}
			continue
		}
		lastLineAt = now
		select {
		case lines <- readResult{line: buf[:n], slot: slot}:
		case <-ctx.Done():
			return
		}
	}
}

func (s *session) readTransportLine(dst []byte) (int, error) {
	if tr, ok := s.tr.(boundedLineTransport); ok {
		return tr.ReadLineInto(dst)
	}
	line, err := s.tr.ReadLine()
	if err != nil {
		return 0, err
	}
	if len(line) > len(dst) {
		return 0, ErrLineTooLong
	}
	copy(dst, line)
	return len(line), nil
}

func (s *session) releaseReadSlot(freeSlots chan<- int, slot int) {
	if slot < 0 {
		return
	}
	freeSlots <- slot
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
	counters := s.counters
	if s.link != linkUp && (reason != "" || err != "") {
		status = statusDown
	}
	topic := s.linkStateTopic
	if topic == nil {
		topic = bus.T("state", "fabric", "link", s.linkID)
	}
	s.conn.PublishValue(
		topic,
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
			Reason:            reason,
			Err:               err,
			Counters:          counters,
		},
		true,
	)
}

func (s *session) markRx() {
	s.lastRxAt = time.Now()
}

func (s *session) markFrameRX() {
	s.counters.RXFrames++
}

func (s *session) logActiveTransferRXLoss(reason string, lineLen int, frameType, xferID, errStr string) {
	cur := s.incomingTransfer
	if cur == nil {
		return
	}
	if frameType == "" {
		frameType = "-"
	}
	if xferID == "" {
		xferID = "-"
	}
	if errStr == "" {
		errStr = "-"
	}
	logFabricRXLoss(reason, cur.bytesWritten, cur.meta.Size, cur.chunksSeen, s.counters.RXLines, s.counters.RXLineTooLong, s.counters.RXBadJSON, lineLen, frameType, xferID, errStr)
}

func (s *session) markTx() {
	s.lastTxAt = time.Now()
	s.counters.TXFrames++
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
// elapsed. Retained state replay is handled uniformly by the wildcard export
// subscriptions; Fabric no longer treats any state/self topic as special.
func (s *session) tickReady(now time.Time) {
	if s.link != linkUp || s.rpcReady {
		return
	}
	if s.exportReadyAt.IsZero() || now.Before(s.exportReadyAt) {
		return
	}
	s.rpcReady = true
	s.publishLinkState("", "")
	s.drainQueuedExports()
}

// ---- dispatch ----

func (s *session) dispatch(line []byte) {
	t := protoType(line)
	if t != "" {
	}
	if t == "" {
		s.logMalformed(line, nil)
		return
	}

	switch t {
	case msgHello:
		msg, ok := decodeHelloFast(line)
		if !ok {
			s.logMalformed(line, errors.New("bad_hello"))
			return
		}
		s.markFrameRX()
		s.onHello(&msg)
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
		msg, ok := decodePingFast(line, msgPing)
		if !ok {
			s.logMalformed(line, errors.New("bad_ping"))
			return
		}
		s.markFrameRX()
		s.onPing(&msg)
	case msgPong:
		msg, ok := decodePongFast(line)
		if !ok {
			s.logMalformed(line, errors.New("bad_pong"))
			return
		}
		s.markFrameRX()
		s.onPong(&msg)
	case msgPub:
		typedDispatch(s, t, line, s.onPub)
	case msgUnretain:
		typedDispatch(s, t, line, s.onUnretain)
	case msgCall:
		msg, ok := decodeCallFast(line)
		if !ok {
			s.logMalformed(line, errors.New("bad_call"))
			return
		}
		s.markFrameRX()
		s.onCall(&msg)
	case msgReply:
		s.markFrameRX()
		s.log("unexpected reply dropped")
	case msgXferBegin:
		msg, ok := decodeXferBeginFast(line)
		if !ok {
			s.logMalformed(line, errors.New("bad_xfer_begin"))
			return
		}
		s.markFrameRX()
		s.onTransferBegin(&msg)
	case msgXferChunk:
		msg, ok := decodeXferChunkFast(line)
		if !ok {
			s.logMalformed(line, errors.New("bad_xfer_chunk"))
			s.retryMalformedTransferFrame(t, line)
			return
		}
		s.markFrameRX()
		s.onTransferChunk(&msg)
	case msgXferCommit:
		msg, ok := decodeXferCommitFast(line)
		if !ok {
			s.logMalformed(line, errors.New("bad_xfer_commit"))
			return
		}
		s.markFrameRX()
		s.onTransferCommit(&msg)
	case msgXferAbort:
		msg, ok := decodeXferAbortFast(line)
		if !ok {
			s.logMalformed(line, errors.New("bad_xfer_abort"))
			return
		}
		s.markFrameRX()
		s.onTransferAbort(&msg)
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
		}
		s.logMalformed(line, err)
		return
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("trailing_json")
		}
		if msgType == msgXferBegin {
		}
		s.logMalformed(line, err)
		return
	}
	s.markFrameRX()
	handler(&msg)
	if msgType == msgXferBegin {
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
	s.counters.RXBadJSON++
	errStr := ""
	if err != nil {
		errStr = err.Error()
	}
	frameType := protoType(line)
	wireXferID := protoXferID(line)
	s.logActiveTransferRXLoss("bad_json", len(line), frameType, wireXferID, errStr)

	// During an active stop-and-wait transfer, any malformed inbound frame is
	// treated as evidence that the current expected chunk may have been lost.
	// Malformed xfer_chunk frames are handled by retryMalformedTransferFrame so
	// current-transfer chunks are retried promptly and non-current chunks do not
	// charge the active transfer. Other malformed traffic is rate-limited.
	if frameType == msgXferChunk {
		return
	}
	s.requestTransferRetry("bad_json", false)
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

	if !s.sendControl(marshalHelloAck(s.localSID, s.nodeID)) {
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
	if !s.sendControl(marshalPong(s.localSID)) {
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
	if !s.sendControl(marshalPing(s.localSID)) {
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
		s.sendRPC(marshalReplyErr(msg.ID, "bad_topic"))
		return
	}
	localTopic := importCallTopic(msg.Topic)
	if localTopic == nil {
		s.log("incoming call dropped: no_route")
		s.sendRPC(marshalReplyErr(msg.ID, reasonNoRoute))
		return
	}

	s.markRx()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeoutDef)
	defer cancel()
	reply, err := s.conn.Call(ctx, localTopic, msg.Payload)
	if err != nil {
		reason := err.Error()
		if reason == "bus: no_route" {
			reason = reasonNoRoute
		}
		s.sendRPC(marshalReplyErr(msg.ID, reason))
		return
	}
	payload, err := marshalPayload(reply)
	if err != nil {
		s.sendRPC(marshalReplyErr(msg.ID, "bad_reply_payload"))
		return
	}
	s.sendRPC(marshalReplyOKRaw(msg.ID, payload))
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

// A wildcard state/self/# subscription can receive a retained replay larger
// than the main bus queue (production currently uses a queue of three). Use a
// larger bounded queue for export subscriptions so Fabric can coalesce and send
// the full retained surface on link establishment rather than silently dropping
// retained facts in the subscription buffer.
const exportReplayQueueLen = 32

func (s *session) busReady() <-chan struct{} {
	if s.busSubs == nil {
		return nil
	}
	return s.busSubs.Ready()
}

func (s *session) subscribeBus(tp bus.Topic) *bus.Subscription {
	return s.subscribeBusBuffered(tp, 0)
}

func (s *session) subscribeBusBuffered(tp bus.Topic, queueLen int) *bus.Subscription {
	if s.busSubs == nil {
		return s.conn.SubscribeBuffered(tp, queueLen)
	}
	return s.busSubs.SubscribeBuffered(tp, queueLen)
}

func (s *session) watchRetained(tp bus.Topic, opts bus.RetainedWatchOptions) *bus.RetainedWatch {
	if s.busSubs == nil {
		return s.conn.WatchRetained(tp, opts)
	}
	return s.busSubs.WatchRetained(tp, opts)
}

func (s *session) drainBusEvents(now time.Time) {
	// A single bus readiness edge may cover several subscriptions. Drain each
	// class without blocking. Export drains collect/coalesce retained facts and
	// admit only a small number of frames per tick. This keeps the UART writer
	// event-driven without adding a second writer actor or a transfer-specific
	// quiet window.
	s.drainExports()
	s.tickReady(now)
}

func (s *session) setupExports() {
	if s.conn == nil {
		return
	}
	for _, p := range exportRetainedPatterns() {
		watch := s.watchRetained(p, bus.RetainedWatchOptions{Replay: true, QueueLen: exportReplayQueueLen})
		s.exportRetainedWatches = append(s.exportRetainedWatches, watch)
	}
	for _, p := range exportEventPatterns() {
		sub := s.subscribeBusBuffered(p, exportReplayQueueLen)
		s.exportEventSubs = append(s.exportEventSubs, sub)
	}
}

func (s *session) teardownExports() {
	s.clearExportPending()
	for _, watch := range s.exportRetainedWatches {
		s.conn.UnwatchRetained(watch)
	}
	s.exportRetainedWatches = nil
	for _, sub := range s.exportEventSubs {
		s.conn.Unsubscribe(sub)
	}
	s.exportEventSubs = nil
}

func (s *session) sendExportMessage(m *bus.Message) (bool, bool) {
	if m == nil {
		return false, true
	}
	return s.sendExportItem(exportItem{topic: m.Topic, payload: m.Payload, retained: m.Retained, unset: m.Retained && m.Payload == nil})
}

func (s *session) sendExportItem(item exportItem) (bool, bool) {
	if item.topic == nil {
		return false, true
	}
	if item.unset {
		if s.writerIdle() {
			s.fastFrameBuf = s.fastFrameBuf[:0]
			if frame, ok := appendUnretainTopic(s.fastFrameBuf, item); ok {
				s.fastFrameBuf = frame
				if !s.writeFrame(laneRPC, frame) {
					return false, false
				}
				return true, true
			}
		}
		if frame, ok := marshalUnretainTopic(item); ok {
			if !s.sendRPC(frame) {
				return false, false
			}
			return true, true
		}
		wire := exportTopic(item.topic)
		if wire == nil {
			return false, true
		}
		if !s.sendRPC(marshal(protoUnretain{
			Type:  msgUnretain,
			Topic: wire,
		})) {
			return false, false
		}
		return true, true
	}
	if payload, ok := item.payload.(appendJSONPayload); ok {
		if s.writerIdle() {
			s.fastFrameBuf = s.fastFrameBuf[:0]
			if frame, ok := appendPubAppendJSON(s.fastFrameBuf, item, payload); ok {
				s.fastFrameBuf = frame
				if !s.writeFrame(laneRPC, frame) {
					return false, false
				}
				return true, true
			}
		}
		if frame, ok := marshalPubAppendJSON(item, payload); ok {
			if !s.sendRPC(frame) {
				return false, false
			}
			return true, true
		}
	}
	wire := exportTopic(item.topic)
	if wire == nil {
		return false, true
	}
	payload, err := marshalPayload(item.payload)
	if err != nil {
		s.logKV("export payload dropped", "err", err.Error())
		return false, true
	}
	if !s.sendRPC(marshal(protoPub{
		Type:    msgPub,
		Topic:   wire,
		Payload: payload,
		Retain:  item.retained,
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
			copy := m
			latest = &copy
		default:
			return latest
		}
	}
}

func (s *session) exportCanSend(now time.Time) bool {
	return s.link == linkUp && s.exportsEnabled && (s.exportReadyAt.IsZero() || !now.Before(s.exportReadyAt))
}

func (s *session) exportPendingIndex(offset int) int {
	return (s.exportPendingHead + offset) % maxPendingExports
}

func (s *session) clearExportPending() {
	for i := 0; i < s.exportPendingLen; i++ {
		s.exportPendingItems[s.exportPendingIndex(i)] = exportItem{}
	}
	s.exportPendingHead = 0
	s.exportPendingLen = 0
}

func (s *session) pushExportBack(item exportItem) {
	if s.exportPendingLen == maxPendingExports {
		s.exportPendingItems[s.exportPendingHead] = exportItem{}
		s.exportPendingHead = s.exportPendingIndex(1)
		s.exportPendingLen--
	}
	idx := s.exportPendingIndex(s.exportPendingLen)
	s.exportPendingItems[idx] = item
	s.exportPendingLen++
}

func (s *session) pushExportFront(item exportItem) {
	if s.exportPendingLen == maxPendingExports {
		// Preserve the retried item by dropping the oldest tail entry.
		tail := s.exportPendingIndex(s.exportPendingLen - 1)
		s.exportPendingItems[tail] = exportItem{}
		s.exportPendingLen--
	}
	s.exportPendingHead = (s.exportPendingHead + maxPendingExports - 1) % maxPendingExports
	s.exportPendingItems[s.exportPendingHead] = item
	s.exportPendingLen++
}

func (s *session) popExportFront() (exportItem, bool) {
	if s.exportPendingLen == 0 {
		return exportItem{}, false
	}
	item := s.exportPendingItems[s.exportPendingHead]
	s.exportPendingItems[s.exportPendingHead] = exportItem{}
	s.exportPendingHead = s.exportPendingIndex(1)
	s.exportPendingLen--
	if s.exportPendingLen == 0 {
		s.exportPendingHead = 0
	}
	return item, true
}

func (s *session) queueExportMessage(m *bus.Message) {
	if m == nil {
		return
	}
	s.queueExportItem(exportItem{topic: m.Topic, payload: m.Payload, retained: m.Retained, unset: m.Retained && m.Payload == nil})
}

func (s *session) queueRetainedExport(ev bus.RetainedEvent) {
	switch ev.Op {
	case bus.RetainedSet:
		s.queueExportItem(exportItem{topic: ev.Topic, payload: ev.Payload, retained: true})
	case bus.RetainedUnset:
		s.queueExportItem(exportItem{topic: ev.Topic, retained: true, unset: true})
	}
}

func (s *session) queueExportItem(item exportItem) {
	if item.topic == nil {
		return
	}
	// Retained exports are a cache: when several retained facts for the same
	// topic arrive before the UART writer has capacity, only the newest value is
	// useful on the wire. Coalescing here keeps the output path event-driven and
	// avoids a semantic "pause exports during transfer" policy.
	if item.retained {
		for i := 0; i < s.exportPendingLen; i++ {
			idx := s.exportPendingIndex(i)
			pending := s.exportPendingItems[idx]
			if pending.topic != nil && pending.retained && topicEquals(pending.topic, item.topic) {
				s.exportPendingItems[idx] = item
				return
			}
		}
	}
	// Non-retained events are sparse, but keep the FIFO bounded. If the link is
	// congested, old observations are less valuable than keeping the
	// reactor and control frames moving.
	s.pushExportBack(item)
}

func (s *session) hasExportBacklog() bool {
	return s.exportPendingLen > 0
}

func (s *session) handleExportEvent(m *bus.Message) {
	if m == nil {
		return
	}
	s.queueExportMessage(m)
	s.scheduleExportDrain(time.Now())
}

func (s *session) drainQueuedExports() {
	now := time.Now()
	if !s.exportCanSend(now) {
		return
	}
	total := 0
	for total < exportMaxPerTick && s.exportPendingLen > 0 {
		item, ok := s.popExportFront()
		if !ok || item.topic == nil {
			continue
		}
		sent, alive := s.sendExportItem(item)
		if !alive {
			s.pushExportFront(item)
			return
		}
		if sent {
			total++
		}
	}
	if s.hasExportBacklog() {
		s.scheduleExportDrain(now)
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
	if !s.exportCanSend(now) {
		return
	}
	// Collect all immediately-available export notifications into the session's
	// coalesced retained queue. Sending is handled by drainQueuedExports below,
	// with a small per-tick budget. This keeps retained replay fair without
	// making transfer state a special case.
	for _, watch := range s.exportRetainedWatches {
		for {
			ev, ok := watch.TryNext()
			if !ok {
				break
			}
			s.queueRetainedExport(ev)
		}
	}
	for _, sub := range s.exportEventSubs {
		for {
			select {
			case m, ok := <-sub.Channel():
				if !ok {
					goto nextSub
				}
				s.queueExportMessage(&m)
			default:
				goto nextSub
			}
		}
	nextSub:
	}
	s.drainQueuedExports()
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
