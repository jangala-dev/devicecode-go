// bus.go
package bus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

var defaultQLen = 3

// -----------------------------------------------------------------------------
// Tokens + Topics
// -----------------------------------------------------------------------------

// Token can be string or int (or any comparable type you choose to use as a key).
type Token any

// topic is the internal canonical representation (a slice of tokens).
type topic []Token

// Topic is the exported, sealed topic handle. Only this package can implement it.
type Topic interface {
	isBusTopic() // unexported method seals the interface
	Append(tokens ...Token) Topic

	Len() int
	At(i int) Token
}

func (t topic) Len() int       { return len(t) }
func (t topic) At(i int) Token { return t[i] }

// ---- topic interner

type internNode struct {
	children map[Token]*internNode
	topic    topic // canonical slice for this exact path (nil if non-terminal)
}

var interner struct {
	mu   sync.Mutex
	root *internNode
	// (optional) soft cap; set >0 to stop growing after N distinct topics
	maxTopics int
	count     int
}

func init() {
	interner.root = &internNode{children: make(map[Token]*internNode)}
	interner.maxTopics = 0 // 0 = unlimited; you can tune if you want a cap
}

// Seals `topic` as implementing `Topic`.
func (t topic) isBusTopic() {}

func internTopic(tokens ...Token) topic {
	n := interner.root
	// single critical section keeps it simple and TinyGo-friendly
	interner.mu.Lock()
	defer interner.mu.Unlock()

	for _, t := range tokens {
		if n.children == nil {
			n.children = make(map[Token]*internNode)
		}
		child := n.children[t]
		if child == nil {
			// respect cap if configured: stop growing the trie, fall back to fresh slice
			if interner.maxTopics > 0 && interner.count >= interner.maxTopics {
				// return a fresh independent slice
				cp := make(topic, len(tokens))
				copy(cp, tokens)
				return cp
			}
			child = &internNode{}
			n.children[t] = child
		}
		n = child
	}
	if n.topic != nil {
		return n.topic
	}
	// create canonical slice for this exact sequence
	cp := make(topic, len(tokens))
	copy(cp, tokens)
	n.topic = cp
	interner.count++
	return cp
}

// ---- topic creation functions

func validateTokens(tokens ...Token) {
	for _, tok := range tokens {
		switch tok.(type) {
		case string,
			int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64,
			uintptr:
			// ok
		default:
			panic("bus: token type is not allowed/comparable")
		}
	}
}

// T validates and interns a topic, returning an opaque Topic.
func T(tokens ...Token) Topic {
	validateTokens(tokens...)
	return internTopic(tokens...)
}

// TNoIntern validates but DOES NOT intern the tokens.
// Intended for short-lived subjects (e.g. per-request replies).
func TNoIntern(tokens ...Token) Topic {
	validateTokens(tokens...)
	cp := make(topic, len(tokens))
	copy(cp, tokens)
	return cp
}

// Append validates and interns t + tokens, returning a canonical Topic.
// It never aliases the caller’s storage; you always get an interned slice.
func (t topic) Append(tokens ...Token) Topic {
	validateTokens(tokens...)
	combined := make([]Token, 0, len(t)+len(tokens))
	combined = append(combined, t...)
	combined = append(combined, tokens...)
	return internTopic(combined...)
}

// Helpers to work with opaque Topic inside the package.
func toConcrete(tp Topic) topic {
	if tp == nil {
		return nil
	}
	return tp.(topic)
}

func topicLen(tp Topic) int {
	return len(toConcrete(tp))
}

// -----------------------------------------------------------------------------
// Message
// -----------------------------------------------------------------------------

type Message struct {
	Topic    Topic
	Payload  any
	Retained bool
	ReplyTo  Topic
}

func (m *Message) CanReply() bool { return topicLen(m.ReplyTo) != 0 }

// -----------------------------------------------------------------------------
// Subscription
// -----------------------------------------------------------------------------

type Subscription struct {
	topic topic
	ch    chan Message
	bus   *Bus
	conn  *Connection
	set   *SubscriptionSet

	// lastDeliveryGen is used while publishing to avoid delivering the same
	// message twice to one subscription when wildcard traversal reaches it more
	// than once. It is only read and written while Bus.mu is held.
	lastDeliveryGen uint64
}

func (s *Subscription) Topic() Topic            { return s.topic }
func (s *Subscription) Channel() <-chan Message { return s.ch }
func (s *Subscription) Unsubscribe()            { s.conn.Unsubscribe(s) }

