package core

import (
	"context"
	"time"

	"devicecode-go/bus"
	"devicecode-go/errcode"
	"devicecode-go/types"
	"devicecode-go/x/fmtx"
)

const eventQueueLen = 32

type capKey struct {
	domain string
	kind   string
	name   string
}

type HAL struct {
	conn *bus.Connection
	res  Resources

	// Device registry
	dev map[string]Device // devID -> device

	// Capability index: (domain,kind,name) -> devID
	capIndex map[capKey]string

	cfgSub  *bus.Subscription
	ctrlSub *bus.Subscription

	// Single-threaded publication of device events
	evCh chan Event
}

func NewHAL(conn *bus.Connection, res Resources) *HAL {
	h := &HAL{
		conn:     conn,
		res:      res,
		dev:      map[string]Device{},
		capIndex: map[capKey]string{},
		evCh:     make(chan Event, eventQueueLen),
	}
	// HAL provides the emitter to devices.
	h.res.Pub = h
	return h
}

func (h *HAL) Run(ctx context.Context) {
	h.cfgSub = h.conn.Subscribe(topicConfigHAL())
	h.ctrlSub = h.conn.Subscribe(ctrlWildcard())
	defer h.conn.Unsubscribe(h.cfgSub)
	defer h.conn.Unsubscribe(h.ctrlSub)
	ready := false
	for {
		select {
		case <-ctx.Done():
			h.shutdown()
			h.pubHALState("stopped", "context_cancelled")
			return
		case msg := <-h.cfgSub.Channel():
			if v, ok := msg.Payload.(types.HALConfig); ok {
				// Existing applyConfig is additive/idempotent for existing devices.
				h.applyConfig(ctx, v)
				if !ready {
					ready = true
					h.pubHALState("ready", "")
				}
			}
		case m := <-h.ctrlSub.Channel():
			if !ready {
				// Reject controls until HAL has a configuration.
				h.reply(m, false, errcode.HALNotReady, nil)
				continue
			}
			h.handleControl(m) // strictly non-blocking
		case ev := <-h.evCh:
			// All device→HAL telemetry is published from this goroutine.
			h.handleEvent(ev)
		}
	}
}

// shutdown attempts a best-effort, orderly release of resources.
func (h *HAL) shutdown() {
	// 1) Ask devices to close and relinquish their claims.
	for _, d := range h.dev {
		_ = d.Close()
	}
	// 2) If the registry supports Close(), stop background workers (e.g. I2C).
	if c, ok := h.res.Reg.(interface{ Close() }); ok {
		c.Close()
	}
}

func (h *HAL) applyConfig(ctx context.Context, cfg types.HALConfig) {
	for i := range cfg.Devices {
		dc := cfg.Devices[i]
		if _, exists := h.dev[dc.ID]; exists {
			continue
		}
		b, ok := lookupBuilder(dc.Type)
		if !ok {
			panic(fmtx.Sprintf("[hal] no builder for type: %s id: %s\n", dc.Type, dc.ID))
		}
		dev, err := b.Build(ctx, BuilderInput{
			ID:     dc.ID,
			Type:   dc.Type,
			Params: dc.Params,
			Res:    h.res,
		})
		if err != nil {
			panic(fmtx.Sprintf("[hal] build failed for: %s err: %s\n", dc.ID, err.Error()))
		}
		if err := dev.Init(ctx); err != nil {
			panic(fmtx.Sprintf("[hal] init failed for: %s\n", dc.ID))
		}
		h.dev[dev.ID()] = dev

		// Register capabilities, publish retained info + initial status:down
		for _, cs := range dev.Capabilities() {
			h.registerCap(dev.ID(), cs)
		}

	}
}

