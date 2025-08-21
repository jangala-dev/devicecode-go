// bus/bus_test.go
package bus

import (
	"testing"
	"time"
)

const (
	TopicConfig = "config"
	TopicGeo    = "geo"
)

func TestBasicPubSub(t *testing.T) {
	b := NewBus(4)

	sub := b.Subscribe(Topic{TopicConfig, TopicGeo})

	msg := &Message{
		Topic:   Topic{TopicConfig, TopicGeo},
		Payload: "hello",
	}
	b.Publish(msg)

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

	msg := &Message{
		Topic:    Topic{TopicConfig, TopicGeo},
		Payload:  "persist",
		Retained: true,
	}
	b.Publish(msg)

	sub := b.Subscribe(Topic{TopicConfig, TopicGeo})

	select {
	case got := <-sub.Channel():
		if got.Payload.(string) != "persist" {
			t.Errorf("expected retained payload 'persist', got %v", got.Payload)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for retained message")
	}
}
