// services/hal/internal/service/service.go
package service

import (
	"context"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/hal/config"
	"devicecode-go/services/hal/internal/gpioirq"
	"devicecode-go/services/hal/internal/halcore"
	"devicecode-go/services/hal/internal/registry"
	"devicecode-go/services/hal/internal/util"
	"devicecode-go/services/hal/internal/worker"
)

type devEntry struct {
	adaptor halcore.Adaptor
	caps    map[string]int // kind -> numeric capability id
	busID   string
}

type capKey struct {
	kind string
	id   int
}

type Service struct {
	conn  *bus.Connection
	buses halcore.I2CBusFactory
	pins  halcore.PinFactory

	workers map[string]*worker.MeasureWorker // busID -> worker
	results chan halcore.Result

	adaptors map[string]halcore.Adaptor // devID -> adaptor
	devices  map[string]devEntry

	capToDev  map[capKey]string // (kind,id) -> devID
	nextCapID map[string]int

	devPeriodMS map[string]int
	devNextDue  map[string]time.Time

	timer *time.Timer

	// GPIO IRQ support
	gpioW      *gpioirq.Worker
	gpioCancel map[string]func() // devID -> cancel function
}

func New(conn *bus.Connection, buses halcore.I2CBusFactory, pins halcore.PinFactory) *Service {
	return &Service{
		conn:        conn,
		buses:       buses,
		pins:        pins,
		workers:     map[string]*worker.MeasureWorker{},
		results:     make(chan halcore.Result, 64),
		adaptors:    map[string]halcore.Adaptor{},
		devices:     map[string]devEntry{},
		capToDev:    map[capKey]string{},
		nextCapID:   map[string]int{},
		devPeriodMS: map[string]int{},
		devNextDue:  map[string]time.Time{},
		gpioW:       gpioirq.New(64, 64),
		gpioCancel:  map[string]func(){},
	}
}

func (s *Service) Run(ctx context.Context) {
	s.gpioW.Start(ctx)

	cfgSub := s.conn.Subscribe(bus.Topic{"config", "hal"})
	ctrlSub := s.conn.Subscribe(bus.Topic{"hal", "capability", "+", "+", "control", "+"})
	defer s.conn.Unsubscribe(cfgSub)
	defer s.conn.Unsubscribe(ctrlSub)

	s.publishState("idle", "awaiting_config", nil)

	s.timer = time.NewTimer(time.Hour)
	if !s.timer.Stop() {
		util.DrainTimer(s.timer)
	}

	var gpioEv <-chan gpioirq.GPIOEvent = s.gpioW.Events()

	for {
		// (re)arm timer
		if next := s.earliestDevDue(); next.IsZero() {
			util.ResetTimer(s.timer, time.Hour)
		} else {
			util.ResetTimer(s.timer, time.Until(next))
		}

		select {
		case <-ctx.Done():
			s.publishState("stopped", "context_cancelled", nil)
			// best-effort: cancel any IRQ registrations
			for _, c := range s.gpioCancel {
				c()
			}
			return

		case msg := <-cfgSub.Channel():
			var cfg config.HALConfig
			if err := util.DecodeJSON(msg.Payload, &cfg); err != nil {
				s.publishState("error", "config_decode_failed", err)
				continue
			}
			if err := s.applyConfig(ctx, cfg); err != nil {
				s.publishState("error", "apply_config_failed", err)
				continue
			}
			s.publishState("ready", "configured", nil)

		case msg := <-ctrlSub.Channel():
			// hal/capability/<kind>/<id:int>/control/<method>
			if len(msg.Topic) < 6 {
				continue
			}
			kind, _ := msg.Topic[2].(string)
			idNum, ok := asInt(msg.Topic[3])
			if !ok || kind == "" {
				s.replyErr(msg, "invalid capability address")
				continue
			}
			key := capKey{kind: kind, id: idNum}
			devID, ok := s.capToDev[key]
			if !ok {
				s.replyErr(msg, "unknown capability")
				continue
			}
			method, _ := msg.Topic[5].(string)

			switch method {
			case "read_now":
				if s.submitMeasure(devID, true) {
					s.bumpDevNext(devID, time.Now())
					s.replyOK(msg, nil)
				} else {
					s.replyErr(msg, "busy")
				}
			case "set_rate":
				ms, ok := parsePeriodMS(msg.Payload)
				if ok && ms > 0 {
					s.devPeriodMS[devID] = util.ClampInt(ms, 200, 3_600_000)
					s.bumpDevNext(devID, time.Now())
					s.replyOK(msg, map[string]any{"period_ms": s.devPeriodMS[devID]})
				} else {
					s.replyErr(msg, "invalid period")
				}
			default:
				ent := s.devices[devID]
				if ent.adaptor == nil {
					s.replyErr(msg, "no adaptor")
					continue
				}
				if res, err := ent.adaptor.Control(kind, method, msg.Payload); err == nil {
					s.replyOK(msg, map[string]any{"result": res})
				} else {
					s.replyErr(msg, err.Error())
				}
			}

		case <-s.timer.C:
			now := time.Now()
			for devID, due := range s.devNextDue {
				if !now.Before(due) {
					s.submitMeasure(devID, false)
					s.bumpDevNext(devID, now)
				}
			}

		case r := <-s.results:
			s.handleResult(r)

		case ev := <-gpioEv:
			s.handleGPIOEvent(ev)
		}
	}
}