func (h *HAL) handleControl(msg *bus.Message) {
	// hal/cap/<domain>/<kind>/<name>/control/<verb>
	if msg.Topic.Len() < 7 {
		h.reply(msg, false, errcode.InvalidTopic, nil)
		return
	}
	domain, _ := msg.Topic.At(2).(string)
	kind, _ := msg.Topic.At(3).(string)
	name, _ := msg.Topic.At(4).(string)
	verb, _ := msg.Topic.At(6).(string)

	ownerID, ok := h.capIndex[capKey{domain: domain, kind: kind, name: name}]
	if !ok {
		h.reply(msg, false, errcode.UnknownCapability, nil)
		return
	}
	dev := h.dev[ownerID]
	if dev == nil {
		h.reply(msg, false, errcode.Error, nil) // defensive fallback
		return
	}

	res, err := dev.Control(CapAddr{Domain: domain, Kind: kind, Name: name}, verb, msg.Payload)
	if err != nil {
		h.reply(msg, false, "", err)
		return
	}
	if !msg.CanReply() {
		return
	}
	if res.OK {
		h.reply(msg, true, "", nil)
		return
	}
	h.reply(msg, false, res.Error, nil)
}

func (h *HAL) handleEvent(ev Event) {
	d, k, n := ev.Addr.Domain, ev.Addr.Kind, ev.Addr.Name
	// 1) Error → retained status:degraded; no value/event published.
	if ev.Err != "" {
		h.pubStatus(d, k, n, ev.TS, ev.Err)
		return
	}
	// 2) Success: event vs value
	if ev.IsEvent {
		if ev.EventTag != "" {
			h.conn.Publish(h.conn.NewMessage(capEventTagged(d, k, n, ev.EventTag), ev.Payload, false))
		} else {
			h.conn.Publish(h.conn.NewMessage(capEvent(d, k, n), ev.Payload, false))
		}
	} else {
		h.conn.Publish(h.conn.NewMessage(capValue(d, k, n), ev.Payload, true))
	}
	// 3) Retained status: up
	h.pubStatus(d, k, n, ev.TS, "")
}

func (h *HAL) pubHALState(level, status string) {
	h.conn.Publish(h.conn.NewMessage(
		T("hal", "state"),
		types.HALState{Level: level, Status: status, TS: time.Now().UnixNano()},
		true,
	))
}

// registerCap indexes the capability and publishes its info and initial status:down (retained).
func (h *HAL) registerCap(devID string, cs CapabilitySpec) {
	k := string(cs.Kind)
	domain := cs.Domain
	if domain == "" {
		domain = defaultDomainFor(k)
	}
	name := cs.Name
	if name == "" {
		name = devID
	}
	// Index for control routing.
	h.capIndex[capKey{domain: domain, kind: k, name: name}] = devID
	// Publish static info (retained).
	h.conn.Publish(h.conn.NewMessage(
		capInfo(domain, k, name),
		types.Info{
			SchemaVersion: cs.Info.SchemaVersion,
			Driver:        cs.Info.Driver,
			Detail:        cs.Info.Detail,
		},
		true,
	))
	// Publish initial status: down (retained).
	h.conn.Publish(h.conn.NewMessage(
		capStatus(domain, k, name),
		types.CapabilityStatus{Link: types.LinkDown, TS: time.Now().UnixNano()},
		true,
	))
}

// pubStatus publishes a retained status update for a capability.
// err=="" → LinkUp; otherwise LinkDegraded and Error is included.
func (h *HAL) pubStatus(domain, kind, name string, ts int64, err string) {
	link := types.LinkUp
	if err != "" {
		link = types.LinkDegraded
	}
	h.conn.Publish(h.conn.NewMessage(
		capStatus(domain, kind, name),
		types.CapabilityStatus{Link: link, TS: ts, Error: err},
		true,
	))
}

// Default domain inference unchanged.
func defaultDomainFor(kind string) string {
	switch kind {
	case "temperature", "humidity":
		return "env"
	case "led", "pwm", "button":
		return "io"
	case "switch", "rail", "voltage", "current", "power":
		return "power"
	default:
		return "io"
	}
}

// ---- HAL as EventEmitter (enqueue to single publisher) ----

func (h *HAL) Emit(ev Event) bool {
	select {
	case h.evCh <- ev:
		return true
	default:
		return false
	}
}
