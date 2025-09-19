// cmd/pico-bus-selftest/main.go
package main

import (
	"context"
	"sort"
	"time"

	"devicecode-go/bus"

	"machine"
)

// --- tiny logger (avoid fmt on MCU) ------------------------------------------

func logln(s string) { println(s) }
func logf(format string, a ...interface{}) {
	// minimal %s, %d substitution to keep code tiny
	out := make([]byte, 0, len(format)+16)
	argi := 0
	for i := 0; i < len(format); i++ {
		if format[i] == '%' && i+1 < len(format) {
			switch format[i+1] {
			case 's':
				if argi < len(a) {
					out = append(out, toString(a[argi])...)
					argi++
				}
				i++
				continue
			case 'd':
				if argi < len(a) {
					out = append(out, itoa(intFrom(a[argi]))...)
					argi++
				}
				i++
				continue
			}
		}
		out = append(out, format[i])
	}
	println(string(out))
}

func toString(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		// very small fallback
		return "<val>"
	}
}

func intFrom(v interface{}) int {
	switch x := v.(type) {
	case int:
		return x
	case int32:
		return int(x)
	case int64:
		return int(x)
	case uint:
		return int(x)
	case uint32:
		return int(x)
	case uint64:
		return int(x)
	default:
		return 0
	}
}

// tiny itoa (copy of your helper approach)
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	sign := ""
	if i < 0 {
		sign = "-"
		i = -i
	}
	var buf [32]byte
	b := len(buf)
	for i > 0 {
		b--
		buf[b] = byte('0' + (i % 10))
		i /= 10
	}
	if sign != "" {
		b--
		buf[b] = '-'
	}
	return string(buf[b:])
}

// --- helpers mirroring your test utilities -----------------------------------

func expectOneOf(sub *bus.Subscription, want string, timeout time.Duration) (ok bool, why string) {
	select {
	case got := <-sub.Channel():
		s, ok := got.Payload.(string)
		if !ok || s != want {
			return false, "unexpected payload"
		}
		return true, ""
	case <-time.After(timeout):
		return false, "timeout"
	}
}

func expectNoMessage(sub *bus.Subscription, timeout time.Duration) (ok bool, why string) {
	select {
	case got := <-sub.Channel():
		_ = got
		return false, "unexpected message"
	case <-time.After(timeout):
		return true, ""
	}
}

func drainPayloads(sub *bus.Subscription, n int, deadline time.Time) ([]string, bool, string) {
	var out []string
	for len(out) < n && time.Now().Before(deadline) {
		select {
		case m := <-sub.Channel():
			if s, ok := m.Payload.(string); ok {
				out = append(out, s)
			} else {
				return nil, false, "non-string payload"
			}
		case <-time.After(10 * time.Millisecond):
		}
	}
	if len(out) != n {
		return out, false, "drain count mismatch"
	}
	return out, true, ""
}

