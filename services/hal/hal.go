// services/hal/hal.go
package hal

import (
	"context"
	"encoding/json"
	"time"

	"devicecode-go/bus"
)

// -----------------------------------------------------------------------------
// Entry point
// -----------------------------------------------------------------------------

func Run(ctx context.Context, conn *bus.Connection, i2cFactory I2CBusFactory, pinFactory PinFactory) {

	h := &service{
		conn:        conn,
		i2cFactory:  i2cFactory,
		pinFactory:  pinFactory,
		workers:     map[string]*measureWorker{},
		adaptors:    map[string]Adaptor{},
		devices:     map[string]devEntry{},
		capToDev:    map[capKey]string{},
		nextCapID:   map[string]int{},
		devPeriodMS: map[string]int{},
		devNextDue:  map[string]time.Time{},
		results:     make(chan Result, 32),
		gpioW:       newGPIOIRQWorker(32, 32),
		gpioCancel:  map[string]func(){},
	}
	h.gpioW.Start(ctx)
	h.loop(ctx)
}

// -----------------------------------------------------------------------------
// Types (as in your existing file)
// -----------------------------------------------------------------------------

type devEntry struct {
	adaptor Adaptor
	caps    map[string]int // kind -> numeric capability id
	busID   string
}

type capKey struct {
	kind string
	id   int
}

type service struct {
	conn       *bus.Connection
	i2cFactory I2CBusFactory
	pinFactory PinFactory

	workers  map[string]*measureWorker
	adaptors map[string]Adaptor
	devices  map[string]devEntry

	capToDev  map[capKey]string
	nextCapID map[string]int

	devPeriodMS map[string]int
	devNextDue  map[string]time.Time

	timer *time.Timer

	// Results fan-in
	results chan Result

	// GPIO IRQ support
	gpioW      *gpioIRQWorker
	gpioCancel map[string]func() // devID -> cancel function
}

// -----------------------------------------------------------------------------
// Main loop
// -----------------------------------------------------------------------------