// Convenience wrapper that replies via the owning connection.
func (s *Subscription) Reply(to *Message, payload any, retained bool) {
	s.conn.Reply(to, payload, retained)
}

// -----------------------------------------------------------------------------
// SubscriptionSet
// -----------------------------------------------------------------------------

// SubscriptionSet lets a reactor wait on one readiness channel for several
// subscriptions without spawning a goroutine per subscription. Readiness is
// coalesced: once Ready() is signalled, callers should drain the subscriptions
// they own until no immediate work remains.
type SubscriptionSet struct {
	conn            *Connection
	ready           chan struct{}
	mu              sync.Mutex
	subs            []*Subscription
	retainedWatches []*RetainedWatch
	closed          bool
}

func (c *Connection) NewSubscriptionSet() *SubscriptionSet {
	return &SubscriptionSet{
		conn:  c,
		ready: make(chan struct{}, 1),
	}
}

func (ss *SubscriptionSet) Ready() <-chan struct{} {
	if ss == nil {
		return nil
	}
	return ss.ready
}

func (ss *SubscriptionSet) Subscribe(tp Topic) *Subscription {
	return ss.SubscribeBuffered(tp, 0)
}

// SubscribeBuffered subscribes with a per-subscription channel size.
//
// Most MCU bus subscriptions deliberately use the bus default queue length,
// often very small, so producers remain bounded. Fabric export subscriptions
// are the main exception: a wildcard state/self/# subscription may receive a
// retained replay containing many already-published facts at subscription time.
// Giving that subscription a larger bounded queue prevents the initial replay
// from dropping older retained facts before Fabric has a chance to coalesce and
// send them.
func (ss *SubscriptionSet) SubscribeBuffered(tp Topic, queueLen int) *Subscription {
	if ss == nil || ss.conn == nil {
		return nil
	}
	if queueLen <= 0 {
		queueLen = ss.conn.bus.qLen
	}
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return nil
	}
	ss.mu.Unlock()
	ct := toConcrete(tp)
	sub := &Subscription{topic: ct, ch: make(chan Message, queueLen), bus: ss.conn.bus, conn: ss.conn, set: ss}
	ss.conn.bus.addSubscription(ct, sub)
	ss.conn.mu.Lock()
	ss.conn.subs = append(ss.conn.subs, sub)
	ss.conn.mu.Unlock()
	ss.mu.Lock()
	if !ss.closed {
		ss.subs = append(ss.subs, sub)
	} else {
		sub.set = nil
	}
	ss.mu.Unlock()
	if sub.set == nil {
		ss.conn.Unsubscribe(sub)
		return nil
	}
	return sub
}

func (ss *SubscriptionSet) WatchRetained(tp Topic, opts RetainedWatchOptions) *RetainedWatch {
	if ss == nil || ss.conn == nil {
		return nil
	}
	if opts.QueueLen <= 0 {
		opts.QueueLen = ss.conn.bus.qLen
	}
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return nil
	}
	ss.mu.Unlock()
	ct := toConcrete(tp)
	w := &RetainedWatch{pattern: ct, bus: ss.conn.bus, conn: ss.conn, set: ss, ready: make(chan struct{}, 1), q: make([]RetainedEvent, opts.QueueLen)}
	ss.conn.bus.addRetainedWatch(ct, w, opts.Replay)
	ss.mu.Lock()
	if !ss.closed {
		ss.retainedWatches = append(ss.retainedWatches, w)
	} else {
		w.set = nil
	}
	ss.mu.Unlock()
	if w.set == nil {
		ss.conn.UnwatchRetained(w)
		return nil
	}
	return w
}

func (ss *SubscriptionSet) removeRetainedWatch(w *RetainedWatch) {
	if ss == nil || w == nil {
		return
	}
	ss.mu.Lock()
	for i, existing := range ss.retainedWatches {
		if existing == w {
			ss.retainedWatches = append(ss.retainedWatches[:i], ss.retainedWatches[i+1:]...)
			break
		}
	}
	ss.mu.Unlock()
}

func (ss *SubscriptionSet) Request(msg *Message) *Subscription {
	if ss == nil || ss.conn == nil {
		return nil
	}
	if topicLen(msg.ReplyTo) == 0 {
		msg.ReplyTo = TNoIntern("_rr", ss.conn.rrCtr.Add(1))
	}
	sub := ss.Subscribe(msg.ReplyTo)
	ss.conn.Publish(msg)
	return sub
}

