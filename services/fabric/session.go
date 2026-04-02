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

// ---- timeouts (local policy) ----
//
// Timing relationships:
//   staleTimeout (45s) > exportWaitFallback (15s) > callTimeoutDef (5s)
//
// The CM5 sends pings every 15s of TX inactivity. The MCU marks the
// peer stale after 45s without any RX, giving a 30s margin. The
// exportWaitFallback arms exports if no peer traffic arrives within
// 15s of link-up (normally armed earlier by peer_pub).

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
	exportWaitFallback = 15 * time.Second
	errPayloadMarshal  = "payload_marshal_failed"
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

	link          linkState
	remoteNode    string
	peerSID       string
	peerProto     int
	helloSeen     bool
	lastRxAt      time.Time
	lastTxAt      time.Time
	lastPongAt    time.Time
	exportReadyAt time.Time
	exportWaitUntil time.Time
	exportsArmed  bool

	exportSubs       []*bus.Subscription
	exportCallSubs   []*bus.Subscription
	pendingCalls     []*pendingCall
	pendingWireCalls []*pendingWireCall
	nextWireCallID   uint64
}

func (s *session) log(msg string) {
	println("[fabric]", "sid", s.localSID, msg)
}

func (s *session) logKV(msg, key, value string) {
	println("[fabric]", "sid", s.localSID, msg, key, value)
}

type pendingCall struct {
	id       string
	sub      *bus.Subscription
	deadline time.Time
}

type pendingWireCall struct {
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
	RemoteID          string `json:"remote_id,omitempty"`
	PeerProto         int    `json:"peer_proto,omitempty"`
	LastRxUnixMilli   int64  `json:"last_rx_unix_ms,omitempty"`
	LastTxUnixMilli   int64  `json:"last_tx_unix_ms,omitempty"`
	LastPongUnixMilli int64  `json:"last_pong_unix_ms,omitempty"`
	PendingCalls      int    `json:"pending_calls"`
	PendingWireCalls  int    `json:"pending_wire_calls"`
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
	defer s.teardownPendingCalls()
	defer s.teardownPendingWireCalls("link_down")
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

	s.logWaiting()
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
				s.handleLinkDown("transport_down", res.err.Error())
				return
			}
			if s.dispatch(res.line) {
				resetTimer(stale, staleTimeout)
			}

		case <-exportTick.C:
			s.drainExports()
			s.drainPendingCalls(time.Now())
			s.drainWireCalls(time.Now())

		case <-waitTick.C:
			s.logWaiting()

		case <-stale.C:
			if s.link == linkUp {
				s.handleLinkDown("peer_stale", "")
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
		return "ready"
	}
	return "opening"
}

func (s *session) publishLinkState(reason, err string) {
	if s.conn == nil {
		return
	}
	status := s.currentStatus()
	if s.link != linkUp && (reason != "" || err != "") {
		status = "down"
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
			RemoteID:          s.remoteNode,
			PeerProto:         s.peerProto,
			LastRxUnixMilli:   unixMilli(s.lastRxAt),
			LastTxUnixMilli:   unixMilli(s.lastTxAt),
			LastPongUnixMilli: unixMilli(s.lastPongAt),
			PendingCalls:      len(s.pendingCalls),
			PendingWireCalls:  len(s.pendingWireCalls),
			Reason:            reason,
			Err:               err,
		},
		true,
	))
}

func (s *session) noteRx(msgType string) {
	s.lastRxAt = time.Now()
	if msgType == "pong" {
		s.lastPongAt = s.lastRxAt
	}
}

func (s *session) noteTx() {
	s.lastTxAt = time.Now()
}

func (s *session) handleLinkDown(reason, err string) {
	pendingReason := reason
	if pendingReason == "" {
		pendingReason = "link_down"
	}
	s.link = linkDown
	s.remoteNode = ""
	s.peerSID = ""
	s.peerProto = 0
	s.helloSeen = false
	s.exportReadyAt = time.Time{}
	s.exportWaitUntil = time.Time{}
	s.exportsArmed = false
	s.teardownExports()
	s.teardownPendingCalls()
	s.teardownPendingWireCalls(pendingReason)
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
		s.teardownPendingCalls()
		if reason == "" {
			reason = "peer_reset"
		}
		s.teardownPendingWireCalls(reason)
	}
	s.link = linkUp
	s.exportReadyAt = time.Time{}
	s.exportWaitUntil = time.Now().Add(exportWaitFallback)
	s.exportsArmed = false
	s.publishLinkState(reason, "")
}

