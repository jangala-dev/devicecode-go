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
	exportMaxPerTick  = 1
	errPayloadMarshal = "payload_marshal_failed"
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

	exportSubs     []*bus.Subscription
	exportCallSubs []*bus.Subscription
	inboundCalls   []*inboundCall
	outboundCalls  []*outboundCall
	nextOutboundID uint64
}

func (s *session) log(msg string) {
	println("[fabric]", "sid", s.localSID, msg)
}

func (s *session) logKV(msg, key, value string) {
	println("[fabric]", "sid", s.localSID, msg, key, value)
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

// run is the main loop. Blocks until ctx is cancelled.
func (s *session) run(ctx context.Context) {
	lines := make(chan readResult, 1)

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
	defer s.log("run stop")

	stale := time.NewTimer(staleTimeout)
	defer stale.Stop()

	waitTick := time.NewTicker(waitLogEvery)
	defer waitTick.Stop()

	// Poll subscription channels periodically. Needed because select
	// blocks until a line/timer fires; without this, exported bus
	// messages and async call replies would sit in subscription channels.
	exportTick := time.NewTicker(50 * time.Millisecond)
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
		s.teardownExports()
		s.teardownInbound()
		if reason == "" {
			reason = reasonPeerReset
		}
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

// validateInbound checks whether a message should be processed.
// Handshake messages (hello, hello_ack) are always accepted.
// All others require an established link and a matching session ID.
func (s *session) validateInbound(msg *wireMsg) bool {
	if msg.T == msgHello || msg.T == msgHelloAck {
		return true
	}
	if s.link != linkUp {
		s.logKV("dropped before handshake", "type", msg.T)
		return false
	}
	if s.peerSID != "" && msg.SID != "" && msg.SID != s.peerSID {
		s.logKV("dropped: wrong session", "type", msg.T)
		return false
	}
	return true
}

func (s *session) dispatch(line []byte) {
	var msg wireMsg
	if err := json.Unmarshal(line, &msg); err != nil {
		s.logKV("malformed frame dropped", "err", err.Error())
		return
	}
	s.markRx()
	if !s.validateInbound(&msg) {
		return
	}
	switch msg.T {
	case msgHello:
		s.onHello(&msg)
	case msgHelloAck:
		s.onHelloAck(&msg)
	case msgPing:
		s.onPing(&msg)
	case msgPong:
		s.onPong(&msg)
	case msgPub:
		s.onPub(&msg)
	case msgUnretain:
		s.onUnretain(&msg)
	case msgCall:
		s.onCall(&msg)
	case msgReply:
		s.onReply(&msg)
	default:
		s.logKV("unknown message type dropped", "type", msg.T)
	}
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

func (s *session) onHello(msg *wireMsg) {
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

	if !s.writeLine(marshal(wireHelloAck{
		T:     msgHelloAck,
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

func (s *session) onHelloAck(msg *wireMsg) {
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

func (s *session) onPing(msg *wireMsg) {
	s.logKV("ping rx", "peer_sid", msg.SID)
	if !s.writeLine(marshal(wirePong{T: msgPong, TS: msg.TS, SID: s.localSID})) {
		return
	}
	s.log("pong tx")
}

func (s *session) onPong(msg *wireMsg) {
	if s.isSelfControlFrame("", msg.SID) {
		s.log("echoed pong ignored")
		return
	}
	s.lastPongAt = s.lastRxAt
}

func (s *session) onPub(msg *wireMsg) {
	t := importPublishTopic(msg.Topic)
	if t == nil {
		if hasWirePrefix(msg.Topic, []string{"state"}) {
			s.log("echoed state pub ignored")
			return
		}
		s.log("incoming pub dropped: no_route")
		return
	}
	s.conn.Publish(s.conn.NewMessage(t, msg.Payload, msg.Retain))
}

func (s *session) onUnretain(msg *wireMsg) {
	t := importPublishTopic(msg.Topic)
	if t == nil {
		s.log("incoming unretain dropped: no_route")
		return
	}
	s.conn.Publish(s.conn.NewMessage(t, nil, true))
}

func (s *session) onCall(msg *wireMsg) {
	t := importCallTopic(msg.Topic)
	if t == nil {
		s.log("incoming call dropped: no_route")
		s.writeLine(marshal(wireReply{T: msgReply, Corr: msg.ID, OK: false, Err: reasonNoRoute}))
		return
	}

	timeout := callTimeoutDef
	if msg.TimeoutMs > 0 {
		timeout = time.Duration(msg.TimeoutMs) * time.Millisecond
	}
	busMsg := s.conn.NewMessage(t, msg.Payload, false)
	sub := s.conn.Request(busMsg)
	s.inboundCalls = append(s.inboundCalls, &inboundCall{
		id:       msg.ID,
		sub:      sub,
		deadline: time.Now().Add(timeout),
	})
}

func (s *session) onReply(msg *wireMsg) {

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
		s.conn.Reply(call.req, decodePayload(msg.Payload), false)
		return
	}

	s.logKV("unexpected reply dropped", "corr", msg.Corr)
}

func checkBusError(payload any) string {
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
					if !s.writeLine(marshal(wireUnretain{
						T:     msgUnretain,
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
				if !s.writeLine(marshal(wirePub{
					T:       msgPub,
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
				if !s.writeLine(marshal(wireReply{T: msgReply, Corr: call.id, OK: false, Err: reasonTimeout})) {
					return
				}
				continue
			}
			if errStr := checkBusError(reply.Payload); errStr != "" {
				if !s.writeLine(marshal(wireReply{T: msgReply, Corr: call.id, OK: false, Err: errStr})) {
					return
				}
				continue
			}
			payload, err := marshalPayload(reply.Payload)
			if err != nil {
				if !s.writeLine(marshal(wireReply{T: msgReply, Corr: call.id, OK: false, Err: errPayloadMarshal})) {
					return
				}
				continue
			}
			if !s.writeLine(marshal(wireReply{T: msgReply, Corr: call.id, OK: true, Payload: payload})) {
				return
			}
			continue
		default:
		}

		if !now.Before(call.deadline) {
			s.conn.Unsubscribe(call.sub)
			call.sub = nil
			if !s.writeLine(marshal(wireReply{T: msgReply, Corr: call.id, OK: false, Err: reasonTimeout})) {
				return
			}
			continue
		}

		keep = append(keep, call)
	}

	s.inboundCalls = keep
}

func (s *session) drainOutbound(now time.Time) {
	s.drainOutboundNew(now)
	s.drainOutboundPending(now)
}

func (s *session) drainOutboundNew(now time.Time) {
	if s.link != linkUp || len(s.exportCallSubs) == 0 {
		return
	}

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
				if !s.writeLine(marshal(wireCall{
					T:         msgCall,
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

func (s *session) drainOutboundPending(now time.Time) {
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

// ---- transport write ----

func (s *session) writeLine(data []byte) bool {
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	if err := s.tr.WriteLine(data); err != nil {
		if errors.Is(err, ErrLineTooLong) {
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
