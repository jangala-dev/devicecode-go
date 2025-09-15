# bus

An in-process **publish/subscribe bus** for Go with the following features:

* **Topics as token arrays** (strings, ints, or any comparable values).
* **Exact match, single-level (`+`) and multi-level (`#`) wildcards**.
* **Retained messages** (last message on a topic stored and delivered to new subscribers).
* **Request–reply** helper pattern.
* **Back-pressure handling** (bounded queues; drops oldest on overflow).
* **Connections** to group subscriptions for easy cleanup.

This is intended for lightweight concurrent services running in the same process or on microcontrollers with TinyGo.

---

## Installation

```bash
go get github.com/jangala-dev/go-bus
```

---

## Basic Usage

```go
b := bus.NewBus(8)                   // queue length = 8
conn := b.NewConnection("client1")   // create a connection

// Subscribe to a topic.
sub := conn.Subscribe(bus.Topic{"config", "geo"})

// Publish a message (via Bus or Connection).
msg := conn.NewMessage(bus.Topic{"config", "geo"}, "hello", false)
conn.Publish(msg)

// Receive from subscription channel.
select {
case m := <-sub.Channel():
    fmt.Println("got:", m.Payload)
}
```

---

## Topics and Tokens

* A **topic** is a `bus.Topic`, which is just `[]Token`.
* Tokens can be strings, integers, or any other comparable values.

Example:

```go
bus.Topic{"sensor", 1, "temperature"}
```

---

## Wildcards

Subscriptions may include:

* **Single-level**: `+` matches exactly one token.
* **Multi-level**: `#` matches zero or more trailing tokens.

Example:

```go
c := b.NewConnection("test")

// Subscribe to "a/+/c"
s1 := c.Subscribe(bus.Topic{"a", "+", "c"})

// Subscribe to "a/#"
s2 := c.Subscribe(bus.Topic{"a", "#"})

// Publish "a/b/c"
c.Publish(c.NewMessage(bus.Topic{"a", "b", "c"}, "payload", false))
```

* `s1` receives because `+` matches `b`.
* `s2` receives because `#` matches `b/c`.

---

## Retained Messages

* A retained message is stored at its topic and delivered to **new subscribers** immediately.
* Publish a retained message with `Retained: true`.
* Clear a retained message by publishing with `Payload: nil` and `Retained: true`.

```go
// Publish retained
c.Publish(c.NewMessage(bus.Topic{"status"}, "online", true))

// Later: new subscriber immediately gets "online".
s := c.Subscribe(bus.Topic{"status"})
```

---

## Request–Reply

Helpers make it easy to implement request–reply patterns.

```go
// Requester
reqConn := b.NewConnection("req")
req := reqConn.NewMessage(bus.Topic{"service", "get"}, nil, false)

ctx, cancel := context.WithTimeout(context.Background(), time.Second)
defer cancel()

resp, err := reqConn.RequestWait(ctx, req)
if err != nil {
    log.Fatal(err)
}
fmt.Println("got reply:", resp.Payload)
```

```go
// Responder
respConn := b.NewConnection("resp")
sub := respConn.Subscribe(bus.Topic{"service", "get"})
go func() {
    for m := range sub.Channel() {
        respConn.Reply(m, "OK", false)
    }
}()
```

* `Request` subscribes to a unique reply topic, sets `ReplyTo`, publishes, and returns the subscription.
* `RequestWait` is a convenience that waits for one reply (or timeout).

---

## Connections and Subscriptions

* `Connection` groups subscriptions for cleanup.
* `Unsubscribe(sub)` removes a subscription.
* `Disconnect()` removes and closes **all** subscriptions.

---

## Back-pressure

* Each subscription has a bounded queue (`QueueLen`).
* If the queue is full, the **oldest message is dropped** to make space.

---

## Custom Wildcards

You can override default wildcard tokens (`+` and `#`) when creating the bus:

```go
b := bus.NewBusWithOptions(bus.Options{
    QueueLen:       8,
    SingleWildcard: "?",
    MultiWildcard:  "*",
})
```

---

## Summary

* Use `NewBus` → `NewConnection` → `Subscribe` / `Publish`.
* Topics are arrays of tokens; support `+` and `#` wildcards.
* Retained messages persist per topic and deliver to new subscribers.
* Request–reply helpers simplify RPC-style interactions.
* Connection cleanup is straightforward with `Disconnect()`.

This provides a flexible, efficient message bus suitable for embedded or service-oriented applications.