func assertUnorderedEqual(got, want []string) bool {
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// --- individual tests (return bool pass/fail) --------------------------------

func TestBasicPubSub() bool {
	bb := bus.NewBus(4)
	conn := bb.NewConnection("test")
	sub := conn.Subscribe(bus.T("config", "geo"))

	msg := conn.NewMessage(bus.T("config", "geo"), "hello", false)
	conn.Publish(msg)

	ok, why := expectOneOf(sub, "hello", 100*time.Millisecond)
	if !ok {
		logf("TestBasicPubSub: %s", why)
	}
	return ok
}

func TestRetainedMessage() bool {
	bb := bus.NewBus(2)
	conn := bb.NewConnection("test")

	conn.Publish(bb.NewMessage(bus.T("config", "geo"), "persist", true))
	sub := conn.Subscribe(bus.T("config", "geo"))

	ok, why := expectOneOf(sub, "persist", 100*time.Millisecond)
	if !ok {
		logf("TestRetainedMessage: %s", why)
	}
	return ok
}

func TestWildcard_SingleLevel() bool {
	b := bus.NewBus(16)
	c := b.NewConnection("test")

	s1 := c.Subscribe(bus.T("a", "+", "c"))
	s2 := c.Subscribe(bus.T("a", "+", "+"))
	s3 := c.Subscribe(bus.T("a", "b", "+"))
	sNo := c.Subscribe(bus.T("a", "+", "d"))

	c.Publish(b.NewMessage(bus.T("a", "b", "c"), "m1", false))
	if ok, _ := expectOneOf(s1, "m1", 200*time.Millisecond); !ok {
		logln("TestWildcard_SingleLevel: s1 failed")
		return false
	}
	if ok, _ := expectOneOf(s2, "m1", 200*time.Millisecond); !ok {
		logln("TestWildcard_SingleLevel: s2 failed")
		return false
	}
	if ok, _ := expectOneOf(s3, "m1", 200*time.Millisecond); !ok {
		logln("TestWildcard_SingleLevel: s3 failed")
		return false
	}
	if ok, _ := expectNoMessage(sNo, 60*time.Millisecond); !ok {
		logln("TestWildcard_SingleLevel: sNo got unexpected")
		return false
	}

	c.Publish(b.NewMessage(bus.T("a", "x", "y"), "m2", false))
	if ok, _ := expectOneOf(s2, "m2", 200*time.Millisecond); !ok {
		logln("TestWildcard_SingleLevel: s2 m2 failed")
		return false
	}
	if ok, _ := expectNoMessage(s1, 60*time.Millisecond); !ok {
		logln("TestWildcard_SingleLevel: s1 got unexpected")
		return false
	}
	if ok, _ := expectNoMessage(s3, 60*time.Millisecond); !ok {
		logln("TestWildcard_SingleLevel: s3 got unexpected")
		return false
	}
	if ok, _ := expectNoMessage(sNo, 60*time.Millisecond); !ok {
		logln("TestWildcard_SingleLevel: sNo got unexpected 2")
		return false
	}

	c.Publish(b.NewMessage(bus.T("a", "c"), "m3", false))
	ok1, _ := expectNoMessage(s1, 60*time.Millisecond)
	ok2, _ := expectNoMessage(s2, 60*time.Millisecond)
	ok3, _ := expectNoMessage(s3, 60*time.Millisecond)
	ok4, _ := expectNoMessage(sNo, 60*time.Millisecond)
	if !(ok1 && ok2 && ok3 && ok4) {
		logln("TestWildcard_SingleLevel: unexpected messages on short topic")
		return false
	}
	return true
}

func TestWildcard_MultiLevel() bool {
	b := bus.NewBus(16)
	c := b.NewConnection("test")

	sAHash := c.Subscribe(bus.T("a", "#"))
	sHash := c.Subscribe(bus.T("#"))
	sABHash := c.Subscribe(bus.T("a", "b", "#"))
	sAExact := c.Subscribe(bus.T("a"))

	c.Publish(b.NewMessage(bus.T("a"), "p1", false))
	if ok, _ := expectOneOf(sAHash, "p1", 200*time.Millisecond); !ok {
		logln("TestWildcard_MultiLevel: a# p1 fail")
		return false
	}
	if ok, _ := expectOneOf(sHash, "p1", 200*time.Millisecond); !ok {
		logln("TestWildcard_MultiLevel: # p1 fail")
		return false
	}
	if ok, _ := expectOneOf(sAExact, "p1", 200*time.Millisecond); !ok {
		logln("TestWildcard_MultiLevel: a p1 fail")
		return false
	}
	if ok, _ := expectNoMessage(sABHash, 60*time.Millisecond); !ok {
		logln("TestWildcard_MultiLevel: ab# got p1")
		return false
	}

	c.Publish(b.NewMessage(bus.T("a", "b"), "p2", false))
	if ok, _ := expectOneOf(sAHash, "p2", 200*time.Millisecond); !ok {
		logln("TestWildcard_MultiLevel: a# p2 fail")
		return false
	}
	if ok, _ := expectOneOf(sHash, "p2", 200*time.Millisecond); !ok {
		logln("TestWildcard_MultiLevel: # p2 fail")
		return false
	}
	if ok, _ := expectOneOf(sABHash, "p2", 200*time.Millisecond); !ok {
		logln("TestWildcard_MultiLevel: ab# p2 fail")
		return false
	}
	if ok, _ := expectNoMessage(sAExact, 60*time.Millisecond); !ok {
		logln("TestWildcard_MultiLevel: a got p2")
		return false
	}

	c.Publish(b.NewMessage(bus.T("a", "b", "c"), "p3", false))
	if ok, _ := expectOneOf(sAHash, "p3", 200*time.Millisecond); !ok {
		logln("TestWildcard_MultiLevel: a# p3 fail")
		return false
	}
	if ok, _ := expectOneOf(sHash, "p3", 200*time.Millisecond); !ok {
		logln("TestWildcard_MultiLevel: # p3 fail")
		return false
	}
	if ok, _ := expectOneOf(sABHash, "p3", 200*time.Millisecond); !ok {
		logln("TestWildcard_MultiLevel: ab# p3 fail")
		return false
	}
	if ok, _ := expectNoMessage(sAExact, 60*time.Millisecond); !ok {
		logln("TestWildcard_MultiLevel: a got p3")
		return false
	}
	return true
}

func TestWildcard_RetainedDelivery() bool {
	b := bus.NewBus(32)
	c := b.NewConnection("test")

	c.Publish(b.NewMessage(bus.T("a"), "r0", true))
	c.Publish(b.NewMessage(bus.T("a", "b"), "r1", true))
	c.Publish(b.NewMessage(bus.T("a", "b", "c"), "r2", true))
	c.Publish(b.NewMessage(bus.T("a", "x"), "r3", true))

	sAll := c.Subscribe(bus.T("a", "#"))
	gotAll, ok, _ := drainPayloads(sAll, 4, time.Now().Add(300*time.Millisecond))
	if !ok || !assertUnorderedEqual(gotAll, []string{"r0", "r1", "r2", "r3"}) {
		logln("TestWildcard_RetainedDelivery: sAll mismatch")
		return false
	}

	sPlusHash := c.Subscribe(bus.T("a", "+", "#"))
	gotPH, ok, _ := drainPayloads(sPlusHash, 3, time.Now().Add(300*time.Millisecond))
	if !ok || !assertUnorderedEqual(gotPH, []string{"r1", "r2", "r3"}) {
		logln("TestWildcard_RetainedDelivery: sPlusHash mismatch")
		return false
	}

	sPlus := c.Subscribe(bus.T("a", "+"))
	gotP, ok, _ := drainPayloads(sPlus, 2, time.Now().Add(300*time.Millisecond))
	if !ok || !assertUnorderedEqual(gotP, []string{"r1", "r3"}) {
		logln("TestWildcard_RetainedDelivery: sPlus mismatch")
		return false
	}
	return true
}

func TestWildcard_RetainedClear() bool {
	b := bus.NewBus(16)
	c := b.NewConnection("test")

	c.Publish(b.NewMessage(bus.T("a", "b"), "keep", true))
	c.Publish(b.NewMessage(bus.T("a", "y"), "other", true))
	c.Publish(b.NewMessage(bus.T("a", "b"), nil, true))

	s := c.Subscribe(bus.T("a", "#"))
	got, ok, _ := drainPayloads(s, 1, time.Now().Add(300*time.Millisecond))
	if !ok || len(got) != 1 || got[0] != "other" {
		logln("TestWildcard_RetainedClear: expected only 'other'")
		return false
	}
	return true
}

func TestWildcard_NoMatchCases() bool {
	b := bus.NewBus(8)
	c := b.NewConnection("test")
	s := c.Subscribe(bus.T("a", "+", "c"))

	c.Publish(b.NewMessage(bus.T("a", "c"), "x", false))
	if ok, _ := expectNoMessage(s, 60*time.Millisecond); !ok {
		logln("TestWildcard_NoMatchCases: got x")
		return false
	}
	c.Publish(b.NewMessage(bus.T("a", "b", "d"), "y", false))
	if ok, _ := expectNoMessage(s, 60*time.Millisecond); !ok {
		logln("TestWildcard_NoMatchCases: got y")
		return false
	}
	return true
}

func TestRequestReply_RequestWait() bool {
	b := bus.NewBus(8)
	reqConn := b.NewConnection("requester")
	respConn := b.NewConnection("responder")

	reqTopic := bus.T("power", "status", "get")
	respSub := respConn.Subscribe(reqTopic)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if msg, ok := <-respSub.Channel(); ok {
			respConn.Reply(msg, "OK", false)
		}
	}()

	req := b.NewMessage(reqTopic, nil, false)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	reply, err := reqConn.RequestWait(ctx, req)
	respConn.Unsubscribe(respSub)
	<-done

	if err != nil {
		logln("TestRequestReply_RequestWait: timeout/error")
		return false
	}
	got, ok := reply.Payload.(string)
	if !ok || got != "OK" {
		logln("TestRequestReply_RequestWait: bad reply payload")
		return false
	}
	// Ensure the reply arrived on the same topic as the request's ReplyTo.
	// (We can't name the unexported topic type, but we can use slice ops.)
	same := len(reply.Topic) == len(req.ReplyTo)
	if same {
		for i := 0; i < len(reply.Topic); i++ {
			if reply.Topic[i] != req.ReplyTo[i] {
				same = false
				break
			}
		}
	}
	if len(req.ReplyTo) == 0 || !same {
		logln("TestRequestReply_RequestWait: ReplyTo/topic mismatch")
		return false
	}
	return true
}

