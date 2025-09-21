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

	exec    map[string]*devExec // per-device executor (optional)
	cfgSub  *bus.Subscription
	ctrlSub *bus.Subscription
}

type devExec struct {
	q chan execReq
}
type execReq struct {
	ctx  context.Context
	msg  *bus.Message // reply-to (optional)
	kind types.Kind
}

func NewHAL(conn *bus.Connection, res Resources) *HAL {
	return &HAL{
		conn:     conn,
		res:      res,
		nextID:   map[string]int{},
		capToDev: map[capKey]string{},
		dev:      map[string]Device{},
		devCapID: map[string]map[string]int{},
		exec:     map[string]*devExec{},
	}
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
			h.handleControl(ctx, m)
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

		// Register capabilities
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

		// Create a small executor queue for read_now (bounded, lazy)
		ex := &devExec{q: make(chan execReq, 8)}
		h.exec[dev.ID()] = ex
		go h.runExecutor(ctx, dev, ex)
	}
}

func (h *HAL) runExecutor(ctx context.Context, dev Device, ex *devExec) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-ex.q:
			var last any
			_ = dev.Read(r.ctx, func(k types.Kind, payload any) {
				kindStr := string(k)

				// Safe nested lookup
				m, ok := h.devCapID[dev.ID()]
				if !ok {
					return
				}
				id, ok := m[kindStr]
				if !ok {
					return
				}

				last = payload
				h.conn.Publish(h.conn.NewMessage(capValue(kindStr, id), payload, false))
				h.conn.Publish(h.conn.NewMessage(
					capState(kindStr, id),
					types.CapabilityState{Link: types.LinkUp, TSms: nowMs()},
					true,
				))
			})
			if r.msg != nil && r.msg.CanReply() && last != nil {
				h.conn.Reply(r.msg, last, false)
			}
		}
	}
}

func (h *HAL) handleControl(ctx context.Context, msg *bus.Message) {
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

	switch verb {
	case "read_now":
		ex := h.exec[devID]
		if ex == nil {
			h.replyErr(msg, "busy")
			return
		}
		// Enqueue non-blocking; if full, fail fast
		req := execReq{ctx: ctx, msg: msg, kind: types.Kind(kind)}
		select {
		case ex.q <- req:
			// reply will be sent by executor
		default:
			h.replyErr(msg, "busy")
		}
	default:
		res, err := dev.Control(types.Kind(kind), verb, msg.Payload)
		if err != nil {
			h.replyErr(msg, err.Error())
			return
		}
		if msg.CanReply() {
			h.conn.Reply(msg, res, false)
		}
		// If the device returned a typed value, publish it as telemetry too.
		switch res.(type) {
		case types.LEDValue:
			h.conn.Publish(h.conn.NewMessage(capValue(kind, idNum), res, false))
		}
		h.conn.Publish(h.conn.NewMessage(capState(kind, idNum),
			types.CapabilityState{Link: types.LinkUp, TSms: nowMs()}, true))
	}
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
