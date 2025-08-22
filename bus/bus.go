// bus.go
package bus

import (
	"sync"
)

// -----------------------------------------------------------------------------
// Tokens + Topics
// -----------------------------------------------------------------------------

// Token is a single element in a topic path.
// It can be either a string or an integer.
type Token struct {
	kind byte // 0 = string, 1 = int
	sval string
	ival int
}

// Constructors
func S(s string) Token { return Token{kind: 0, sval: s} }
func I(i int) Token    { return Token{kind: 1, ival: i} }

// Topic is a sequence of tokens.
type Topic []Token

// -----------------------------------------------------------------------------
// Message
// -----------------------------------------------------------------------------

type Message struct {
	Topic    Topic
	Payload  any
	Retained bool
	ReplyTo  Topic
}

// -----------------------------------------------------------------------------
// Subscription
// -----------------------------------------------------------------------------

type Subscription struct {
	topic Topic
	ch    chan *Message
	bus   *Bus
	conn  *Connection // owning connection
}

func (s *Subscription) Topic() Topic             { return s.topic }
func (s *Subscription) Channel() <-chan *Message { return s.ch }
func (s *Subscription) Unsubscribe()             { s.conn.Unsubscribe(s) }

// -----------------------------------------------------------------------------
// Trie node
// -----------------------------------------------------------------------------

type node struct {
	children map[Token]*node
	subs     []*Subscription
	retained *Message
}

// -----------------------------------------------------------------------------
// Bus
// -----------------------------------------------------------------------------

type Bus struct {
	mu   sync.RWMutex
	root *node
	qLen int
}

// NewBus creates a new bus with the given subscription queue length.
func NewBus(queueLen int) *Bus {
	if queueLen <= 0 {
		queueLen = 8 // safe default
	}
	return &Bus{
		root: &node{},
		qLen: queueLen,
	}
}

// addSubscription inserts a subscription into the trie.
func (b *Bus) addSubscription(topic Topic, sub *Subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := b.root
	for _, tok := range topic {
		if n.children == nil {
			n.children = make(map[Token]*node)
		}
		child, ok := n.children[tok]
		if !ok {
			child = &node{}
			n.children[tok] = child
		}
		n = child
	}

	n.subs = append(n.subs, sub)

	// Deliver retained message if present.
	if n.retained != nil {
		select {
		case sub.ch <- n.retained:
		default:
		}
	}
}

// Publish delivers a message to all subscribers of its topic.
func (b *Bus) Publish(msg *Message) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := b.root
	for _, token := range msg.Topic {
		if n.children == nil {
			if !msg.Retained {
				return
			}
			n.children = make(map[Token]*node)
		}
		child, exists := n.children[token]
		if !exists {
			if !msg.Retained {
				return
			}
			child = &node{}
			n.children[token] = child
		}
		n = child
	}

	// Deliver to all subscribers at the final node.
	for _, sub := range n.subs {
		select {
		case sub.ch <- msg:
		default:
			// drop oldest if queue full
			<-sub.ch
			sub.ch <- msg
		}
	}

	// Store or clear retained message.
	if msg.Retained {
		if msg.Payload == nil {
			n.retained = nil
		} else {
			n.retained = msg
		}
	}
}

// unsubscribe removes a subscription from the trie.
func (b *Bus) unsubscribe(topic Topic, sub *Subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := b.root
	var stack []*node
	for _, t := range topic {
		if n.children == nil {
			return
		}
		child, ok := n.children[t]
		if !ok {
			return
		}
		stack = append(stack, n)
		n = child
	}

	// Remove subscription.
	for i, s := range n.subs {
		if s == sub {
			n.subs = append(n.subs[:i], n.subs[i+1:]...)
			break
		}
	}

	// Prune empty nodes.
	for i := len(topic) - 1; i >= 0; i-- {
		parent := stack[i]
		key := topic[i]
		child := parent.children[key]
		if len(child.subs) == 0 && len(child.children) == 0 && child.retained == nil {
			delete(parent.children, key)
		} else {
			break
		}
	}
}

// -----------------------------------------------------------------------------
// Connection
// -----------------------------------------------------------------------------

type Connection struct {
	bus  *Bus
	subs []*Subscription
	mu   sync.Mutex
	id   string // placeholder for future identity/auth
}

// NewConnection creates a new connection bound to this bus.
func (b *Bus) NewConnection(id string) *Connection {
	return &Connection{
		bus: b,
		id:  id,
	}
}

// Publish sends a message via the bus.
func (c *Connection) Publish(msg *Message) {
	c.bus.Publish(msg)
}

// Subscribe registers a subscription owned by this connection.
func (c *Connection) Subscribe(topic Topic) *Subscription {
	sub := &Subscription{
		topic: topic,
		ch:    make(chan *Message, c.bus.qLen),
		bus:   c.bus,
		conn:  c,
	}
	c.bus.addSubscription(topic, sub)
	c.mu.Lock()
	c.subs = append(c.subs, sub)
	c.mu.Unlock()
	return sub
}

// Unsubscribe removes a subscription owned by this connection.
func (c *Connection) Unsubscribe(sub *Subscription) {
	c.bus.unsubscribe(sub.topic, sub)
	c.mu.Lock()
	for i, s := range c.subs {
		if s == sub {
			c.subs = append(c.subs[:i], c.subs[i+1:]...)
			break
		}
	}
	c.mu.Unlock()
	close(sub.ch)
}

// Disconnect closes all subscriptions and clears them.
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