func (s *Service) applyConfig(ctx context.Context, cfg config.HALConfig) error {
	seen := map[string]struct{}{}

	for i := range cfg.Devices {
		d := &cfg.Devices[i]
		seen[d.ID] = struct{}{}

		// Skip if already present (simple idempotence)
		if _, exists := s.devices[d.ID]; exists {
			continue
		}

		// Find a builder
		b, ok := registry.Lookup(d.Type)
		if !ok {
			continue // unknown type; ignore
		}

		out, err := b.Build(registry.BuildInput{
			Ctx:        ctx,
			Buses:      s.buses,
			Pins:       s.pins,
			DeviceID:   d.ID,
			Type:       d.Type,
			ParamsJSON: d.Params,
			BusRefType: d.BusRef.Type,
			BusRefID:   d.BusRef.ID,
		})
		if err != nil {
			continue
		}

		// Ensure a worker for the referenced bus, if any.
		if out.BusID != "" {
			if _, ok := s.workers[out.BusID]; !ok {
				w := worker.New(halcore.WorkerConfig{}, s.results)
				w.Start(ctx)
				s.workers[out.BusID] = w
			}
		}

		// Record adaptor and publish retained capability info/state.
		ad := out.Adaptor
		s.adaptors[d.ID] = ad
		entry := devEntry{adaptor: ad, busID: out.BusID, caps: map[string]int{}}

		for _, ci := range ad.Capabilities() {
			id := s.nextCapID[ci.Kind]
			s.nextCapID[ci.Kind]++

			entry.caps[ci.Kind] = id
			s.capToDev[capKey{kind: ci.Kind, id: id}] = d.ID

			s.pubRet(capTopicInt(ci.Kind, id, "info"), ci.Info)
			s.pubRet(capTopicInt(ci.Kind, id, "state"),
				map[string]any{"link": "up", "ts_ms": time.Now().UnixMilli()})
		}
		s.devices[d.ID] = entry

		// Schedule periodic sampling for producers only (as declared by builder)
		if out.SampleEvery > 0 {
			ms := int(out.SampleEvery / time.Millisecond)
			s.devPeriodMS[d.ID] = util.ClampInt(ms, 200, 3_600_000)
			s.devNextDue[d.ID] = time.Now().Add(200 * time.Millisecond)
		}

		// Register GPIO IRQs if requested and supported
		if out.IRQ != nil && out.IRQ.Pin != nil {
			cancel, err := s.gpioW.RegisterInput(out.IRQ.DevID, out.IRQ.Pin, out.IRQ.Edge, out.IRQ.DebounceMS, out.IRQ.Invert)
			if err == nil {
				s.gpioCancel[d.ID] = cancel
			}
		}
	}

	// Tidy-up: remove devices not in config
	for devID, ent := range s.devices {
		if _, ok := seen[devID]; ok {
			continue
		}
		for kind, id := range ent.caps {
			s.pubRet(capTopicInt(kind, id, "info"), nil)
			s.pubRet(capTopicInt(kind, id, "state"),
				map[string]any{"link": "down", "ts_ms": time.Now().UnixMilli()})
			delete(s.capToDev, capKey{kind: kind, id: id})
		}
		if c, ok := s.gpioCancel[devID]; ok {
			c()
			delete(s.gpioCancel, devID)
		}
		delete(s.devices, devID)
		delete(s.adaptors, devID)
		delete(s.devPeriodMS, devID)
		delete(s.devNextDue, devID)
	}
	return nil
}

