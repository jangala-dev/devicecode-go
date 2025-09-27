package core

import (
	"context"
	"time"

	"devicecode-go/bus"
	"devicecode-go/types"
)

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
}

func NewHAL(conn *bus.Connection, res Resources) *HAL {
	h := &HAL{
		conn:     conn,
		res:      res,
		dev:      map[string]Device{},
		capIndex: map[capKey]string{},
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
				h.replyErr(m, "hal_not_ready")
				continue
			}
			h.handleControl(m) // strictly non-blocking
		}
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
			println("[hal] no builder for type:", dc.Type, "id:", dc.ID)
			continue
		}
		dev, err := b.Build(ctx, BuilderInput{
			ID:     dc.ID,
			Type:   dc.Type,
			Params: dc.Params,
			Res:    h.res,
		})
		if err != nil {
			println("[hal] build failed for:", dc.ID, "err:", err.Error())
			continue
		}
		if err := dev.Init(ctx); err != nil {
			println("[hal] init failed for:", dc.ID)
			continue
		}
		h.dev[dev.ID()] = dev

		// Register capabilities, publish retained info + initial status:down
		caps := dev.Capabilities()
		for _, cs := range caps {
			k := string(cs.Kind)
			domain := cs.Domain
			if domain == "" {
				domain = defaultDomainFor(k)
			}
			name := cs.Name
			if name == "" {
				name = dev.ID()
			}

			h.capIndex[capKey{domain: domain, kind: k, name: name}] = dev.ID()

			h.conn.Publish(h.conn.NewMessage(
				capInfo(domain, k, name),
				types.Info{SchemaVersion: cs.Info.SchemaVersion, Driver: cs.Info.Driver, Detail: cs.Info.Detail},
				true,
			))
			// Initial status (retained)
			h.conn.Publish(h.conn.NewMessage(
				capStatus(domain, k, name),
				types.CapabilityStatus{Link: types.LinkDown, TSms: nowMs()},
				true,
			))
		}
	}
}

func (h *HAL) handleControl(msg *bus.Message) {
	// hal/cap/<domain>/<kind>/<name>/control/<verb>
	if msg.Topic.Len() < 7 {
		return
	}
	domain, _ := msg.Topic.At(2).(string)
	kind, _ := msg.Topic.At(3).(string)
	name, _ := msg.Topic.At(4).(string)
	verb, _ := msg.Topic.At(6).(string)

	ownerID, ok := h.capIndex[capKey{domain: domain, kind: kind, name: name}]
	if !ok {
		h.replyErr(msg, "unknown_capability")
		return
	}
	dev := h.dev[ownerID]
	if dev == nil {
		h.replyErr(msg, "no_device")
		return
	}

	res, err := dev.Control(CapAddr{Domain: domain, Kind: kind, Name: name}, verb, msg.Payload)
	if err != nil {
		h.replyErr(msg, err.Error())
		return
	}
	if msg.CanReply() {
		if res.OK {
			h.conn.Reply(msg, types.OKReply{OK: true}, false)
		} else {
			h.conn.Reply(msg, types.ErrorReply{OK: false, Error: coalesce(res.Error, "busy")}, false)
		}
	}
}

func (h *HAL) handleEvent(ev Event) {
	d := ev.Addr.Domain
	k := ev.Addr.Kind
	n := ev.Addr.Name

	// 1) Error → retained status:degraded; no value/event published.
	if ev.Err != "" {
		h.conn.Publish(h.conn.NewMessage(
			capStatus(d, k, n),
			types.CapabilityStatus{Link: types.LinkDegraded, TSms: ev.TSms, Error: ev.Err},
			true,
		))
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
	// Retained status: up
	h.conn.Publish(h.conn.NewMessage(
		capStatus(d, k, n),
		types.CapabilityStatus{Link: types.LinkUp, TSms: ev.TSms},
		true,
	))
}

func (h *HAL) pubHALState(level, status string) {
	h.conn.Publish(h.conn.NewMessage(
		T("hal", "state"),
		types.HALState{Level: level, Status: status, TSms: nowMs()},
		true,
	))
}

func (h *HAL) replyErr(msg *bus.Message, code string) {
	if !msg.CanReply() {
		return
	}
	if code == "" {
		code = "error"
	}
	h.conn.Reply(msg, types.ErrorReply{OK: false, Error: code}, false)
}

func nowMs() int64 { return time.Now().UnixMilli() }

func coalesce(s, d string) string {
	if s == "" {
		return d
	}
	return s
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

// ---- HAL as EventEmitter (now direct, no queue) ----

func (h *HAL) Emit(ev Event) bool {
	h.handleEvent(ev)
	return true
}