func (s *service) loop(ctx context.Context) {
	cfgSub := s.conn.Subscribe(bus.Topic{"config", "hal"})
	ctrlSub := s.conn.Subscribe(bus.Topic{"hal", "capability", "+", "+", "control", "+"})
	defer s.conn.Unsubscribe(cfgSub)
	defer s.conn.Unsubscribe(ctrlSub)

	s.publishState("idle", "awaiting_config", nil)

	s.timer = time.NewTimer(time.Hour)
	if !s.timer.Stop() {
		drainTimer(s.timer)
	}

	var gpioEv <-chan GPIOEvent = s.gpioW.Events()

	for {
		// (re)arm timer
		if next := s.earliestDevDue(); next.IsZero() {
			if !s.timer.Stop() {
				drainTimer(s.timer)
			}
			s.timer.Reset(time.Hour)
		} else {
			d := time.Until(next)
			if d < 0 {
				d = 0
			}
			if !s.timer.Stop() {
				drainTimer(s.timer)
			}
			s.timer.Reset(d)
		}

		select {
		case <-ctx.Done():
			s.publishState("stopped", "context_cancelled", nil)
			return

		case msg := <-cfgSub.Channel():
			var cfg HALConfig
			if err := decodeJSON(msg.Payload, &cfg); err != nil {
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
				ms := parsePeriodMS(msg.Payload)
				if ms > 0 {
					s.devPeriodMS[devID] = clampInt(ms, 200, 3_600_000)
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

// -----------------------------------------------------------------------------
// Configuration
// -----------------------------------------------------------------------------

func (s *service) applyConfig(ctx context.Context, cfg HALConfig) error {

	seen := map[string]struct{}{}

	for i := range cfg.Devices {
		d := &cfg.Devices[i]
		seen[d.ID] = struct{}{}

		// Skip if already present (simple idempotence for now)
		if _, exists := s.devices[d.ID]; exists {
			continue
		}

		var ad Adaptor
		var busID string

		switch d.Type {
		case "aht20":
			// Require a valid I²C bus reference
			if d.BusRef.Type != "i2c" || d.BusRef.ID == "" {
				continue
			}
			i2c, ok := s.i2cFactory.ByID(d.BusRef.ID)
			if !ok {
				continue
			}
			// Ensure a worker for this bus
			if _, ok := s.workers[d.BusRef.ID]; !ok {
				w := NewWorker(WorkerConfig{}, s.results)
				w.Start(ctx)
				s.workers[d.BusRef.ID] = w
			}
			var p struct {
				Addr int `json:"addr"`
			}
			_ = decodeJSON(d.Params, &p)
			if p.Addr == 0 {
				p.Addr = 0x38
			}
			ad = NewAHT20Adaptor(d.ID, i2c, uint16(p.Addr))
			busID = d.BusRef.ID

		case "gpio":
			var p GPIOParams
			if err := decodeJSON(d.Params, &p); err != nil {
				continue
			}

			pin, ok := s.pinFactory.ByNumber(p.Pin)
			if !ok {
				continue
			}

			// Configure initial mode
			if p.Mode == "input" {
				if err := pin.ConfigureInput(parsePull(p.Pull)); err != nil {
					continue
				}
			} else {
				init := false
				if p.Initial != nil {
					init = *p.Initial
				}
				if p.Invert {
					init = !init
				}
				if err := pin.ConfigureOutput(init); err != nil {
					continue
				}
			}

			ad = NewGPIOAdaptor(d.ID, pin, p)

		default:
			continue
		}

		// Record adaptor and publish retained capability info/state.
		s.adaptors[d.ID] = ad
		entry := devEntry{adaptor: ad, busID: busID, caps: map[string]int{}}

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

		// Schedule periodic sampling for producers only
		switch d.Type {
		case "aht20":
			s.devPeriodMS[d.ID] = 2000
			s.devNextDue[d.ID] = time.Now().Add(200 * time.Millisecond)
		}

		// Register GPIO IRQs if configured and supported
		if d.Type == "gpio" {
			ga := ad.(*gpioAdaptor)
			if ga.params.Mode == "input" && ga.params.IRQ != nil && ParseEdge(ga.params.IRQ.Edge) != EdgeNone {
				if irqPin, ok := ga.pin.(IRQPin); ok {
					cancel, err := s.gpioW.RegisterInput(d.ID, irqPin, ParseEdge(ga.params.IRQ.Edge), ga.params.IRQ.DebounceMS, ga.params.Invert)
					if err != nil {
					} else {
						s.gpioCancel[d.ID] = cancel
					}
				}
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

// -----------------------------------------------------------------------------
// Results, events, and helpers (unchanged apart from optional debug)
// -----------------------------------------------------------------------------

func (s *service) submitMeasure(devID string, prio bool) bool {
	ent, ok := s.devices[devID]
	if !ok {
		return false
	}
	w := s.workers[ent.busID]
	if w == nil {
		return false
	}
	return w.Submit(MeasureReq{ID: devID, Adaptor: ent.adaptor, Prio: prio})
}

func (s *service) bumpDevNext(devID string, from time.Time) {
	period := time.Duration(clampInt(s.devPeriodMS[devID], 200, 3_600_000)) * time.Millisecond
	s.devNextDue[devID] = from.Add(period)
}

func (s *service) earliestDevDue() time.Time {
	var min time.Time
	for _, t := range s.devNextDue {
		if !t.IsZero() && (min.IsZero() || t.Before(min)) {
			min = t
		}
	}
	return min
}

func (s *service) handleResult(r Result) {
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
	// Publish each reading to its mapped capability id.
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

func (s *service) handleGPIOEvent(ev GPIOEvent) {
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
			"edge":  edgeToString(ev.Edge),
			"level": ev.Level,
			"ts_ms": ts,
		},
		false,
	))
	// State (retained)
	s.pubRet(capTopicInt("gpio", id, "state"),
		map[string]any{"link": "up", "level": ev.Level, "ts_ms": ts})
}

// ---- helpers (as in your existing file) ----

func (s *service) publishState(level, status string, err error) {
	payload := map[string]any{"level": level, "status": status, "ts_ms": time.Now().UnixMilli()}
	if err != nil {
		payload["error"] = err.Error()
	}
	s.conn.Publish(s.conn.NewMessage(bus.Topic{"hal", "state"}, payload, true))
}

func (s *service) replyOK(req *bus.Message, extra map[string]any) {
	if len(req.ReplyTo) == 0 {
		return
	}
	m := map[string]any{"ok": true}
	for k, v := range extra {
		m[k] = v
	}
	s.conn.Reply(req, m, false)
}

func (s *service) replyErr(req *bus.Message, e string) {
	if len(req.ReplyTo) == 0 {
		return
	}
	s.conn.Reply(req, map[string]any{"ok": false, "error": e}, false)
}

func capTopicInt(kind string, id int, rest ...bus.Token) bus.Topic {
	base := bus.Topic{"hal", "capability", kind, id}
	return append(base, rest...)
}

func (s *service) pubRet(t bus.Topic, p any) {
	s.conn.Publish(s.conn.NewMessage(t, p, true))
}

func parsePeriodMS(p any) int {
	if m, ok := p.(map[string]any); ok {
		switch v := m["period_ms"].(type) {
		case int:
			return v
		case int64:
			return int(v)
		case float64:
			return int(v)
		}
	}
	return 0
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func decodeJSON[T any](src any, dst *T) error {
	switch v := src.(type) {
	case []byte:
		return json.Unmarshal(v, dst)
	case string:
		return json.Unmarshal([]byte(v), dst)
	default:
		// Accept maps, structs, numbers… by marshaling then decoding to T.
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, dst)
	}
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