func (ss *SubscriptionSet) Unsubscribe(sub *Subscription) {
	if ss == nil || sub == nil || ss.conn == nil {
		return
	}
	ss.conn.Unsubscribe(sub)
}

func (ss *SubscriptionSet) Close() {
	if ss == nil || ss.conn == nil {
		return
	}
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return
	}
	subs := append([]*Subscription(nil), ss.subs...)
	watches := append([]*RetainedWatch(nil), ss.retainedWatches...)
	ss.subs = nil
	ss.retainedWatches = nil
	ss.closed = true
	close(ss.ready)
	ss.mu.Unlock()
	for _, sub := range subs {
		ss.conn.Unsubscribe(sub)
	}
	for _, w := range watches {
		ss.conn.UnwatchRetained(w)
	}
}

func (ss *SubscriptionSet) remove(sub *Subscription) {
	if ss == nil || sub == nil {
		return
	}
	ss.mu.Lock()
	ss.subs = removeSub(ss.subs, sub)
	ss.mu.Unlock()
}

func (ss *SubscriptionSet) signal() {
	if ss == nil {
		return
	}
	ss.mu.Lock()
	ready := ss.ready
	closed := ss.closed
	ss.mu.Unlock()
	if closed || ready == nil {
		return
	}
	defer func() { _ = recover() }()
	select {
	case ready <- struct{}{}:
	default:
	}
}

func (s *Subscription) signalReady() {
	if s != nil && s.set != nil {
		s.set.signal()
	}
}

// -----------------------------------------------------------------------------
// Retained watch
// -----------------------------------------------------------------------------

// RetainedOp names the retained-state lifecycle operation delivered to a
// retained watch. The names mirror lua-bus terminology: retained watchers see
// retain/unretain events plus an optional replay_done marker.
type RetainedOp uint8

const (
	RetainedReplayDone RetainedOp = iota
	RetainedSet
	RetainedUnset
)

func (op RetainedOp) String() string {
	switch op {
	case RetainedSet:
		return "retain"
	case RetainedUnset:
		return "unretain"
	case RetainedReplayDone:
		return "replay_done"
	default:
		return "unknown"
	}
}

type RetainedWatchOptions struct {
	Replay   bool
	QueueLen int
}

type RetainedEvent struct {
	Op      RetainedOp
	Topic   Topic
	Payload any
}

type RetainedWatch struct {
	pattern topic
	bus     *Bus
	conn    *Connection
	set     *SubscriptionSet
	ready   chan struct{}
	q       []RetainedEvent
	head    int
	len     int
	dropped uint32
	closed  bool

	// lastDeliveryGen is used while delivering retained lifecycle events to
	// avoid duplicate delivery when wildcard traversal reaches this watch more
	// than once. It is only accessed while Bus.mu is held.
	lastDeliveryGen uint64
}

func (w *RetainedWatch) Ready() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.ready
}

func (w *RetainedWatch) Dropped() uint32 {
	if w == nil {
		return 0
	}
	return w.dropped
}

func (w *RetainedWatch) Close() { w.conn.UnwatchRetained(w) }

func (w *RetainedWatch) signalReady() {
	if w == nil {
		return
	}
	if w.set != nil {
		w.set.signal()
		return
	}
	select {
	case w.ready <- struct{}{}:
	default:
	}
}

func (w *RetainedWatch) push(ev RetainedEvent) {
	if len(w.q) == 0 || w.closed {
		return
	}
	if w.len == len(w.q) {
		w.q[w.head] = RetainedEvent{}
		w.head = (w.head + 1) % len(w.q)
		w.len--
		w.dropped++
	}
	idx := (w.head + w.len) % len(w.q)
	w.q[idx] = ev
	w.len++
	w.signalReady()
}

func (w *RetainedWatch) TryNext() (RetainedEvent, bool) {
	if w == nil {
		return RetainedEvent{}, false
	}
	w.bus.mu.Lock()
	defer w.bus.mu.Unlock()
	if w.len == 0 {
		return RetainedEvent{}, false
	}
	ev := w.q[w.head]
	w.q[w.head] = RetainedEvent{}
	w.head = (w.head + 1) % len(w.q)
	w.len--
	if w.len == 0 {
		w.head = 0
	}
	return ev, true
}

// -----------------------------------------------------------------------------
// Trie node (shared for subscribers and retained messages)
// -----------------------------------------------------------------------------

