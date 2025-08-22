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
}

func (s *Subscription) Topic() Topic             { return s.topic }
func (s *Subscription) Channel() <-chan *Message { return s.ch }
func (s *Subscription) Unsubscribe()             { s.bus.unsubscribe(s.topic, s) }

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

// Subscribe registers a subscription to a topic.
func (b *Bus) Subscribe(topic Topic) *Subscription {
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

	sub := &Subscription{
		topic: topic,
		ch:    make(chan *Message, b.qLen),
		bus:   b,
	}
	n.subs = append(n.subs, sub)

	// Deliver retained message if present.
	if n.retained != nil {
		select {
		case sub.ch <- n.retained:
		default:
		}
	}
	return sub
}

// Publish delivers a message to all subscribers of its topic.
func (b *Bus) Publish(msg *Message) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := b.root
	// Walk the trie for the topic. Only create nodes if we MUST (for retained messages).
	for _, token := range msg.Topic {
		if n.children == nil {
			// No children at this level. Can we short-circuit?
			if !msg.Retained { // we can exit early.
				return
			}
			// We need to create the path for a retained message.
			n.children = make(map[Token]*node)
		}

		child, exists := n.children[token]
		if !exists {
			// Node for this token doesn't exist. Can we short-circuit?
			if !msg.Retained { // we can exit early.
				return
			}
			// We need to create the node for a retained message.
			child = &node{}
			n.children[token] = child
		}
		n = child
	}

	// Deliver to all subscribers at the final node.
	for _, sub := range n.subs {
		select {
		case sub.ch <- msg:
		default: // drop oldest message if subscriber queue is full
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