func TestRequestReply_Timeout() bool {
	b := bus.NewBus(8)
	reqConn := b.NewConnection("requester")

	req := b.NewMessage(bus.T("service", "noop"), nil, false)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := reqConn.RequestWait(ctx, req)
	if err == nil {
		logln("TestRequestReply_Timeout: expected timeout")
		return false
	}
	return true
}

func TestRequestReply_ManualSubscription() bool {
	b := bus.NewBus(8)
	reqConn := b.NewConnection("requester")
	respConn := b.NewConnection("responder")

	reqTopic := bus.T("sensor", "read")
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
			logln("TestRequestReply_ManualSubscription: wrong type")
			return false
		}
		v, ok := m["value"]
		if !ok || intFrom(v) != 42 {
			logln("TestRequestReply_ManualSubscription: bad content")
			return false
		}
	case <-time.After(300 * time.Millisecond):
		logln("TestRequestReply_ManualSubscription: timeout")
		return false
	}
	<-done
	return true
}

func TestTopic_InvalidTokenPanics() (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			// we DID get the panic we expected
			ok = true
		} else {
			logln("TestTopic_InvalidTokenPanics: expected panic, got none")
			ok = false
		}
	}()
	_ = bus.T([]byte{1, 2, 3}) // []byte is not comparable; should panic in T(...)
	return false               // only reached if no panic
}