type node struct {
	children        map[Token]*node
	subs            []*Subscription
	retainedWatches []*RetainedWatch
	bindings        []*Binding
	retained        Message // Message.Topic is opaque; internal traversal uses stored path
	retainedSet     bool
}

func ensureChild(n *node, t Token) *node {
	if n.children == nil {
		n.children = make(map[Token]*node)
	}
	if n.children[t] == nil {
		n.children[t] = &node{}
	}
	return n.children[t]
}

// -----------------------------------------------------------------------------
// Bus
// -----------------------------------------------------------------------------

type Options struct {
	QueueLen       int
	SingleWildcard Token
	MultiWildcard  Token
}

type Bus struct {
	mu    sync.Mutex
	root  *node
	qLen  int
	sWild Token
	mWild Token

	// deliveryGen increments once per publish. Matching traversal delivers
	// directly to subscribers and uses this marker to deduplicate without
	// allocating a per-publish subscriber slice or map.
	deliveryGen uint64

	// retainedDeliveryGen increments for retained lifecycle delivery and is
	// used to deduplicate wildcard retained watches without allocating a map.
	retainedDeliveryGen uint64
}

func NewBus(queueLen int, singleWild, multiWild Token) *Bus {
	if queueLen <= 0 || singleWild == nil || multiWild == nil {
		panic("bus: Options must fully specify QueueLen>0 and wildcards")
	}
	return &Bus{
		root:  &node{},
		qLen:  queueLen,
		sWild: singleWild,
		mWild: multiWild,
	}
}

func (b *Bus) NewMessage(tp Topic, payload any, retained bool) *Message {
	return &Message{
		Topic:    tp,
		Payload:  payload,
		Retained: retained,
	}
}

func (b *Bus) addSubscription(tp topic, sub *Subscription) {
	b.mu.Lock()
	n := b.root
	for _, t := range tp {
		n = ensureChild(n, t)
	}
	n.subs = append(n.subs, sub)

	// Replay retained messages directly while walking the retained trie. This
	// avoids allocating a temporary retained-message slice during subscription,
	// which is important for Fabric wildcard exports on the MCU.
	b.deliverRetainedLocked(b.root, tp, 0, sub)
	b.mu.Unlock()
}

func (b *Bus) nextDeliveryGenLocked() uint64 {
	b.deliveryGen++
	if b.deliveryGen == 0 {
		// Practically unreachable with uint64, but keep zero as the never-used
		// marker so freshly allocated subscriptions are not accidentally skipped.
		b.deliveryGen = 1
	}
	return b.deliveryGen
}

func (b *Bus) PublishValue(tp Topic, payload any, retained bool) {
	msgTopic := toConcrete(tp)

	b.mu.Lock()
	if retained {
		if payload == nil {
			b.retainDeleteLocked(msgTopic)
			b.deliverRetainedWatchersLocked(msgTopic, RetainedEvent{Op: RetainedUnset, Topic: tp})
		} else {
			b.retainSetValueLocked(msgTopic, tp, payload)
			b.deliverRetainedWatchersLocked(msgTopic, RetainedEvent{Op: RetainedSet, Topic: tp, Payload: payload})
		}
	}
	if !b.hasSubscribersLocked(b.root, msgTopic, 0) {
		b.mu.Unlock()
		return
	}
	msg := Message{Topic: tp, Payload: payload, Retained: retained}
	gen := b.nextDeliveryGenLocked()
	b.deliverSubscribersLocked(b.root, msgTopic, 0, msg, gen)
	b.mu.Unlock()
}

func (b *Bus) Publish(msg *Message) { b.publish(msg) }

func (b *Bus) publish(msg *Message) {
	msgTopic := toConcrete(msg.Topic)

	b.mu.Lock()
	if msg.Retained {
		if msg.Payload == nil {
			b.retainDeleteLocked(msgTopic)
			b.deliverRetainedWatchersLocked(msgTopic, RetainedEvent{Op: RetainedUnset, Topic: msg.Topic})
		} else {
			b.retainSetLocked(msgTopic, msg)
			b.deliverRetainedWatchersLocked(msgTopic, RetainedEvent{Op: RetainedSet, Topic: msg.Topic, Payload: msg.Payload})
		}
	}

	gen := b.nextDeliveryGenLocked()
	b.deliverSubscribersLocked(b.root, msgTopic, 0, *msg, gen)
	b.mu.Unlock()
}

func trySend(ch chan Message, m Message) bool {
	select {
	case ch <- m:
		return true
	default:
		return false
	}
}

