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
	ch    chan *Message
	bus   *Bus
	conn  *Connection
}

func (s *Subscription) Topic() Topic             { return s.topic }
func (s *Subscription) Channel() <-chan *Message { return s.ch }
func (s *Subscription) Unsubscribe()             { s.conn.Unsubscribe(s) }

// Convenience wrapper that replies via the owning connection.
func (s *Subscription) Reply(to *Message, payload any, retained bool) {
	s.conn.Reply(to, payload, retained)
}

// -----------------------------------------------------------------------------
// Trie node (shared for subscribers and retained messages)
// -----------------------------------------------------------------------------

type node struct {
	children map[Token]*node
	subs     []*Subscription
	retained *Message // Message.Topic is opaque; internal traversal uses stored path
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
}

func NewBus(queueLen int) *Bus {
	return NewBusWithOptions(Options{QueueLen: queueLen, SingleWildcard: "+", MultiWildcard: "#"})
}

func NewBusWithOptions(o Options) *Bus {
	if o.QueueLen <= 0 {
		o.QueueLen = defaultQLen
	}
	if o.SingleWildcard == nil {
		o.SingleWildcard = "+"
	}
	if o.MultiWildcard == nil {
		o.MultiWildcard = "#"
	}
	return &Bus{
		root:  &node{},
		qLen:  o.QueueLen,
		sWild: o.SingleWildcard,
		mWild: o.MultiWildcard,
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

	var retained []*Message
	b.collectRetainedLocked(b.root, tp, 0, &retained)
	b.mu.Unlock()

	for _, rm := range retained {
		b.tryDeliver(sub, rm)
	}
}

func (b *Bus) Publish(msg *Message) {
	msgTopic := toConcrete(msg.Topic)

	b.mu.Lock()
	// collect into map to dedupe
	var subs []*Subscription
	// optional fast-path: reuse slice, then dedupe only if likely duplicates
	b.collectSubscribersLocked(b.root, msgTopic, 0, &subs)

	// Deduplicate
	if len(subs) > 1 {
		seen := make(map[*Subscription]struct{}, len(subs))
		j := 0
		for _, s := range subs {
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			subs[j] = s
			j++
		}
		subs = subs[:j]
	}

	if msg.Retained {
		if msg.Payload == nil {
			b.retainDeleteLocked(msgTopic)
		} else {
			b.retainSetLocked(msgTopic, msg)
		}
	}
	b.mu.Unlock()

	for _, sub := range subs {
		b.tryDeliver(sub, msg)
	}
}

func trySend(ch chan *Message, m *Message) bool {
	select {
	case ch <- m:
		return true
	default:
		return false
	}
}

func drainOne(ch chan *Message) {
	select {
	case <-ch:
	default:
	}
}

func (b *Bus) tryDeliver(sub *Subscription, msg *Message) {
	defer func() { _ = recover() }() // channel may be closed; best-effort
	if trySend(sub.ch, msg) {
		return
	}
	drainOne(sub.ch)
	_ = trySend(sub.ch, msg)
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
		if child != nil && len(child.subs) == 0 && len(child.children) == 0 && child.retained == nil {
			delete(parent.children, key)
		} else {
			break
		}
	}
}

// -----------------------------------------------------------------------------
// Subscriber collection (topic = concrete message topic)
// -----------------------------------------------------------------------------

func (b *Bus) collectSubscribersLocked(n *node, tp topic, depth int, out *[]*Subscription) {
	if n == nil {
		return
	}
	if depth == len(tp) {
		*out = append(*out, n.subs...)
		if n.children != nil {
			if mw := n.children[b.mWild]; mw != nil {
				*out = append(*out, mw.subs...) // '#' matches zero additional tokens
			}
		}
		return
	}
	tok := tp[depth]
	if n.children != nil {
		if child := n.children[tok]; child != nil {
			b.collectSubscribersLocked(child, tp, depth+1, out)
		}
		if sw := n.children[b.sWild]; sw != nil {
			b.collectSubscribersLocked(sw, tp, depth+1, out)
		}
		if mw := n.children[b.mWild]; mw != nil {
			*out = append(*out, mw.subs...) // '#' matches any remainder
		}
	}
}

// -----------------------------------------------------------------------------
// Retained storage and collection (pattern = subscription topic with wildcards)
// -----------------------------------------------------------------------------

func (b *Bus) retainSetLocked(tp topic, msg *Message) {
	n := b.root
	for _, t := range tp {
		n = ensureChild(n, t)
	}
	n.retained = msg
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
	n.retained = nil
	b.pruneEmptyLocked(stack, tp)
}

func (b *Bus) collectRetainedLocked(n *node, pattern topic, depth int, out *[]*Message) {
	if n == nil {
		return
	}
	if depth == len(pattern) {
		if n.retained != nil {
			*out = append(*out, n.retained)
		}
		return
	}
	ptok := pattern[depth]
	switch ptok {
	case b.mWild:
		b.collectAllRetainedLocked(n, out) // '#' consumes the rest (incl. zero)
	case b.sWild:
		for _, child := range n.children {
			b.collectRetainedLocked(child, pattern, depth+1, out)
		}
	default:
		if child := n.children[ptok]; child != nil {
			b.collectRetainedLocked(child, pattern, depth+1, out)
		}
	}
}

func (b *Bus) collectAllRetainedLocked(n *node, out *[]*Message) {
	if n == nil {
		return
	}
	if n.retained != nil {
		*out = append(*out, n.retained)
	}
	for _, child := range n.children {
		b.collectAllRetainedLocked(child, out)
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

func (c *Connection) NewMessage(tp Topic, payload any, retained bool) *Message {
	return c.bus.NewMessage(tp, payload, retained)
}

func (c *Connection) Publish(msg *Message) { c.bus.Publish(msg) }

func (c *Connection) Subscribe(tp Topic) *Subscription {
	ct := toConcrete(tp)
	sub := &Subscription{topic: ct, ch: make(chan *Message, c.bus.qLen), bus: c.bus, conn: c}
	c.bus.addSubscription(ct, sub)
	c.mu.Lock()
	c.subs = append(c.subs, sub)
	c.mu.Unlock()
	return sub
}

func (c *Connection) Unsubscribe(sub *Subscription) {
	c.bus.unsubscribe(sub.topic, sub)
	c.mu.Lock()
	c.subs = removeSub(c.subs, sub)
	c.mu.Unlock()
	close(sub.ch)
}

func (c *Connection) Disconnect() {
	c.mu.Lock()
	subs := c.subs
	c.subs = nil
	c.mu.Unlock()

	for _, sub := range subs {
		c.bus.unsubscribe(sub.topic, sub)
		close(sub.ch)
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
	case m := <-sub.ch:
		if m == nil {
			return nil, errors.New("subscription closed")
		}
		return m, nil
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
