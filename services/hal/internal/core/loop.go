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

type capMeta struct {
	domain string
	name   string
	kind   string
}

type HAL struct {
	conn *bus.Connection
	res  Resources

	// Device registry
	dev map[string]Device // devID -> device

	// Capability indices
	capIndex    map[capKey]CapID // (domain,kind,name) -> CapID
	capOwner    map[CapID]string // CapID -> devID
	capMetaByID map[CapID]capMeta
	nextCapID   CapID

	cfgSub  *bus.Subscription
	ctrlSub *bus.Subscription

	// Device → HAL events
	evCh chan Event
}

func NewHAL(conn *bus.Connection, res Resources) *HAL {
	h := &HAL{
		conn:        conn,
		res:         res,
		dev:         map[string]Device{},
		capIndex:    map[capKey]CapID{},
		capOwner:    map[CapID]string{},
		capMetaByID: map[CapID]capMeta{},
		evCh:        make(chan Event, 128),
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

	h.pubHALState("idle", "awaiting_config")

	// Wait for initial config
	var cfg types.HALConfig
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-h.cfgSub.Channel():
			if v, ok := msg.Payload.(types.HALConfig); ok {
				cfg = v
				goto APPLY
			}
		}
	}

APPLY:
	h.applyConfig(ctx, cfg)
	h.pubHALState("ready", "")

	for {
		select {
		case <-ctx.Done():
			h.pubHALState("stopped", "context_cancelled")
			return

		case m := <-h.ctrlSub.Channel():
			h.handleControl(m) // strictly non-blocking

		case ev := <-h.evCh:
			h.handleEvent(ev) // publish value/status or event/status
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

		// Register capabilities, assign CapIDs, and publish retained info + initial status:down
		caps := dev.Capabilities()
		ids := make([]CapID, len(caps))
		for i, cs := range caps {
			k := string(cs.Kind)
			domain := cs.Domain
			if domain == "" {
				domain = defaultDomainFor(string(cs.Kind))
			}
			name := cs.Name
			if name == "" {
				name = dev.ID()
			}

			id := h.nextCapID
			h.nextCapID++

			h.capIndex[capKey{domain: domain, kind: k, name: name}] = id
			h.capOwner[id] = dev.ID()
			h.capMetaByID[id] = capMeta{domain: domain, name: name, kind: k}
			ids[i] = id

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
		// Hand CapIDs back to device.
		dev.BindCapabilities(ids)
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

	id, ok := h.capIndex[capKey{domain: domain, kind: kind, name: name}]
	if !ok {
		h.replyErr(msg, "unknown_capability")
		return
	}
	ownerID := h.capOwner[id]
	dev := h.dev[ownerID]
	if dev == nil {
		h.replyErr(msg, "no_device")
		return
	}

	// Device.Control is non-blocking and returns enqueue outcome only.
	res, err := dev.Control(id, verb, msg.Payload)
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
	meta, ok := h.capMetaByID[ev.CapID]
	if !ok {
		return
	}

	domain := meta.domain
	k := meta.kind
	name := meta.name

	// 1) Error → retained status:degraded; no value/event published.
	if ev.Err != "" {
		h.conn.Publish(h.conn.NewMessage(
			capStatus(domain, k, name),
			types.CapabilityStatus{Link: types.LinkDegraded, TSms: ev.TSms, Error: ev.Err},
			true,
		))
		return
	}

	// 2) Success:
	//    a) If marked as event => publish non-retained event (optionally tagged);
	//       still update retained status:up.
	//    b) Else => publish retained value and retained status:up.
	if ev.IsEvent {
		if ev.EventTag != "" {
			h.conn.Publish(h.conn.NewMessage(capEventTagged(domain, k, name, ev.EventTag), ev.Payload, false))
		} else {
			h.conn.Publish(h.conn.NewMessage(capEvent(domain, k, name), ev.Payload, false))
		}
	} else {
		h.conn.Publish(h.conn.NewMessage(capValue(domain, k, name), ev.Payload, true)) // retained last-known good
	}

	// Retained status: up
	h.conn.Publish(h.conn.NewMessage(
		capStatus(domain, k, name),
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

// Default domain inference keeps devices simple if they don't set Domain explicitly.
// It is string-based to avoid depending on specific enum constants in types.
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

// ---- HAL as EventEmitter ----

func (h *HAL) Emit(ev Event) bool {
	select {
	case h.evCh <- ev:
		return true
	default:
		return false
	}
}