func drainOne(ch chan Message) {
	select {
	case <-ch:
	default:
	}
}

func (b *Bus) tryDeliver(sub *Subscription, msg Message) {
	defer func() { _ = recover() }() // channel may be closed; best-effort
	if trySend(sub.ch, msg) {
		sub.signalReady()
		return
	}
	drainOne(sub.ch)
	_ = trySend(sub.ch, msg)
	sub.signalReady()
}

// -----------------------------------------------------------------------------
// Unsubscribe + pruning
// -----------------------------------------------------------------------------

func (b *Bus) unsubscribe(tp topic, sub *Subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := b.root
	var stack []*node
	for _, t := range tp {
		if n.children == nil {
			return
		}
		child := n.children[t]
		if child == nil {
			return
		}
		stack = append(stack, n)
		n = child
	}

	for i, s := range n.subs {
		if s == sub {
			n.subs = append(n.subs[:i], n.subs[i+1:]...)
			break
		}
	}
	b.pruneEmptyLocked(stack, tp)
}

func (b *Bus) pruneEmptyLocked(stack []*node, path []Token) {
	for i := len(path) - 1; i >= 0; i-- {
		parent := stack[i]
		key := path[i]
		child := parent.children[key]
		if child != nil && len(child.subs) == 0 && len(child.retainedWatches) == 0 && len(child.bindings) == 0 && len(child.children) == 0 && !child.retainedSet {
			delete(parent.children, key)
		} else {
			break
		}
	}
}

// -----------------------------------------------------------------------------
// Subscriber delivery (topic = concrete message topic)
// -----------------------------------------------------------------------------

func (b *Bus) deliverSubLocked(sub *Subscription, msg Message, gen uint64) {
	if sub == nil {
		return
	}
	if sub.lastDeliveryGen == gen {
		return
	}
	sub.lastDeliveryGen = gen
	b.tryDeliver(sub, msg)
}

func (b *Bus) deliverSubsLocked(subs []*Subscription, msg Message, gen uint64) {
	for _, sub := range subs {
		b.deliverSubLocked(sub, msg, gen)
	}
}

func (b *Bus) hasSubscribersLocked(n *node, tp topic, depth int) bool {
	if n == nil {
		return false
	}
	if depth == len(tp) {
		if len(n.subs) > 0 {
			return true
		}
		if n.children != nil {
			if mw := n.children[b.mWild]; mw != nil && len(mw.subs) > 0 {
				return true
			}
		}
		return false
	}
	tok := tp[depth]
	if n.children != nil {
		if child := n.children[tok]; child != nil && b.hasSubscribersLocked(child, tp, depth+1) {
			return true
		}
		if sw := n.children[b.sWild]; sw != nil && b.hasSubscribersLocked(sw, tp, depth+1) {
			return true
		}
		if mw := n.children[b.mWild]; mw != nil && len(mw.subs) > 0 {
			return true
		}
	}
	return false
}

func (b *Bus) deliverSubscribersLocked(n *node, tp topic, depth int, msg Message, gen uint64) {
	if n == nil {
		return
	}
	if depth == len(tp) {
		b.deliverSubsLocked(n.subs, msg, gen)
		if n.children != nil {
			if mw := n.children[b.mWild]; mw != nil {
				b.deliverSubsLocked(mw.subs, msg, gen) // '#' matches zero additional tokens
			}
		}
		return
	}
	tok := tp[depth]
	if n.children != nil {
		if child := n.children[tok]; child != nil {
			b.deliverSubscribersLocked(child, tp, depth+1, msg, gen)
		}
		if sw := n.children[b.sWild]; sw != nil {
			b.deliverSubscribersLocked(sw, tp, depth+1, msg, gen)
		}
		if mw := n.children[b.mWild]; mw != nil {
			b.deliverSubsLocked(mw.subs, msg, gen) // '#' matches any remainder
		}
	}
}

// -----------------------------------------------------------------------------
// Retained storage and collection (pattern = subscription topic with wildcards)
// -----------------------------------------------------------------------------

func (b *Bus) retainSetLocked(tp topic, msg *Message) {
	b.retainSetValueLocked(tp, msg.Topic, msg.Payload)
}

func (b *Bus) retainSetValueLocked(tp topic, topicValue Topic, payload any) {
	n := b.root
	for _, t := range tp {
		n = ensureChild(n, t)
	}
	n.retained = Message{Topic: topicValue, Payload: payload, Retained: true}
	n.retainedSet = true
}

