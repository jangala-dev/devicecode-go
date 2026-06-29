package core

import (
	"context"
	"math/rand"
	"time"

	"devicecode-go/bus"
	"devicecode-go/errcode"
	"devicecode-go/types"
	"devicecode-go/x/fmtx"
)

const eventQueueLen = 8

type capKey struct {
	domain string
	kind   types.Kind
	name   string
}

type capTopicSet struct {
	info   bus.Topic
	status bus.Topic
	value  bus.Topic
	event  bus.Topic
}

type capEventKey struct {
	capKey
	tag string
}

type HAL struct {
	conn *bus.Connection
	res  Resources

	// Device registry
	dev map[string]Device // devID -> device

	// Capability index: (domain,kind,name) -> devID and precomputed topics.
	capIndex      map[capKey]string
	capTopics     map[capKey]capTopicSet
	capEventTopic map[capEventKey]bus.Topic

	cfgSub  *bus.Subscription
	ctrlSub *bus.Subscription

	// Single-threaded publication of device events
	evCh chan Event

	// ---- Inlined poller state (single-threaded in HAL loop) ----
	pollWake   chan struct{} // edge-triggered wake
	pollTimer  *time.Timer   // reused timer
	pollItems  map[pollKey]*pollItem
	pollHeap   pollHeap
	randJitter *rand.Rand

	// Coalescing timestamps (retained value emissions)
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
		conn:          conn,
		res:           res,
		dev:           map[string]Device{},
		capIndex:      map[capKey]string{},
		capTopics:     map[capKey]capTopicSet{},
		capEventTopic: map[capEventKey]bus.Topic{},
		evCh:          make(chan Event, eventQueueLen),
		lastEmit:      make(map[capKey]int64),
		lastDevEmit:   make(map[string]int64),
		lastStatus: make(map[capKey]struct {
			link types.Link
			err  string
		}),
		// Inlined poller
		pollWake:   make(chan struct{}, 1),
		pollTimer:  time.NewTimer(time.Hour),
		pollItems:  make(map[pollKey]*pollItem),
		randJitter: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	// Ensure timer is stopped & drained before use.
	if !h.pollTimer.Stop() {
		select {
		case <-h.pollTimer.C:
		default:
		}
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
		// Arm/re-arm poll timer based on next due
		wait := h.pollNextWait()
		switch {
		case wait < 0:
			// no items -> keep timer stopped
			if !h.pollTimer.Stop() {
				select {
				case <-h.pollTimer.C:
				default:
				}
			}
		case wait == 0:
			// immediate fire: kick the loop
			select {
			case h.pollWake <- struct{}{}:
			default:
			}
			// leave timer stopped
			if !h.pollTimer.Stop() {
				select {
				case <-h.pollTimer.C:
				default:
				}
			}
		default:
			h.pollTimer.Reset(wait)
		}

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
				h.replyErr(&m, errcode.HALNotReady)
				continue
			}
			h.handleControl(&m) // strictly non-blocking

		case ev := <-h.evCh:
			// All device→HAL telemetry is published from this goroutine.
			h.handleEvent(ev)

		// Inlined poller wakes
		case <-h.pollWake:
			// handled after select
		case <-h.pollTimer.C:
			// handled after select
		}

		// After any wake/timer: fire at most one due poll (keeps loop responsive)
		if ready {
			if fire := h.pollFireDue(); fire != nil {
				// Coalescing: skip if a retained value was recently emitted
				k := capKey{domain: fire.key.d, kind: fire.key.k, name: fire.key.n}
				now := time.Now().UnixNano()

				ownerID, ok := h.capIndex[k]
				if ok {
					lastCap := h.lastEmit[k]
					lastDev := h.lastDevEmit[ownerID]
					lastAny := lastCap
					if lastDev > lastAny {
						lastAny = lastDev
					}
					if lastAny > 0 && (now-lastAny) < fire.every.Nanoseconds() {
						h.pollBumpAfter(fire.key.d, fire.key.k, fire.key.n, fire.key.verb, lastAny)
					} else {
						if dev := h.dev[ownerID]; dev != nil {
							// Best-effort; devices should return Busy if already active.
							_, _ = dev.Control(CapAddr{Domain: fire.key.d, Kind: fire.key.k, Name: fire.key.n}, fire.key.verb, nil)
						}
					}
				}
			}
		}

		// Drain timer channel if we stopped it but it fired concurrently.
		if !h.pollTimer.Stop() {
			select {
			case <-h.pollTimer.C:
			default:
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
		h.dev[dev.ID()] = dev
		// Register capabilities, publish retained info + initial status:down
		for _, cs := range dev.Capabilities() {
			h.registerCap(dev.ID(), cs)
		}
		if err := dev.Init(ctx); err != nil {
			panic(fmtx.Sprintf("[hal] init failed for: %s\n", dc.ID))
		}
	}
	// Apply declarative pollers from config after all capabilities are registered.
	for i := range cfg.Pollers {
		ps := cfg.Pollers[i]
		if ps.IntervalMs == 0 || ps.Verb == "" || ps.Domain == "" || ps.Kind == "" || ps.Name == "" {
			continue
		}
		h.pollUpsert(
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
		h.pollUpsert(cap.Domain, cap.Kind, cap.Name, ps.Verb,
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
		h.pollStop(cap.Domain, cap.Kind, cap.Name, verbToStop)
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
	if res.OK {
		h.replyOK(msg)
	} else {
		h.replyErr(msg, res.Error)
	}
}

func (h *HAL) handleEvent(ev Event) {
	d, k, n := ev.Addr.Domain, ev.Addr.Kind, ev.Addr.Name
	ck := capKey{domain: d, kind: k, name: n}
	ts := time.Now().UnixNano()
	// 1) Error → retained status:degraded; no value/event published.
	if ev.Err != "" {
		h.pubStatus(d, k, n, ts, ev.Err)
		return
	}
	// 2) Success: event vs value
	if ev.EventTag != "" {
		h.conn.PublishValue(h.taggedEventTopic(ck, ev.EventTag), ev.Payload, false)
	} else {
		h.conn.PublishValue(h.topicsFor(ck).value, ev.Payload, true)
		// Record last successful retained value emission for coalescing (capability-level).
		h.lastEmit[ck] = ts
		// Also record device-level emission time for cross-capability coalescing.
		if ownerID, ok := h.capIndex[ck]; ok {
			h.lastDevEmit[ownerID] = ts
		}
	}
	// 3) Retained status: up
	h.pubStatus(d, k, n, ts, "")
}

func (h *HAL) pubHALState(level, status string) {
	h.conn.PublishValue(
		T("hal", "state"),
		types.HALState{Level: level, Status: status, TS: time.Now().UnixNano()},
		true,
	)
}

// registerCap indexes the capability and publishes its info and initial status:down (retained).
func (h *HAL) registerCap(devID string, cs CapabilitySpec) {
	if cs.Domain == "" || string(cs.Kind) == "" || cs.Name == "" {
		panic(fmtx.Sprintf("[hal] capability must specify non-empty domain/kind/name: dev=%s", devID))
	}
	domain := cs.Domain
	k := cs.Kind
	name := cs.Name
	ck := capKey{domain: domain, kind: k, name: name}
	// Index for control routing and cache hot-path topics once at registration.
	h.capIndex[ck] = devID
	base := capBase(domain, k, name)
	ts := capTopicSet{
		info:   base.Append("info"),
		status: base.Append("status"),
		value:  base.Append("value"),
		event:  base.Append("event"),
	}
	h.capTopics[ck] = ts
	// Publish static info (retained).
	h.conn.PublishValue(
		ts.info,
		types.Info{
			SchemaVersion: cs.Info.SchemaVersion,
			Driver:        cs.Info.Driver,
			Detail:        cs.Info.Detail,
		},
		true,
	)
	// Publish initial status: down (retained).
	h.conn.PublishValue(
		ts.status,
		types.CapabilityStatus{Link: types.LinkDown, TS: time.Now().UnixNano()},
		true,
	)
	h.lastStatus[ck] =
		struct {
			link types.Link
			err  string
		}{link: types.LinkDown, err: ""}
}

func (h *HAL) topicsFor(ck capKey) capTopicSet {
	if ts, ok := h.capTopics[ck]; ok {
		return ts
	}
	base := capBase(ck.domain, ck.kind, ck.name)
	return capTopicSet{
		info:   base.Append("info"),
		status: base.Append("status"),
		value:  base.Append("value"),
		event:  base.Append("event"),
	}
}

func (h *HAL) taggedEventTopic(ck capKey, tag string) bus.Topic {
	key := capEventKey{capKey: ck, tag: tag}
	if t, ok := h.capEventTopic[key]; ok {
		return t
	}
	base := h.topicsFor(ck).event
	t := base.Append(tag)
	h.capEventTopic[key] = t
	return t
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
	h.conn.PublishValue(
		h.topicsFor(ck).status,
		types.CapabilityStatus{Link: link, TS: ts, Error: err},
		true,
	)
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
