// Package retainedpub provides small retained-state publishing helpers for
// services. It keeps de-duplication and liveness policy out of Fabric and gives
// services one consistent place to publish retained facts through bus.PublishValue.
package retainedpub

import (
	"time"

	"devicecode-go/bus"
)

// EqualFunc reports whether two facts are semantically equal.
type EqualFunc[T any] func(a, b T) bool

// ComparableEqual is the default equality helper for comparable fact types.
func ComparableEqual[T comparable](a, b T) bool { return a == b }

// Publisher owns one retained fact topic.
type Publisher[T any] struct {
	conn  *bus.Connection
	topic bus.Topic
	equal EqualFunc[T]

	last   T
	have   bool
	lastAt time.Time
}

// New constructs a retained publisher. If equal is nil, every fact is treated
// as changed by PublishChangedOrAlive.
func New[T any](conn *bus.Connection, topic bus.Topic, equal EqualFunc[T]) Publisher[T] {
	return Publisher[T]{conn: conn, topic: topic, equal: equal}
}

// PublishNow publishes the fact and updates the retained-publisher baseline.
func (p *Publisher[T]) PublishNow(now time.Time, fact T) bool {
	if p == nil || p.conn == nil {
		return false
	}
	p.conn.PublishValue(p.topic, fact, true)
	p.last = fact
	p.have = true
	p.lastAt = now
	return true
}

// PublishChangedOrAlive publishes immediately when fact changes, or republishes
// unchanged facts when keepalive has elapsed. A keepalive <= 0 disables
// liveness republish and only publishes changed facts.
func (p *Publisher[T]) PublishChangedOrAlive(now time.Time, fact T, keepalive time.Duration) bool {
	if p == nil || p.conn == nil {
		return false
	}
	changed := !p.have
	if !changed {
		if p.equal == nil {
			changed = true
		} else {
			changed = !p.equal(p.last, fact)
		}
	}
	due := false
	if p.have && keepalive > 0 {
		due = !now.Before(p.lastAt.Add(keepalive))
	}
	if !changed && !due {
		return false
	}
	return p.PublishNow(now, fact)
}

// Last returns the most recently published fact and whether one exists.
func (p *Publisher[T]) Last() (T, bool) { return p.last, p.have }