func (b *Bus) retainDeleteLocked(tp topic) {
	n := b.root
	var stack []*node
	for _, t := range tp {
		if n.children == nil {
			return
		}
		child := n.children[t]
		if child == nil {
			return
		}
		stack = append(stack, n)
		n = child
	}
	n.retained = Message{}
	n.retainedSet = false
	b.pruneEmptyLocked(stack, tp)
}

func (b *Bus) deliverRetainedLocked(n *node, pattern topic, depth int, sub *Subscription) {
	if n == nil {
		return
	}
	if depth == len(pattern) {
		if n.retainedSet {
			b.tryDeliver(sub, n.retained)
		}
		return
	}
	ptok := pattern[depth]
	switch ptok {
	case b.mWild:
		b.deliverAllRetainedLocked(n, sub) // '#' consumes the rest (incl. zero)
	case b.sWild:
		for _, child := range n.children {
			b.deliverRetainedLocked(child, pattern, depth+1, sub)
		}
	default:
		if child := n.children[ptok]; child != nil {
			b.deliverRetainedLocked(child, pattern, depth+1, sub)
		}
	}
}

func (b *Bus) deliverAllRetainedLocked(n *node, sub *Subscription) {
	if n == nil {
		return
	}
	if n.retainedSet {
		b.tryDeliver(sub, n.retained)
	}
	for _, child := range n.children {
		b.deliverAllRetainedLocked(child, sub)
	}
}

func (b *Bus) nextRetainedDeliveryGenLocked() uint64 {
	b.retainedDeliveryGen++
	if b.retainedDeliveryGen == 0 {
		b.retainedDeliveryGen = 1
	}
	return b.retainedDeliveryGen
}

func (b *Bus) addRetainedWatch(pattern topic, w *RetainedWatch, replay bool) {
	b.mu.Lock()
	n := b.root
	for _, t := range pattern {
		n = ensureChild(n, t)
	}
	n.retainedWatches = append(n.retainedWatches, w)
	if replay {
		b.replayRetainedToWatchLocked(b.root, pattern, 0, w)
	}
	w.push(RetainedEvent{Op: RetainedReplayDone})
	b.mu.Unlock()
}

func (b *Bus) removeRetainedWatch(pattern topic, w *RetainedWatch) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.root
	var stack []*node
	for _, t := range pattern {
		if n.children == nil {
			return
		}
		child := n.children[t]
		if child == nil {
			return
		}
		stack = append(stack, n)
		n = child
	}
	for i, existing := range n.retainedWatches {
		if existing == w {
			n.retainedWatches = append(n.retainedWatches[:i], n.retainedWatches[i+1:]...)
			break
		}
	}
	b.pruneEmptyLocked(stack, pattern)
}

func (b *Bus) deliverRetainedWatchLocked(w *RetainedWatch, ev RetainedEvent, gen uint64) {
	if w == nil || w.closed {
		return
	}
	if w.lastDeliveryGen == gen {
		return
	}
	w.lastDeliveryGen = gen
	w.push(ev)
}

func (b *Bus) deliverRetainedWatchListLocked(list []*RetainedWatch, ev RetainedEvent, gen uint64) {
	for _, w := range list {
		b.deliverRetainedWatchLocked(w, ev, gen)
	}
}

func (b *Bus) deliverRetainedWatchersLocked(tp topic, ev RetainedEvent) {
	gen := b.nextRetainedDeliveryGenLocked()
	b.deliverRetainedWatchersAtLocked(b.root, tp, 0, ev, gen)
}

func (b *Bus) deliverRetainedWatchersAtLocked(n *node, tp topic, depth int, ev RetainedEvent, gen uint64) {
	if n == nil {
		return
	}
	if depth == len(tp) {
		b.deliverRetainedWatchListLocked(n.retainedWatches, ev, gen)
		if n.children != nil {
			if mw := n.children[b.mWild]; mw != nil {
				b.deliverRetainedWatchListLocked(mw.retainedWatches, ev, gen)
			}
		}
		return
	}
	tok := tp[depth]
	if n.children != nil {
		if child := n.children[tok]; child != nil {
			b.deliverRetainedWatchersAtLocked(child, tp, depth+1, ev, gen)
		}
		if sw := n.children[b.sWild]; sw != nil {
			b.deliverRetainedWatchersAtLocked(sw, tp, depth+1, ev, gen)
		}
		if mw := n.children[b.mWild]; mw != nil {
			b.deliverRetainedWatchListLocked(mw.retainedWatches, ev, gen)
		}
	}
}

