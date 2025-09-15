// bus.go
package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
type Topic []Token

func T(tokens ...Token) Topic {
	for _, tok := range tokens {
		switch tok.(type) {
		case string, int, int32, int64, uint, uint32, uint64, uintptr:
			// fine
		default:
			// try a map assignment to force panic early if not comparable
			_ = map[Token]struct{}{tok: {}}
		}
	}
	return Topic(tokens)
}

// -----------------------------------------------------------------------------
// Message
// -----------------------------------------------------------------------------

type Message struct {
	Topic    Topic
	Payload  any
	Retained bool
	ReplyTo  Topic
	ID       uint32
}

func genID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// -----------------------------------------------------------------------------
// Subscription
// -----------------------------------------------------------------------------

type Subscription struct {
	topic Topic
	ch    chan *Message
	bus   *Bus
	conn  *Connection
}

func (s *Subscription) Topic() Topic             { return s.topic }
func (s *Subscription) Channel() <-chan *Message { return s.ch }
func (s *Subscription) Unsubscribe()             { s.conn.Unsubscribe(s) }

// -----------------------------------------------------------------------------
// Trie node (shared for subscribers and retained messages)
// -----------------------------------------------------------------------------

type node struct {
	children map[Token]*node
	subs     []*Subscription
	retained *Message
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
	idCtr atomic.Uint32
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

func (b *Bus) nextID() uint32 { return b.idCtr.Add(1) }

func (b *Bus) NewMessage(topic Topic, payload any, retained bool) *Message {
	return &Message{
		Topic:    topic,
		Payload:  payload,
		Retained: retained,
		ID:       b.nextID(),
	}
}

func (b *Bus) addSubscription(topic Topic, sub *Subscription) {
	b.mu.Lock()
	n := b.root
	for _, t := range topic {
		n = ensureChild(n, t)
	}
	n.subs = append(n.subs, sub)

	var retained []*Message
	b.collectRetainedLocked(b.root, topic, 0, &retained)
	b.mu.Unlock()

	for _, rm := range retained {
		b.tryDeliver(sub, rm)
	}
}

func (b *Bus) Publish(msg *Message) {
	b.mu.Lock()
	var subs []*Subscription
	b.collectSubscribersLocked(b.root, msg.Topic, 0, &subs)

	if msg.Retained {
		if msg.Payload == nil {
			b.retainDeleteLocked(msg.Topic)
		} else {
			b.retainSetLocked(msg)
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
	defer func() { _ = recover() }() // channel may be closed; best-effort delivery
	if trySend(sub.ch, msg) {
		return
	}
	drainOne(sub.ch)
	_ = trySend(sub.ch, msg)
}

// -----------------------------------------------------------------------------
// Unsubscribe + pruning
// -----------------------------------------------------------------------------

func (b *Bus) unsubscribe(topic Topic, sub *Subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := b.root
	var stack []*node
	for _, t := range topic {
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
	b.pruneEmptyLocked(stack, topic)
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

func (b *Bus) collectSubscribersLocked(n *node, topic Topic, depth int, out *[]*Subscription) {
	if n == nil {
		return
	}
	if depth == len(topic) {
		*out = append(*out, n.subs...)
		if n.children != nil {
			if mw := n.children[b.mWild]; mw != nil {
				*out = append(*out, mw.subs...) // '#' matches zero additional tokens
			}
		}
		return
	}
	tok := topic[depth]
	if n.children != nil {
		if child := n.children[tok]; child != nil {
			b.collectSubscribersLocked(child, topic, depth+1, out)
		}
		if sw := n.children[b.sWild]; sw != nil {
			b.collectSubscribersLocked(sw, topic, depth+1, out)
		}
		if mw := n.children[b.mWild]; mw != nil {
			*out = append(*out, mw.subs...) // '#' matches any remainder
		}
	}
}

// -----------------------------------------------------------------------------
// Retained storage and collection (pattern = subscription topic with wildcards)
// -----------------------------------------------------------------------------

func (b *Bus) retainSetLocked(msg *Message) {
	n := b.root
	for _, t := range msg.Topic {
		n = ensureChild(n, t)
	}
	n.retained = msg
}

func (b *Bus) retainDeleteLocked(topic Topic) {
	n := b.root
	var stack []*node
	for _, t := range topic {
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
	b.pruneEmptyLocked(stack, topic)
}

func (b *Bus) collectRetainedLocked(n *node, pattern Topic, depth int, out *[]*Message) {
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
	bus  *Bus
	subs []*Subscription
	mu   sync.Mutex
	id   string
}

func (b *Bus) NewConnection(id string) *Connection {
	return &Connection{bus: b, id: id}
}

func (c *Connection) NewMessage(topic Topic, payload any, retained bool) *Message {
	return c.bus.NewMessage(topic, payload, retained)
}

func (c *Connection) Publish(msg *Message) { c.bus.Publish(msg) }

func (c *Connection) Subscribe(topic Topic) *Subscription {
	sub := &Subscription{topic: topic, ch: make(chan *Message, c.bus.qLen), bus: c.bus, conn: c}
	c.bus.addSubscription(topic, sub)
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
	if len(msg.ReplyTo) == 0 {
		// ReplyTo is just a single-token topic containing a unique string
		msg.ReplyTo = T(genID())
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
	if len(to.ReplyTo) == 0 {
		return
	}
	c.Publish(&Message{Topic: to.ReplyTo, Payload: payload, Retained: retained, ID: c.bus.nextID()})
}
