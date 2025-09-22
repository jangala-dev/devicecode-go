// bus/bus_test.go
package bus

import (
	"context"
	"sort"
	"testing"
	"time"
)

const (
	TopicConfig = "config"
	TopicGeo    = "geo"
)

func TestBasicPubSub(t *testing.T) {
	b := NewBus(4)
	conn := b.NewConnection("test")

	sub := conn.Subscribe(T(TopicConfig, TopicGeo))

	msg := conn.NewMessage(T(TopicConfig, TopicGeo), "hello", false)
	conn.Publish(msg)

	select {
	case got := <-sub.Channel():
		if got.Payload.(string) != "hello" {
			t.Errorf("expected payload 'hello', got %v", got.Payload)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for message")
	}
}

func TestRetainedMessage(t *testing.T) {
	b := NewBus(2)
	conn := b.NewConnection("test")

	msg := conn.NewMessage(T(TopicConfig, TopicGeo), "persist", true)
	conn.Publish(msg)

	sub := conn.Subscribe(T(TopicConfig, TopicGeo))

	select {
	case got := <-sub.Channel():
		if got.Payload.(string) != "persist" {
			t.Errorf("expected retained payload 'persist', got %v", got.Payload)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for retained message")
	}
}

// -----------------------------------------------------------------------------
// Wildcards
// -----------------------------------------------------------------------------

func TestWildcard_SingleLevel(t *testing.T) {
	b := NewBus(16)
	c := b.NewConnection("test")

	s1 := c.Subscribe(T("a", "+", "c"))
	s2 := c.Subscribe(T("a", "+", "+"))
	s3 := c.Subscribe(T("a", "b", "+"))
	sNo := c.Subscribe(T("a", "+", "d"))

	c.Publish(b.NewMessage(T("a", "b", "c"), "m1", false))

	expectOneOf(t, s1, "m1")
	expectOneOf(t, s2, "m1")
	expectOneOf(t, s3, "m1")
	expectNoMessage(t, sNo)

	c.Publish(b.NewMessage(T("a", "x", "y"), "m2", false))

	expectOneOf(t, s2, "m2")
	expectNoMessage(t, s1)
	expectNoMessage(t, s3)
	expectNoMessage(t, sNo)

	c.Publish(b.NewMessage(T("a", "c"), "m3", false))
	expectNoMessage(t, s1)
	expectNoMessage(t, s2)
	expectNoMessage(t, s3)
	expectNoMessage(t, sNo)
}

func TestWildcard_MultiLevel(t *testing.T) {
	b := NewBus(16)
	c := b.NewConnection("test")

	sAHash := c.Subscribe(T("a", "#"))
	sHash := c.Subscribe(T("#"))
	sABHash := c.Subscribe(T("a", "b", "#"))
	sAExact := c.Subscribe(T("a"))

	c.Publish(b.NewMessage(T("a"), "p1", false))
	expectOneOf(t, sAHash, "p1")
	expectOneOf(t, sHash, "p1")
	expectOneOf(t, sAExact, "p1")
	expectNoMessage(t, sABHash)

	c.Publish(b.NewMessage(T("a", "b"), "p2", false))
	expectOneOf(t, sAHash, "p2")
	expectOneOf(t, sHash, "p2")
	expectOneOf(t, sABHash, "p2")
	expectNoMessage(t, sAExact)

	c.Publish(b.NewMessage(T("a", "b", "c"), "p3", false))
	expectOneOf(t, sAHash, "p3")
	expectOneOf(t, sHash, "p3")
	expectOneOf(t, sABHash, "p3")
	expectNoMessage(t, sAExact)
}

func TestWildcard_RetainedDelivery(t *testing.T) {
	b := NewBus(32)
	c := b.NewConnection("test")

	c.Publish(b.NewMessage(T("a"), "r0", true))
	c.Publish(b.NewMessage(T("a", "b"), "r1", true))
	c.Publish(b.NewMessage(T("a", "b", "c"), "r2", true))
	c.Publish(b.NewMessage(T("a", "x"), "r3", true))

	sAll := c.Subscribe(T("a", "#"))
	gotAll := drainPayloads(t, sAll, 4)
	assertUnorderedEqual(t, gotAll, []string{"r0", "r1", "r2", "r3"})

	sPlusHash := c.Subscribe(T("a", "+", "#"))
	gotPH := drainPayloads(t, sPlusHash, 3)
	assertUnorderedEqual(t, gotPH, []string{"r1", "r2", "r3"})

	sPlus := c.Subscribe(T("a", "+"))
	gotP := drainPayloads(t, sPlus, 2)
	assertUnorderedEqual(t, gotP, []string{"r1", "r3"})
}

func TestWildcard_RetainedClear(t *testing.T) {
	b := NewBus(16)
	c := b.NewConnection("test")

	c.Publish(b.NewMessage(T("a", "b"), "keep", true))
	c.Publish(b.NewMessage(T("a", "y"), "other", true))

	c.Publish(b.NewMessage(T("a", "b"), nil, true))

	s := c.Subscribe(T("a", "#"))
	got := drainPayloads(t, s, 1)

	if len(got) != 1 || got[0] != "other" {
		t.Fatalf("expected only 'other' after clear, got %v", got)
	}
}

func TestWildcard_NoMatchCases(t *testing.T) {
	b := NewBus(8)
	c := b.NewConnection("test")

	s := c.Subscribe(T("a", "+", "c"))

	c.Publish(b.NewMessage(T("a", "c"), "x", false))
	expectNoMessage(t, s)

	c.Publish(b.NewMessage(T("a", "b", "d"), "y", false))
	expectNoMessage(t, s)
}

// -----------------------------------------------------------------------------
// Request–Reply
// -----------------------------------------------------------------------------

func TestRequestReply_RequestWait(t *testing.T) {
	b := NewBus(8)
	reqConn := b.NewConnection("requester")
	respConn := b.NewConnection("responder")

	reqTopic := T("power", "status", "get")
	respSub := respConn.Subscribe(reqTopic)
	defer respConn.Unsubscribe(respSub)

	go func() {
		if msg, ok := <-respSub.Channel(); ok {
			respConn.Reply(msg, "OK", false)
		}
	}()

	req := b.NewMessage(reqTopic, nil, false)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	reply, err := reqConn.RequestWait(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error waiting for reply: %v", err)
	}
	if got, ok := reply.Payload.(string); !ok || got != "OK" {
		t.Fatalf("unexpected reply payload: %#v", reply.Payload)
	}
	if !req.CanReply() {
		t.Fatal("request lacks ReplyTo after RequestWait")
	}
	// if !topicsEqual(reply.Topic, req.ReplyTo) {
	// 	t.Fatalf("reply topic %v != request ReplyTo %v", reply.Topic, req.ReplyTo)
	// }
}

func TestRequestReply_Timeout(t *testing.T) {
	b := NewBus(8)
	reqConn := b.NewConnection("requester")

	req := b.NewMessage(T("service", "noop"), nil, false)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := reqConn.RequestWait(ctx, req)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestRequestReply_ManualSubscription(t *testing.T) {
	b := NewBus(8)
	reqConn := b.NewConnection("requester")
	respConn := b.NewConnection("responder")

	reqTopic := T("sensor", "read")
	reqSub := respConn.Subscribe(reqTopic)
	defer respConn.Unsubscribe(reqSub)

	reqMsg := b.NewMessage(reqTopic, nil, false)
	replySub := reqConn.Request(reqMsg)
	defer reqConn.Unsubscribe(replySub)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if msg, ok := <-reqSub.Channel(); ok {
			respConn.Reply(msg, map[string]any{"value": 42}, false)
		}
	}()

	select {
	case got := <-replySub.Channel():
		m, ok := got.Payload.(map[string]any)
		if !ok {
			t.Fatalf("unexpected reply type: %#v", got.Payload)
		}
		if m["value"] != 42 {
			t.Fatalf("unexpected reply content: %#v", m)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("timeout waiting for manual reply")
	}

	<-done
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

// // topicsEqual compares two opaque Topics by asserting the concrete internal type.
// // Tests are in the same package, so they may refer to unexported identifiers.
// func topicsEqual(a, b Topic) bool {
// 	if a == nil && b == nil {
// 		return true
// 	}
// 	ta, okA := a.(topic)
// 	tb, okB := b.(topic)
// 	if !okA || !okB {
// 		return false
// 	}
// 	if len(ta) != len(tb) {
// 		return false
// 	}
// 	for i := range ta {
// 		if ta[i] != tb[i] {
// 			return false
// 		}
// 	}
// 	return true
// }

func expectOneOf(t *testing.T, sub *Subscription, want string) {
	t.Helper()
	select {
	case got := <-sub.Channel():
		s, ok := got.Payload.(string)
		if !ok || s != want {
			t.Fatalf("unexpected payload: %v (want %q)", got.Payload, want)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timeout waiting for %q", want)
	}
}

func expectNoMessage(t *testing.T, sub *Subscription) {
	t.Helper()
	select {
	case got := <-sub.Channel():
		t.Fatalf("unexpected message: %#v", got)
	case <-time.After(60 * time.Millisecond):
	}
}

func drainPayloads(t *testing.T, sub *Subscription, n int) []string {
	t.Helper()
	var out []string
	deadline := time.Now().Add(300 * time.Millisecond)
	for len(out) < n && time.Now().Before(deadline) {
		select {
		case m := <-sub.Channel():
			if s, ok := m.Payload.(string); ok {
				out = append(out, s)
			} else {
				t.Fatalf("non-string payload in drain: %#v", m.Payload)
			}
		case <-time.After(10 * time.Millisecond):
		}
	}
	if len(out) != n {
		t.Fatalf("drainPayloads: expected %d messages, got %d (%v)", n, len(out), out)
	}
	return out
}

func assertUnorderedEqual(t *testing.T, got, want []string) {
	t.Helper()
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %d, want %d (%v vs %v)", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("mismatch at %d: got %q, want %q (got=%v want=%v)", i, got[i], want[i], got, want)
		}
	}
}

func TestTopic_InvalidTokenPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for non-comparable token, got none")
		}
	}()

	// []byte is not comparable, so T should panic
	_ = T([]byte{1, 2, 3})
}