func (b *Bus) replayRetainedToWatchLocked(n *node, pattern topic, depth int, w *RetainedWatch) {
	if n == nil {
		return
	}
	if depth == len(pattern) {
		if n.retainedSet {
			w.push(RetainedEvent{Op: RetainedSet, Topic: n.retained.Topic, Payload: n.retained.Payload})
		}
		return
	}
	ptok := pattern[depth]
	switch ptok {
	case b.mWild:
		b.replayAllRetainedToWatchLocked(n, w)
	case b.sWild:
		for _, child := range n.children {
			b.replayRetainedToWatchLocked(child, pattern, depth+1, w)
		}
	default:
		if child := n.children[ptok]; child != nil {
			b.replayRetainedToWatchLocked(child, pattern, depth+1, w)
		}
	}
}

func (b *Bus) replayAllRetainedToWatchLocked(n *node, w *RetainedWatch) {
	if n == nil {
		return
	}
	if n.retainedSet {
		w.push(RetainedEvent{Op: RetainedSet, Topic: n.retained.Topic, Payload: n.retained.Payload})
	}
	for _, child := range n.children {
		b.replayAllRetainedToWatchLocked(child, w)
	}
}

// -----------------------------------------------------------------------------
// Connection
// -----------------------------------------------------------------------------

type Connection struct {
	bus   *Bus
	subs  []*Subscription
	mu    sync.Mutex
	id    string
	rrCtr atomic.Uint32 // per-connection counter for reply tokens
}

func (b *Bus) NewConnection(id string) *Connection {
	return &Connection{bus: b, id: id}
}

// NewChildConnection creates a separate connection on the same bus.
// Services should use separate connections so subscriptions, request-reply
// counters, and Disconnect lifetimes remain locally owned.
func (c *Connection) NewChildConnection(id string) *Connection {
	if c == nil || c.bus == nil {
		return nil
	}
	return c.bus.NewConnection(id)
}

func (c *Connection) NewMessage(tp Topic, payload any, retained bool) *Message {
	return c.bus.NewMessage(tp, payload, retained)
}

func (c *Connection) Publish(msg *Message) { c.bus.Publish(msg) }

// PublishValue publishes without requiring callers to allocate a Message. It is
// most useful with retained watches/endpoints. Compatibility subscriptions that
// consume *Message may still force the message value to escape.
func (c *Connection) PublishValue(tp Topic, payload any, retained bool) {
	c.bus.PublishValue(tp, payload, retained)
}

func (c *Connection) Subscribe(tp Topic) *Subscription {
	return c.SubscribeBuffered(tp, 0)
}

// SubscribeBuffered subscribes with a per-subscription channel size. Passing
// queueLen <= 0 preserves the bus default.
func (c *Connection) SubscribeBuffered(tp Topic, queueLen int) *Subscription {
	if queueLen <= 0 {
		queueLen = c.bus.qLen
	}
	ct := toConcrete(tp)
	sub := &Subscription{topic: ct, ch: make(chan Message, queueLen), bus: c.bus, conn: c}
	c.bus.addSubscription(ct, sub)
	c.mu.Lock()
	c.subs = append(c.subs, sub)
	c.mu.Unlock()
	return sub
}

func (c *Connection) WatchRetained(tp Topic, opts RetainedWatchOptions) *RetainedWatch {
	if opts.QueueLen <= 0 {
		opts.QueueLen = c.bus.qLen
	}
	ct := toConcrete(tp)
	w := &RetainedWatch{pattern: ct, bus: c.bus, conn: c, ready: make(chan struct{}, 1), q: make([]RetainedEvent, opts.QueueLen)}
	c.bus.addRetainedWatch(ct, w, opts.Replay)
	return w
}

func (c *Connection) UnwatchRetained(w *RetainedWatch) {
	if w == nil {
		return
	}
	c.bus.removeRetainedWatch(w.pattern, w)
	w.bus.mu.Lock()
	w.closed = true
	w.q = nil
	w.len = 0
	w.head = 0
	w.bus.mu.Unlock()
	if w.set != nil {
		set := w.set
		w.set = nil
		set.removeRetainedWatch(w)
	}
	defer func() { _ = recover() }()
	close(w.ready)
}