// --- main: run all tests, report, and blink LED on failure --------------------

type testFn struct {
	name string
	fn   func() bool
}

func main() {
	// Give the USB CDC time to enumerate so logs show up reliably.
	time.Sleep(250 * time.Millisecond)

	// Configure onboard LED (GP25 on Pico).
	led := machine.LED
	led.Configure(machine.PinConfig{Mode: machine.PinOutput})
	led.High() // signal "running"

	tests := []testFn{
		{"TestBasicPubSub", TestBasicPubSub},
		{"TestRetainedMessage", TestRetainedMessage},
		{"TestWildcard_SingleLevel", TestWildcard_SingleLevel},
		{"TestWildcard_MultiLevel", TestWildcard_MultiLevel},
		{"TestWildcard_RetainedDelivery", TestWildcard_RetainedDelivery},
		{"TestWildcard_RetainedClear", TestWildcard_RetainedClear},
		{"TestWildcard_NoMatchCases", TestWildcard_NoMatchCases},
		{"TestRequestReply_RequestWait", TestRequestReply_RequestWait},
		{"TestRequestReply_Timeout", TestRequestReply_Timeout},
		{"TestRequestReply_ManualSubscription", TestRequestReply_ManualSubscription},
		{"TestTopic_InvalidTokenPanics", TestTopic_InvalidTokenPanics},
	}

	passed, failed := 0, 0
	logln("== bus self-test starting ==")
	for _, tc := range tests {
		ok := tc.fn()
		if ok {
			logf("[PASS] %s", tc.name)
			passed++
		} else {
			logf("[FAIL] %s", tc.name)
			failed++
		}
		// tiny pause between tests to keep timings sane on MCU
		time.Sleep(10 * time.Millisecond)
	}
	logf("== done: %d passed, %d failed ==", passed, failed)

	// LED: solid ON if all passed, otherwise slow blink forever.
	if failed == 0 {
		for {
			led.High()
			time.Sleep(2 * time.Second)
		}
	} else {
		for {
			led.High()
			time.Sleep(250 * time.Millisecond)
			led.Low()
			time.Sleep(250 * time.Millisecond)
		}
	}
}
