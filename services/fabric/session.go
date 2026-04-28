package fabric

import (
	"context"
	"encoding/json"
	"errors"
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
// Timing relationships:
//   staleTimeout (45s) > callTimeoutDef (5s)
//
// The CM5 sends pings every 15s of TX inactivity. The MCU marks the
// peer stale after 45s without any RX, giving a 30s margin.
// Exports are enabled immediately on link-up (after exportStartHoldoff).

const (
	staleTimeout       = 45 * time.Second
	callTimeoutDef     = 5 * time.Second
	waitLogEvery       = 2 * time.Second
	exportStartHoldoff = 1 * time.Second
	// postHelloAckSettle gives the serial reactor goroutine a chance
	// to drain the hello_ack bytes from the TX shmring before
	// promoteLink publishes bus state and triggers export work.
	// TinyGo's cooperative scheduler does not preempt, so without
	// this yield the reactor may not run until the next tick.
	postHelloAckSettle = 10 * time.Millisecond
	// exportMaxPerTick caps the total export messages sent per drain
	// cycle across all subscriptions, keeping UART throughput within
	// the 115200-baud link capacity.
	exportMaxPerTick   = 1
	exportTickInterval = 50 * time.Millisecond
	errPayloadMarshal  = "payload_marshal_failed"
)

// ---- link reasons and error strings ----

const (
	reasonLinkDown           = "link_down"
	reasonPeerStale          = "peer_stale"
	reasonPeerReset          = "peer_reset"
	reasonPeerSessionChanged = "peer_session_changed"
	reasonHelloRejected      = "hello_rejected"
	reasonTransportDown      = "transport_down"
	reasonTransportWrite     = "transport_write_failed"
	reasonNoRoute            = "no_route"
	reasonTimeout            = "timeout"
)

// ---- bus topics for config handling ----

var (
	tConfigHAL    = bus.T("config", "hal")
	dumpCallTopic = []string{"rpc", "hal", "dump"}
)

// ---- types ----

type dumpReply struct {
	OK          bool            `json:"ok"`
	Method      string          `json:"method"`
	Echo        any             `json:"echo,omitempty"`
	HAL         *types.HALState `json:"hal,omitempty"`
	Applied     bool            `json:"applied"`
	ConfigCount int             `json:"config_count,omitempty"`
	ConfigError string          `json:"config_error,omitempty"`
}

type inboundCall struct {
	id       string
	sub      *bus.Subscription
	deadline time.Time
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
	PeerProto         int    `json:"peer_proto,omitempty"`
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

	link           linkState
	peerNode       string
	peerSID        string
	peerProto      int
	lastRxAt       time.Time
	lastTxAt       time.Time
	lastPongAt     time.Time
	exportReadyAt  time.Time
	exportsEnabled bool

	exportSubs       []*bus.Subscription
	exportCallSubs   []*bus.Subscription
	inboundCalls     []*inboundCall
	outboundCalls    []*outboundCall
	nextOutboundID   uint64
	incomingTransfer *incomingTransfer
	beginTransfer    func(transferMeta) (transferSink, error)

	// Config state — tracks config/device → config/hal translation.
	configApplied bool
	configCount   int
	lastConfigErr string
}

func (s *session) log(msg string) {
	println("[fabric]", "sid", s.localSID, msg)
}

func (s *session) logKV(msg, key, value string) {
	println("[fabric]", "sid", s.localSID, msg, key, value)
}

// run is the main loop. Blocks until ctx is cancelled.
func (s *session) run(ctx context.Context) {
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

	stale := time.NewTimer(staleTimeout)
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
			s.dispatch(res.line)
			resetTimer(stale, staleTimeout)

		case <-exportTick.C:
			s.drainExports()
			s.drainInbound(time.Now())
			s.drainOutbound(time.Now())

		case <-waitTick.C:
			s.logWaiting()

		case <-stale.C:
			if s.link == linkUp {
				s.handleLinkDown(reasonPeerStale, "")
			} else {
				stale.Reset(staleTimeout)
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
	if s.link == linkUp {
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
			Ready:             s.link == linkUp,
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
	s.peerProto = 0
	s.exportReadyAt = time.Time{}
	s.exportsEnabled = false
	s.teardownExports()
	s.teardownInbound()
	s.teardownOutbound(pendingReason)
	s.abortTransfer(pendingReason)
	s.publishLinkState(reason, err)
	if err != "" {
		s.logKV("link down", "err", err)
	} else if reason != "" {
		s.logKV("link down", "reason", reason)
	}
}

// promoteLink transitions to linkUp, tearing down any prior session state.
func (s *session) promoteLink(reason string) {
	if s.link == linkUp {
		if reason == "" {
			reason = reasonPeerReset
		}
		s.abortTransfer(reason)
		s.teardownExports()
		s.teardownInbound()
		s.teardownOutbound(reason)
	}
	s.link = linkUp
	s.setupExports()
	s.exportsEnabled = true
	s.exportReadyAt = time.Now().Add(exportStartHoldoff)
	s.log("exports enabled")
	s.publishLinkState(reason, "")
}

// ---- dispatch ----

func (s *session) dispatch(line []byte) {
	t := protoType(line)
	if t == "" {
		s.logMalformed(line, nil)
		return
	}
	s.markRx()

	switch t {
	case msgHello:
		typedDispatch(s, line, s.onHello)
		return
	case msgHelloAck:
		typedDispatch(s, line, s.onHelloAck)
		return
	}

	if !s.requireLinkUp(t) {
		return
	}

	switch t {
	case msgPing:
		typedDispatch(s, line, s.onPing)
	case msgPong:
		typedDispatch(s, line, s.onPong)
	case msgPub:
		typedDispatch(s, line, s.onPub)
	case msgUnretain:
		typedDispatch(s, line, s.onUnretain)
	case msgCall:
		typedDispatch(s, line, s.onCall)
	case msgReply:
		typedDispatch(s, line, s.onReply)
	case msgXferBegin:
		typedDispatch(s, line, s.onTransferBegin)
	case msgXferChunk:
		typedDispatch(s, line, s.onTransferChunk)
	case msgXferCommit:
		typedDispatch(s, line, s.onTransferCommit)
	case msgXferAbort:
		typedDispatch(s, line, s.onTransferAbort)
	default:
		s.logKV("unknown message type dropped", "type", t)
	}
}

func typedDispatch[T any](s *session, line []byte, handler func(*T)) {
	var msg T
	if err := json.Unmarshal(line, &msg); err != nil {
		s.logMalformed(line, err)
		return
	}
	handler(&msg)
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
	println(
		"[fabric]", "sid", s.localSID,
		"malformed frame dropped",
		"line_len", strconvx.Itoa(len(line)),
		"line_head", tracePreview(line),
		"err", errStr,
	)
}

// notePeerIdentity records the remote peer's node, SID, and proto version.
// If the SID changes mid-session, the returned reason triggers a full
// teardown of exports and pending calls on the Go side. Note: the Lua
// side only tears down pending calls on SID change, not exports — this
// asymmetry is intentional since the CM5 re-subscribes on reconnect.
func (s *session) notePeerIdentity(node, sid string, proto int) string {
	reason := ""
	if s.link == linkUp && s.peerSID != "" && sid != "" && s.peerSID != sid {
		reason = reasonPeerSessionChanged
	}
	if node != "" {
		s.peerNode = node
	}
	if sid != "" {
		s.peerSID = sid
	}
	if proto > 0 {
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

func (s *session) onHello(msg *protoHello) {
	if msg.Peer != "" && msg.Peer != s.nodeID {
		s.log("hello dropped: wrong peer")
		return
	}
	if s.peerID != "" && msg.Node != s.peerID {
		s.log("hello dropped: wrong node")
		return
	}
	reason := s.notePeerIdentity(msg.Node, msg.SID, msg.Proto)
	s.logKV("hello rx", "peer_sid", msg.SID)

	if !s.sendFrame(marshal(protoHelloAck{
		Type:  msgHelloAck,
		Node:  s.nodeID,
		SID:   s.localSID,
		Proto: protoVersion,
		OK:    true,
	})) {
		return
	}
	s.log("hello_ack tx")
	time.Sleep(postHelloAckSettle)
	s.promoteLink(reason)
}

func (s *session) onHelloAck(msg *protoHelloAck) {
	if s.isSelfControlFrame(msg.Node, msg.SID) {
		s.log("echoed hello_ack ignored")
		return
	}
	if !msg.OK {
		s.log("hello_ack rejected by peer")
		s.handleLinkDown(reasonHelloRejected, "")
		return
	}
	reason := s.notePeerIdentity(msg.Node, msg.SID, msg.Proto)
	s.logKV("hello_ack rx", "peer_sid", msg.SID)
	s.promoteLink(reason)
}

func (s *session) onPing(msg *protoPing) {
	s.logKV("ping rx", "peer_sid", msg.SID)
	if !s.sendFrame(marshal(protoPong{Type: msgPong, TS: msg.TS, SID: s.localSID})) {
		return
	}
	s.log("pong tx")
}

func (s *session) onPong(msg *protoPong) {
	if s.isSelfControlFrame("", msg.SID) {
		s.log("echoed pong ignored")
		return
	}
	s.lastPongAt = s.lastRxAt
}

func (s *session) onPub(msg *protoPub) {
	localTopic := importPublishTopic(msg.Topic)
	if localTopic == nil {
		if hasWirePrefix(msg.Topic, []string{"state"}) {
			s.log("echoed state pub ignored")
			return
		}
		s.log("incoming pub dropped: no_route")
		return
	}

	// config/device → config/hal: normalize and track.
	if topicEquals(localTopic, tConfigHAL) {
		cfg, err := decodeHALConfig(msg.Payload)
		if err != "" {
			s.lastConfigErr = err
			s.log("config/device rejected: " + err)
			return
		}
		s.configApplied = true
		s.configCount++
		s.lastConfigErr = ""
		s.log("config/device applied to config/hal")
		s.conn.Publish(s.conn.NewMessage(localTopic, cfg, true))
		return
	}

	s.conn.Publish(s.conn.NewMessage(localTopic, msg.Payload, msg.Retain))
}

func (s *session) onUnretain(msg *protoUnretain) {
	localTopic := importPublishTopic(msg.Topic)
	if localTopic == nil {
		s.log("incoming unretain dropped: no_route")
		return
	}
	s.conn.Publish(s.conn.NewMessage(localTopic, nil, true))
}

func (s *session) onCall(msg *protoCall) {
	// rpc/hal/dump: handle directly — reply with config and HAL state.
	if slicesEqualStrings(msg.Topic, dumpCallTopic) {
		var halState *types.HALState
		sub := s.conn.Subscribe(bus.T("hal", "state"))
		select {
		case m := <-sub.Channel():
			if m != nil {
				if st, ok := decodeHALState(m.Payload); ok {
					halState = &st
				}
			}
		default:
		}
		s.conn.Unsubscribe(sub)

		reply := dumpReply{
			OK:          true,
			Method:      "dump",
			Echo:        decodePayload(msg.Payload),
			HAL:         halState,
			Applied:     s.configApplied,
			ConfigCount: s.configCount,
			ConfigError: s.lastConfigErr,
		}
		s.sendFrame(marshal(protoReply{Type: msgReply, Corr: msg.ID, OK: true, Value: mustMarshal(reply)}))
		return
	}

	localTopic := importCallTopic(msg.Topic)
	if localTopic == nil {
		s.log("incoming call dropped: no_route")
		s.sendFrame(marshal(protoReply{Type: msgReply, Corr: msg.ID, OK: false, Err: reasonNoRoute}))
		return
	}

	timeout := callTimeoutDef
	if msg.TimeoutMs > 0 {
		timeout = time.Duration(msg.TimeoutMs) * time.Millisecond
	}
	busMsg := s.conn.NewMessage(localTopic, msg.Payload, false)
	sub := s.conn.Request(busMsg)
	s.inboundCalls = append(s.inboundCalls, &inboundCall{
		id:       msg.ID,
		sub:      sub,
		deadline: time.Now().Add(timeout),
	})
}

func (s *session) onReply(msg *protoReply) {
	for i, call := range s.outboundCalls {
		if call.id != msg.Corr {
			continue
		}
		s.outboundCalls = append(s.outboundCalls[:i], s.outboundCalls[i+1:]...)
		if !call.req.CanReply() {
			return
		}
		if !msg.OK {
			s.conn.Reply(call.req, types.ErrorReply{OK: false, Error: msg.Err}, false)
			return
		}
		s.conn.Reply(call.req, decodePayload(msg.Value), false)
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

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{"error":"marshal_failed"}`)
	}
	return json.RawMessage(b)
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
	for _, p := range exportPatterns() {
		s.exportSubs = append(s.exportSubs, s.conn.Subscribe(p))
	}
	for _, p := range exportCallPatterns() {
		s.exportCallSubs = append(s.exportCallSubs, s.conn.Subscribe(p))
	}
}

func (s *session) teardownExports() {
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

// drainExports does a non-blocking read of each export subscription
// and writes any messages to the wire. Called from the main loop.
func (s *session) drainExports() {
	if s.link != linkUp {
		return
	}
	if !s.exportsEnabled {
		return
	}
	if !s.exportReadyAt.IsZero() && time.Now().Before(s.exportReadyAt) {
		return
	}
	total := 0
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
				wire := exportTopic(m.Topic)
				if wire == nil {
					continue
				}
				if m.Retained && m.Payload == nil {
					if !s.sendFrame(marshal(protoUnretain{
						Type:  msgUnretain,
						Topic: wire,
					})) {
						return
					}
					total++
					continue
				}
				payload, err := marshalPayload(m.Payload)
				if err != nil {
					s.logKV("export payload dropped", "err", err.Error())
					continue
				}
				if !s.sendFrame(marshal(protoPub{
					Type:    msgPub,
					Topic:   wire,
					Payload: payload,
					Retain:  m.Retained,
				})) {
					return
				}
				total++
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
				if !s.sendFrame(marshal(protoReply{Type: msgReply, Corr: call.id, OK: false, Err: reasonTimeout})) {
					return
				}
				continue
			}
			if errStr := checkBusError(reply.Payload); errStr != "" {
				if !s.sendFrame(marshal(protoReply{Type: msgReply, Corr: call.id, OK: false, Err: errStr})) {
					return
				}
				continue
			}
			payload, err := marshalPayload(reply.Payload)
			if err != nil {
				if !s.sendFrame(marshal(protoReply{Type: msgReply, Corr: call.id, OK: false, Err: errPayloadMarshal})) {
					return
				}
				continue
			}
			if !s.sendFrame(marshal(protoReply{Type: msgReply, Corr: call.id, OK: true, Value: payload})) {
				return
			}
			continue
		default:
		}

		if !now.Before(call.deadline) {
			s.conn.Unsubscribe(call.sub)
			call.sub = nil
			if !s.sendFrame(marshal(protoReply{Type: msgReply, Corr: call.id, OK: false, Err: reasonTimeout})) {
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
					if !s.sendFrame(marshal(protoCall{
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

func (s *session) sendFrame(data []byte) bool {
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	if err := s.tr.WriteLine(data); err != nil {
		if errors.Is(err, ErrLineTooLong) {
			// Oversized frame is dropped but the transport is still
			// healthy — return true so the session continues.
			s.log("oversized write dropped")
			return true
		}
		s.handleLinkDown(reasonTransportWrite, err.Error())
		return false
	}
	s.markTx()
	return true
}

func (s *session) logWaiting() {
	if s.peerSID != "" {
		return
	}
	s.log("waiting for connection start")
}