// ---- measurement helpers ----

func (s *Service) submitMeasure(devID string, prio bool) bool {
	ent, ok := s.devices[devID]
	if !ok {
		return false
	}
	w := s.workers[ent.busID]
	if w == nil {
		return false
	}
	return w.Submit(halcore.MeasureReq{ID: devID, Adaptor: ent.adaptor, Prio: prio})
}

func (s *Service) bumpDevNext(devID string, from time.Time) {
	period := time.Duration(util.ClampInt(s.devPeriodMS[devID], 200, 3_600_000)) * time.Millisecond
	s.devNextDue[devID] = from.Add(period)
}

func (s *Service) earliestDevDue() time.Time {
	var min time.Time
	for _, t := range s.devNextDue {
		if !t.IsZero() && (min.IsZero() || t.Before(min)) {
			min = t
		}
	}
	return min
}

// ---- results & events ----

func (s *Service) handleResult(r halcore.Result) {
	ent, ok := s.devices[r.ID]
	if !ok {
		return
	}
	now := time.Now().UnixMilli()

	if r.Err != nil {
		for kind, id := range ent.caps {
			s.pubRet(capTopicInt(kind, id, "state"),
				map[string]any{"link": "degraded", "error": r.Err.Error(), "ts_ms": now})
		}
		return
	}
	for _, rd := range r.Sample {
		id, ok := ent.caps[rd.Kind]
		if !ok {
			continue
		}
		s.conn.Publish(s.conn.NewMessage(
			capTopicInt(rd.Kind, id, "value"),
			rd.Payload,
			false,
		))
		s.pubRet(capTopicInt(rd.Kind, id, "state"), map[string]any{"link": "up", "ts_ms": now})
	}
}

func (s *Service) handleGPIOEvent(ev gpioirq.GPIOEvent) {
	ent, ok := s.devices[ev.DevID]
	if !ok {
		return
	}
	id, ok := ent.caps["gpio"]
	if !ok {
		return
	}
	ts := ev.TS.UnixMilli()

	// Event (non-retained)
	s.conn.Publish(s.conn.NewMessage(
		capTopicInt("gpio", id, "event"),
		map[string]any{
			"edge":  halcore.EdgeToString(ev.Edge),
			"level": ev.Level,
			"ts_ms": ts,
		},
		false,
	))
	// State (retained)
	s.pubRet(capTopicInt("gpio", id, "state"),
		map[string]any{"link": "up", "level": ev.Level, "ts_ms": ts})
}

// ---- bus helpers & utils ----

func (s *Service) publishState(level, status string, err error) {
	payload := map[string]any{"level": level, "status": status, "ts_ms": time.Now().UnixMilli()}
	if err != nil {
		payload["error"] = err.Error()
	}
	s.conn.Publish(s.conn.NewMessage(bus.Topic{"hal", "state"}, payload, true))
}

func (s *Service) replyOK(req *bus.Message, extra map[string]any) {
	if len(req.ReplyTo) == 0 {
		return
	}
	m := map[string]any{"ok": true}
	for k, v := range extra {
		m[k] = v
	}
	s.conn.Reply(req, m, false)
}

func (s *Service) replyErr(req *bus.Message, e string) {
	if len(req.ReplyTo) == 0 {
		return
	}
	s.conn.Reply(req, map[string]any{"ok": false, "error": e}, false)
}

func capTopicInt(kind string, id int, rest ...bus.Token) bus.Topic {
	base := bus.Topic{"hal", "capability", kind, id}
	return append(base, rest...)
}

func (s *Service) pubRet(t bus.Topic, p any) {
	s.conn.Publish(s.conn.NewMessage(t, p, true))
}

func parsePeriodMS(p any) (int, bool) {
	if m, ok := p.(map[string]any); ok {
		switch v := m["period_ms"].(type) {
		case int:
			return v, true
		case int64:
			return int(v), true
		case float64:
			return int(v), true
		}
	}
	return 0, false
}

func asInt(t any) (int, bool) {
	switch v := t.(type) {
	case int:
		return v, true
	case int8:
		return int(v), true
	case int16:
		return int(v), true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case uint:
		return int(v), true
	case uint8:
		return int(v), true
	case uint16:
		return int(v), true
	case uint32:
		return int(v), true
	case uint64:
		return int(v), true
	case float32:
		return int(v), true
	case float64:
		return int(v), true
	default:
		return 0, false
	}
}
