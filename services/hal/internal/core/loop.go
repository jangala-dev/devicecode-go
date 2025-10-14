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
	kind   types.Kind
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

	// Polling
	pollCh      chan PollReq
	poll        *Poller
	lastEmit    map[capKey]int64 // last retained value emission TS (ns) per capability
	lastDevEmit map[string]int64 // last retained value emission TS (ns) per device

	// De-chatter: last published status per capability
	lastStatus map[capKey]struct {
		link types.Link
		err  string
	}
}

func NewHAL(conn *bus.Connection, res Resources) *HAL {
	h := &HAL{
		conn:        conn,
		res:         res,
		dev:         map[string]Device{},
		capIndex:    map[capKey]string{},
		evCh:        make(chan Event, eventQueueLen),
		pollCh:      make(chan PollReq, 32),
		lastEmit:    make(map[capKey]int64),
		lastDevEmit: make(map[string]int64),
		lastStatus: make(map[capKey]struct {
			link types.Link
			err  string
		}),
	}
	// HAL provides the emitter to devices.
	h.res.Pub = h
	// Poller delivers schedule ticks into h.pollCh.
	h.poll = NewPoller(h.pollCh)
	return h
}

func (h *HAL) Run(ctx context.Context) {
	// Start poller with child context; it exits when ctx is cancelled.
	pctx, cancel := context.WithCancel(ctx)
	go h.poll.Run(pctx)
	defer cancel()

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
				h.replyErr(m, errcode.HALNotReady)
				continue
			}
			h.handleControl(m) // strictly non-blocking
		case ev := <-h.evCh:
			// All device→HAL telemetry is published from this goroutine.
			h.handleEvent(ev)
		// Scheduled polling ticks are handled here and routed to devices.
		case pr := <-h.pollCh:
			if !ready {
				continue
			}
			k := capKey{domain: pr.Domain, kind: pr.Kind, name: pr.Name}
			now := time.Now().UnixNano()

			// Resolve owner for device-level coalescing.
			ownerID, ok := h.capIndex[k]
			if !ok {
				continue
			}

			// Coalesce using both capability-level and device-level last emissions.
			lastCap := h.lastEmit[k]
			lastDev := h.lastDevEmit[ownerID]
			lastAny := lastCap
			if lastDev > lastAny {
				lastAny = lastDev
			}
			if lastAny > 0 && (now-lastAny) < pr.Every.Nanoseconds() {
				h.poll.BumpAfter(pr.Domain, pr.Kind, pr.Name, pr.Verb, lastAny)
				continue
			}

			if dev := h.dev[ownerID]; dev != nil {
				// Best-effort; devices should return Busy if already active.
				_, _ = dev.Control(CapAddr{Domain: pr.Domain, Kind: pr.Kind, Name: pr.Name}, pr.Verb, nil)
			}
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

	// Apply declarative pollers from config after all capabilities are registered.
	for i := range cfg.Pollers {
		ps := cfg.Pollers[i]
		if ps.IntervalMs == 0 || ps.Verb == "" || ps.Domain == "" || ps.Kind == "" || ps.Name == "" {
			continue
		}
		h.poll.Upsert(
			ps.Domain, ps.Kind, ps.Name, ps.Verb,
			time.Duration(ps.IntervalMs)*time.Millisecond,
			time.Duration(ps.JitterMs)*time.Millisecond,
		)
	}
}

func (h *HAL) handleControl(msg *bus.Message) {
	// hal/cap/<domain>/<kind>/<name>/control/<verb>
	cap, verb, ok := parseCapCtrl(msg.Topic)
	if !ok {
		h.replyErr(msg, errcode.InvalidTopic)
		return
	}

	// HAL-handled verbs for polling (strictly typed payloads).
	switch verb {
	case "poll_start":
		ps, code := As[types.PollStart](msg.Payload)
		if code != "" || ps.Verb == "" || ps.IntervalMs == 0 {
			h.replyErr(msg, errcode.InvalidPayload)
			return
		}
		h.poll.Upsert(cap.Domain, cap.Kind, cap.Name, ps.Verb,
			time.Duration(ps.IntervalMs)*time.Millisecond,
			time.Duration(ps.JitterMs)*time.Millisecond)
		h.replyOK(msg)
		return
	case "poll_stop":
		ps, _ := As[types.PollStop](msg.Payload) // zero-value allowed
		verbToStop := ps.Verb
		if verbToStop == "" {
			verbToStop = "read"
		}
		h.poll.Stop(cap.Domain, cap.Kind, cap.Name, verbToStop)
		h.replyOK(msg)
		return
	}

	ownerID, ok := h.capIndex[capKey{domain: cap.Domain, kind: cap.Kind, name: cap.Name}]
	if !ok {
		h.replyErr(msg, errcode.UnknownCapability)
		return
	}
	dev := h.dev[ownerID]
	if dev == nil {
		h.replyErr(msg, errcode.Error)
		return
	}

	res, err := dev.Control(cap, verb, msg.Payload)
	if err != nil {
		h.replyErr(msg, errcode.Of(err))
		return
	}
	if !msg.CanReply() {
		return
	}
	if res.OK {
		h.replyOK(msg)
		return
	}
	h.replyErr(msg, res.Error)
}

func (h *HAL) handleEvent(ev Event) {
	d, k, n := ev.Addr.Domain, ev.Addr.Kind, ev.Addr.Name
	ck := capKey{domain: d, kind: k, name: n}
	// 1) Error → retained status:degraded; no value/event published.
	if ev.Err != "" {
		h.pubStatus(d, k, n, ev.TS, ev.Err)
		return
	}
	// 2) Success: event vs value
	if ev.EventTag != "" {
		h.conn.Publish(h.conn.NewMessage(capEventTagged(d, k, n, ev.EventTag), ev.Payload, false))
	} else {
		h.conn.Publish(h.conn.NewMessage(capValue(d, k, n), ev.Payload, true))
		// Record last successful retained value emission for coalescing (capability-level).
		h.lastEmit[ck] = ev.TS
		// Also record device-level emission time for cross-capability coalescing.
		if ownerID, ok := h.capIndex[ck]; ok {
			h.lastDevEmit[ownerID] = ev.TS
		}
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
	if cs.Domain == "" || string(cs.Kind) == "" || cs.Name == "" {
		panic(fmtx.Sprintf("[hal] capability must specify non-empty domain/kind/name: dev=%s", devID))
	}
	domain := cs.Domain
	k := cs.Kind
	name := cs.Name
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
	h.lastStatus[capKey{domain: domain, kind: k, name: name}] =
		struct {
			link types.Link
			err  string
		}{link: types.LinkDown, err: ""}
}

// pubStatus publishes a retained status update for a capability.
// err=="" → LinkUp; otherwise LinkDegraded and Error is included.
func (h *HAL) pubStatus(domain string, kind types.Kind, name string, ts int64, err string) {
	link := types.LinkUp
	if err != "" {
		link = types.LinkDegraded
	}
	ck := capKey{domain: domain, kind: kind, name: name}
	prev := h.lastStatus[ck]
	if prev.link == link && prev.err == err {
		return // unchanged → suppress publish
	}
	h.lastStatus[ck] = struct {
		link types.Link
		err  string
	}{link: link, err: err}
	h.conn.Publish(h.conn.NewMessage(
		capStatus(domain, kind, name),
		types.CapabilityStatus{Link: link, TS: ts, Error: err},
		true,
	))
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