// ---- dispatch ----

func (s *session) dispatch(line []byte) bool {
	msgType := wireType(line)
	switch msgType {
	case "hello":
		return s.onHello(line)
	case "hello_ack":
		return s.onHelloAck(line)
	case "ping":
		return s.onPing(line)
	case "pong":
		return s.onPong(line)
	case "pub":
		return s.onPub(line)
	case "unretain":
		return s.onUnretain(line)
	case "call":
		return s.onCall(line)
	case "reply":
		return s.onReply(line)
	default:
		if msgType == "" {
			s.log("invalid frame dropped")
		} else {
			s.logKV("unknown frame type dropped", "type", msgType)
		}
		return false
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
		reason = "peer_session_changed"
	}
	if node != "" {
		s.remoteNode = node
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

func (s *session) onHello(line []byte) bool {
	var msg wireHello
	if err := json.Unmarshal(line, &msg); err != nil {
		s.logKV("malformed hello dropped", "err", err.Error())
		return false
	}
	s.noteRx("hello")
	if msg.Peer != "" && msg.Peer != s.nodeID {
		s.log("hello dropped: wrong peer")
		return false
	}
	if s.peerID != "" && msg.Node != s.peerID {
		s.log("hello dropped: wrong node")
		return false
	}
	reason := s.notePeerIdentity(msg.Node, msg.SID, msg.Proto)
	s.helloSeen = true
	s.logKV("hello rx", "peer_sid", msg.SID)

	if !s.writeLine(marshal(wireHelloAck{
		T:     "hello_ack",
		Node:  s.nodeID,
		SID:   s.localSID,
		Proto: protoVersion,
		OK:    true,
	})) {
		return true
	}
	s.log("hello_ack tx")
	time.Sleep(postHelloAckSettle)
	s.promoteLink(reason)
	return true
}

func (s *session) armExports(reason string) {
	if s.link != linkUp || s.exportsArmed {
		return
	}
	s.setupExports()
	s.exportReadyAt = time.Now().Add(exportStartHoldoff)
	s.exportWaitUntil = time.Time{}
	s.exportsArmed = true
	s.logKV("export replay armed", "reason", reason)
}

func (s *session) onHelloAck(line []byte) bool {
	var msg wireHelloAck
	if err := json.Unmarshal(line, &msg); err != nil {
		s.logKV("malformed hello_ack dropped", "err", err.Error())
		return false
	}
	if s.isSelfControlFrame(msg.Node, msg.SID) {
		s.log("echoed hello_ack ignored")
		return true
	}
	s.noteRx("hello_ack")
	if !msg.OK {
		s.log("hello_ack rejected by peer")
		s.handleLinkDown("hello_rejected", "")
		return true
	}
	reason := s.notePeerIdentity(msg.Node, msg.SID, msg.Proto)
	s.helloSeen = true
	s.logKV("hello_ack rx", "peer_sid", msg.SID)
	s.promoteLink(reason)
	return true
}

func (s *session) onPing(line []byte) bool {
	var msg wirePing
	if err := json.Unmarshal(line, &msg); err != nil {
		s.logKV("malformed ping dropped", "err", err.Error())
		return false
	}
	s.noteRx("ping")
	if s.link != linkUp {
		s.log("ping dropped: link not up")
		return true
	}
	reason := s.notePeerIdentity("", msg.SID, 0)
	if reason != "" {
		s.logKV("peer session changed", "reason", reason)
		s.teardownExports()
		s.teardownPendingCalls()
		s.teardownPendingWireCalls(reason)
		s.exportsArmed = false
	}
	s.armExports("peer_ping")
	s.logKV("ping rx", "peer_sid", msg.SID)
	if !s.writeLine(marshal(wirePong{T: "pong", TS: msg.TS, SID: s.localSID})) {
		return true
	}
	s.log("pong tx")
	s.publishLinkState(reason, "")
	return true
}

func (s *session) onPong(line []byte) bool {
	var msg wirePong
	if err := json.Unmarshal(line, &msg); err != nil {
		s.logKV("malformed pong dropped", "err", err.Error())
		return false
	}
	if s.isSelfControlFrame("", msg.SID) {
		s.log("echoed pong ignored")
		return true
	}
	s.noteRx("pong")
	reason := s.notePeerIdentity("", msg.SID, 0)
	if reason != "" {
		s.logKV("peer session changed", "reason", reason)
		s.teardownExports()
		s.teardownPendingCalls()
		s.teardownPendingWireCalls(reason)
		s.exportsArmed = false
	}
	s.armExports("peer_pong")
	s.publishLinkState(reason, "")
	return true
}

func (s *session) onPub(line []byte) bool {
	var msg wirePub
	if err := json.Unmarshal(line, &msg); err != nil {
		s.logKV("malformed pub dropped", "err", err.Error())
		return false
	}
	s.noteRx("pub")
	if s.link != linkUp {
		s.log("pub dropped before handshake")
		return true
	}
	t := importPublishTopic(msg.Topic)
	if t == nil {
		if hasWirePrefix(msg.Topic, []string{"state"}) {
			s.log("echoed state pub ignored")
			return true
		}
		s.log("incoming pub dropped: no_route")
		return true
	}
	s.armExports("peer_pub")
	s.conn.Publish(s.conn.NewMessage(t, msg.Payload, msg.Retain))
	return true
}

func (s *session) onUnretain(line []byte) bool {
	var msg wireUnretain
	if err := json.Unmarshal(line, &msg); err != nil {
		s.logKV("malformed unretain dropped", "err", err.Error())
		return false
	}
	s.noteRx("unretain")
	if s.link != linkUp {
		s.log("unretain dropped before handshake")
		return true
	}
	t := importPublishTopic(msg.Topic)
	if t == nil {
		s.log("incoming unretain dropped: no_route")
		return true
	}
	s.armExports("peer_unretain")
	s.conn.Publish(s.conn.NewMessage(t, nil, true))
	return true
}

func (s *session) onCall(line []byte) bool {
	var msg wireCall
	if err := json.Unmarshal(line, &msg); err != nil {
		s.logKV("malformed call dropped", "err", err.Error())
		return false
	}
	s.noteRx("call")
	if s.link != linkUp {
		s.log("call dropped before handshake")
		return true
	}
	t := importCallTopic(msg.Topic)
	if t == nil {
		s.log("incoming call dropped: no_route")
		s.writeLine(marshal(wireReply{T: "reply", Corr: msg.ID, OK: false, Err: "no_route"}))
		return true
	}
	s.armExports("peer_call")

	timeout := callTimeoutDef
	if msg.TimeoutMs > 0 {
		timeout = time.Duration(msg.TimeoutMs) * time.Millisecond
	}
	busMsg := s.conn.NewMessage(t, msg.Payload, false)
	sub := s.conn.Request(busMsg)
	s.pendingCalls = append(s.pendingCalls, &pendingCall{
		id:       msg.ID,
		sub:      sub,
		deadline: time.Now().Add(timeout),
	})
	return true
}

func (s *session) onReply(line []byte) bool {
	var msg wireReply
	if err := json.Unmarshal(line, &msg); err != nil {
		s.logKV("malformed reply dropped", "err", err.Error())
		return false
	}
	s.noteRx("reply")
	s.armExports("peer_reply")

	for i, call := range s.pendingWireCalls {
		if call.id != msg.Corr {
			continue
		}
		s.pendingWireCalls = append(s.pendingWireCalls[:i], s.pendingWireCalls[i+1:]...)
		if !call.req.CanReply() {
			return true
		}
		if !msg.OK {
			s.conn.Reply(call.req, types.ErrorReply{OK: false, Error: msg.Err}, false)
			return true
		}
		s.conn.Reply(call.req, decodePayload(msg.Payload), false)
		return true
	}

	s.logKV("unexpected reply dropped", "corr", msg.Corr)
	return true
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

func (s *session) teardownPendingCalls() {
	for _, call := range s.pendingCalls {
		s.conn.Unsubscribe(call.sub)
	}
	s.pendingCalls = nil
}

func (s *session) teardownPendingWireCalls(reason string) {
	for _, call := range s.pendingWireCalls {
		if call.req != nil && call.req.CanReply() {
			s.conn.Reply(call.req, types.ErrorReply{OK: false, Error: reason}, false)
		}
	}
	s.pendingWireCalls = nil
}

// drainExports does a non-blocking read of each export subscription
// and writes any messages to the wire. Called from the main loop.
func (s *session) drainExports() {
	if s.link != linkUp {
		return
	}
	if !s.exportsArmed {
		if !s.exportWaitUntil.IsZero() && !time.Now().Before(s.exportWaitUntil) {
			s.armExports("fallback")
		} else {
			return
		}
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
				payload, err := marshalPayload(m.Payload)
				if err != nil {
					s.logKV("export payload dropped", "err", err.Error())
					continue
				}
				if !s.writeLine(marshal(wirePub{
					T:       "pub",
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

func (s *session) drainPendingCalls(now time.Time) {
	if len(s.pendingCalls) == 0 {
		return
	}

	keep := s.pendingCalls[:0]
	for _, call := range s.pendingCalls {
		select {
		case reply, ok := <-call.sub.Channel():
			s.conn.Unsubscribe(call.sub)
			if !ok || reply == nil {
				if !s.writeLine(marshal(wireReply{T: "reply", Corr: call.id, OK: false, Err: "timeout"})) {
					return
				}
				continue
			}
			if errStr := checkBusError(reply.Payload); errStr != "" {
				if !s.writeLine(marshal(wireReply{T: "reply", Corr: call.id, OK: false, Err: errStr})) {
					return
				}
				continue
			}
			payload, err := marshalPayload(reply.Payload)
			if err != nil {
				if !s.writeLine(marshal(wireReply{T: "reply", Corr: call.id, OK: false, Err: errPayloadMarshal})) {
					return
				}
				continue
			}
			if !s.writeLine(marshal(wireReply{T: "reply", Corr: call.id, OK: true, Payload: payload})) {
				return
			}
			continue
		default:
		}

		if !now.Before(call.deadline) {
			s.conn.Unsubscribe(call.sub)
			if !s.writeLine(marshal(wireReply{T: "reply", Corr: call.id, OK: false, Err: "timeout"})) {
				return
			}
			continue
		}

		keep = append(keep, call)
	}

	s.pendingCalls = keep
}

func (s *session) drainWireCalls(now time.Time) {
	s.drainOutgoingWireCalls(now)
	s.drainPendingWireCalls(now)
}

func (s *session) drainOutgoingWireCalls(now time.Time) {
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
				id := s.nextWireCallID
				s.nextWireCallID++
				corr := "wire-" + strconvx.Utoa64(id)
				if msg.CanReply() {
					s.pendingWireCalls = append(s.pendingWireCalls, &pendingWireCall{
						id:       corr,
						req:      msg,
						deadline: now.Add(callTimeoutDef),
					})
				}
				if !s.writeLine(marshal(wireCall{
					T:         "call",
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

func (s *session) drainPendingWireCalls(now time.Time) {
	if len(s.pendingWireCalls) == 0 {
		return
	}

	keep := s.pendingWireCalls[:0]
	for _, call := range s.pendingWireCalls {
		if !now.Before(call.deadline) {
			if call.req != nil && call.req.CanReply() {
				s.conn.Reply(call.req, types.ErrorReply{OK: false, Error: "timeout"}, false)
			}
			continue
		}
		keep = append(keep, call)
	}
	s.pendingWireCalls = keep
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
		s.handleLinkDown("transport_write_failed", err.Error())
		return false
	}
	s.noteTx()
	return true
}

func (s *session) logWaiting() {
	if s.helloSeen {
		return
	}
	s.log("waiting for connection start")
}
