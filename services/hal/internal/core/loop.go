package core

import (
	"context"
	"time"

	"devicecode-go/bus"
	"devicecode-go/types"
)

type capKey struct {
	kind string
	id   int
}

type HAL struct {
	conn *bus.Connection
	res  Resources

	nextID   map[string]int            // next numeric id per kind
	capToDev map[capKey]string         // (kind,id) -> devID
	dev      map[string]Device         // devID -> device
	devCapID map[string]map[string]int // devID -> kind -> id

	cfgSub  *bus.Subscription
	ctrlSub *bus.Subscription

	events <-chan Event // owner → HAL events
}

func NewHAL(conn *bus.Connection, res Resources) *HAL {
	return &HAL{
		conn:     conn,
		res:      res,
		nextID:   map[string]int{},
		capToDev: map[capKey]string{},
		dev:      map[string]Device{},
		devCapID: map[string]map[string]int{},
	}
}

func (h *HAL) Run(ctx context.Context) {
	h.cfgSub = h.conn.Subscribe(topicConfigHAL())
	h.ctrlSub = h.conn.Subscribe(ctrlWildcard())
	h.events = h.res.Reg.Events()

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

		case ev := <-h.events:
			h.handleEvent(ev) // publish value/state based on owner events
		}
	}
}

func (h *HAL) applyConfig(ctx context.Context, cfg types.HALConfig) {
	for i := range cfg.Devices {
		d := cfg.Devices[i]
		if _, exists := h.dev[d.ID]; exists {
			continue
		}
		b, ok := lookupBuilder(d.Type)
		if !ok {
			println("[hal] no builder for type:", d.Type, "id:", d.ID)
			continue
		}
		dev, err := b.Build(ctx, BuilderInput{
			ID:     d.ID,
			Type:   d.Type,
			Params: d.Params,
			Res:    h.res,
		})
		if err != nil {
			println("[hal] build failed for:", d.ID, "err:", err.Error())
			continue
		}
		if err := dev.Init(ctx); err != nil {
			println("[hal] init failed for:", d.ID)
			continue
		}
		h.dev[dev.ID()] = dev
		h.devCapID[dev.ID()] = map[string]int{}

		// Register capabilities and publish retained info + initial state:down
		for _, cs := range dev.Capabilities() {
			k := string(cs.Kind)
			id := h.nextID[k]
			h.nextID[k]++

			h.capToDev[capKey{kind: k, id: id}] = dev.ID()
			h.devCapID[dev.ID()][k] = id

			h.conn.Publish(h.conn.NewMessage(
				capInfo(k, id),
				types.Info{SchemaVersion: cs.Info.SchemaVersion, Driver: cs.Info.Driver, Detail: cs.Info.Detail},
				true,
			))
			h.conn.Publish(h.conn.NewMessage(
				capState(k, id),
				types.CapabilityState{Link: types.LinkDown, TSms: nowMs()},
				true,
			))
		}
	}
}

func (h *HAL) handleControl(msg *bus.Message) {
	if msg.Topic.Len() < 6 {
		return
	}
	kind, _ := msg.Topic.At(2).(string)
	idNum, _ := toInt(msg.Topic.At(3))
	verb, _ := msg.Topic.At(5).(string)

	devID, ok := h.capToDev[capKey{kind: kind, id: idNum}]
	if !ok {
		h.replyErr(msg, "unknown_capability")
		return
	}
	dev := h.dev[devID]
	if dev == nil {
		h.replyErr(msg, "no_device")
		return
	}

	// Device.Control is non-blocking and returns enqueue outcome only.
	res, err := dev.Control(types.Kind(kind), verb, msg.Payload)
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
	// Map (kind, devID) → numeric capability id.
	devCaps, ok := h.devCapID[ev.DevID]
	if !ok {
		return
	}
	id, ok := devCaps[string(ev.Kind)]
	if !ok {
		return
	}

	k := string(ev.Kind)
	if ev.Err != "" {
		// Failure: retained state → degraded; do not publish a value.
		h.conn.Publish(h.conn.NewMessage(
			capState(k, id),
			types.CapabilityState{Link: types.LinkDegraded, TSms: ev.TSms, Error: ev.Err},
			true,
		))
		return
	}

	// Success: publish value + retained up state.
	h.conn.Publish(h.conn.NewMessage(capValue(k, id), ev.Payload, false))
	h.conn.Publish(h.conn.NewMessage(
		capState(k, id),
		types.CapabilityState{Link: types.LinkUp, TSms: ev.TSms},
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

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int32:
		return int(x), true
	case int64:
		return int(x), true
	case float32:
		return int(x), true
	case float64:
		return int(x), true
	default:
		return 0, false
	}
}

func nowMs() int64 { return time.Now().UnixMilli() }

func coalesce(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