func (c *Connection) Unsubscribe(sub *Subscription) {
	if sub == nil {
		return
	}
	c.bus.unsubscribe(sub.topic, sub)
	c.mu.Lock()
	c.subs = removeSub(c.subs, sub)
	c.mu.Unlock()
	if sub.set != nil {
		set := sub.set
		sub.set = nil
		set.remove(sub)
	}
	defer func() { _ = recover() }()
	close(sub.ch)
}

func (c *Connection) Disconnect() {
	c.mu.Lock()
	subs := c.subs
	c.subs = nil
	c.mu.Unlock()

	for _, sub := range subs {
		c.bus.unsubscribe(sub.topic, sub)
		if sub.set != nil {
			set := sub.set
			sub.set = nil
			set.remove(sub)
		}
		func(ch chan Message) {
			defer func() { _ = recover() }()
			close(ch)
		}(sub.ch)
	}
}

func removeSub(list []*Subscription, target *Subscription) []*Subscription {
	for i, s := range list {
		if s == target {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

// -----------------------------------------------------------------------------
// Bind/call endpoints
// -----------------------------------------------------------------------------

// Handler is the synchronous endpoint form. It mirrors lua-bus bind/call at the
// API boundary while keeping the first Go implementation deliberately small.
// Long-running handlers should remain on request/reply until they are migrated
// to an asynchronous endpoint worker.
type Handler func(context.Context, any) (any, error)

type Binding struct {
	topic   topic
	conn    *Connection
	handler Handler
}

func (b *Binding) Close() {
	if b == nil || b.conn == nil {
		return
	}
	b.conn.Unbind(b)
}

func (c *Connection) Bind(tp Topic, handler Handler) *Binding {
	if handler == nil {
		return nil
	}
	ct := toConcrete(tp)
	binding := &Binding{topic: ct, conn: c, handler: handler}
	c.bus.addBinding(ct, binding)
	return binding
}

func (c *Connection) Unbind(binding *Binding) {
	if binding == nil {
		return
	}
	c.bus.removeBinding(binding.topic, binding)
}

func (c *Connection) Call(ctx context.Context, tp Topic, payload any) (any, error) {
	return c.bus.call(ctx, toConcrete(tp), payload)
}

func (b *Bus) addBinding(tp topic, binding *Binding) {
	b.mu.Lock()
	n := b.root
	for _, t := range tp {
		n = ensureChild(n, t)
	}
	n.bindings = append(n.bindings, binding)
	b.mu.Unlock()
}

func (b *Bus) removeBinding(tp topic, binding *Binding) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.root
	var stack []*node
	for _, t := range tp {
		if n.children == nil {
			return
		}
		child := n.children[t]
		if child == nil {
			return
		}
		stack = append(stack, n)
		n = child
	}
	for i, existing := range n.bindings {
		if existing == binding {
			n.bindings = append(n.bindings[:i], n.bindings[i+1:]...)
			break
		}
	}
	b.pruneEmptyLocked(stack, tp)
}

func (b *Bus) call(ctx context.Context, tp topic, payload any) (any, error) {
	b.mu.Lock()
	n := b.root
	for _, t := range tp {
		if n.children == nil {
			b.mu.Unlock()
			return nil, errors.New("bus: no_route")
		}
		child := n.children[t]
		if child == nil {
			b.mu.Unlock()
			return nil, errors.New("bus: no_route")
		}
		n = child
	}
	if len(n.bindings) == 0 || n.bindings[0] == nil || n.bindings[0].handler == nil {
		b.mu.Unlock()
		return nil, errors.New("bus: no_route")
	}
	handler := n.bindings[0].handler
	b.mu.Unlock()
	return handler(ctx, payload)
}

// -----------------------------------------------------------------------------
// Request–Reply helpers
// -----------------------------------------------------------------------------

func (c *Connection) Request(msg *Message) *Subscription {
	if topicLen(msg.ReplyTo) == 0 {
		msg.ReplyTo = TNoIntern("_rr", c.rrCtr.Add(1)) // <- changed
	}
	sub := c.Subscribe(msg.ReplyTo)
	c.Publish(msg)
	return sub
}

func (c *Connection) RequestWait(ctx context.Context, msg *Message) (*Message, error) {
	sub := c.Request(msg)
	defer c.Unsubscribe(sub)

	select {
	case m, ok := <-sub.ch:
		if !ok {
			return nil, errors.New("subscription closed")
		}
		return &m, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Connection) Reply(to *Message, payload any, retained bool) {
	if topicLen(to.ReplyTo) == 0 {
		return
	}
	c.Publish(&Message{Topic: to.ReplyTo, Payload: payload, Retained: retained})
}
