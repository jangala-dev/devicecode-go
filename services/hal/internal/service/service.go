// services/hal/internal/service/service.go
package service

import (
	"context"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/hal/internal/consts"
	"devicecode-go/services/hal/internal/gpioirq"
	"devicecode-go/services/hal/internal/halcore"
	"devicecode-go/services/hal/internal/halerr"
	"devicecode-go/services/hal/internal/registry"
	"devicecode-go/services/hal/internal/uartio"
	"devicecode-go/services/hal/internal/util"
	"devicecode-go/services/hal/internal/worker"

	"devicecode-go/types"
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
	uarts halcore.UARTFactory

	workers map[string]*worker.MeasureWorker // busID -> worker
	results chan halcore.Result

	adaptors map[string]halcore.Adaptor // devID -> adaptor
	devices  map[string]devEntry

	capToDev  map[capKey]string // (kind,id) -> devID
	nextCapID map[string]int

	devPeriod  map[string]time.Duration
	devNextDue map[string]time.Time

	timer *time.Timer

	// GPIO IRQ support
	gpioW      *gpioirq.Worker
	gpioCancel map[string]func() // devID -> cancel function

	// UART reader support
	uartW      *uartio.Worker
	uartCancel map[string]func()
	uartEcho   map[string]bool
}

var (
	topicConfigHAL = bus.Topic{consts.TokConfig, consts.TokHAL}
	topicCtrl      = bus.Topic{consts.TokHAL, consts.TokCapability, "+", "+", consts.TokControl, "+"}
)

func New(conn *bus.Connection, buses halcore.I2CBusFactory, pins halcore.PinFactory, uarts halcore.UARTFactory) *Service {
	return &Service{
		conn:       conn,
		buses:      buses,
		pins:       pins,
		uarts:      uarts,
		workers:    map[string]*worker.MeasureWorker{},
		results:    make(chan halcore.Result, 64),
		adaptors:   map[string]halcore.Adaptor{},
		devices:    map[string]devEntry{},
		capToDev:   map[capKey]string{},
		nextCapID:  map[string]int{},
		devPeriod:  map[string]time.Duration{},
		devNextDue: map[string]time.Time{},
		gpioW:      gpioirq.New(64, 64),
		gpioCancel: map[string]func(){},
		uartW:      uartio.New(64),
		uartCancel: map[string]func(){},
		uartEcho:   map[string]bool{},
	}
}

func (s *Service) Run(ctx context.Context) {
	s.gpioW.Start(ctx)

	cfgSub := s.conn.Subscribe(topicConfigHAL)
	ctrlSub := s.conn.Subscribe(topicCtrl)
	defer s.conn.Unsubscribe(cfgSub)
	defer s.conn.Unsubscribe(ctrlSub)

	s.publishState("idle", "awaiting_config", nil)

	s.timer = time.NewTimer(time.Hour)
	if !s.timer.Stop() {
		util.DrainTimer(s.timer)
	}

	var gpioEv <-chan gpioirq.GPIOEvent = s.gpioW.Events()
	var uartEv <-chan uartio.Event = s.uartW.Events() // NEW

	for {
		// arm timer
		if next := s.earliestDevDue(); next.IsZero() {
			util.ResetTimer(s.timer, time.Hour)
		} else {
			util.ResetTimer(s.timer, time.Until(next))
		}

		select {
		case <-ctx.Done():
			s.publishState("stopped", "context_cancelled", nil)
			for _, c := range s.gpioCancel {
				c()
			}
			for _, c := range s.uartCancel {
				c()
			}
			return

		case msg := <-cfgSub.Channel():
			cfg, ok := msg.Payload.(types.HALConfig)
			if !ok {
				s.publishState("error", "config_wrong_type", nil)
				continue
			}
			if err := s.applyConfig(ctx, cfg); err != nil {
				s.publishState("error", "apply_config_failed", err)
				continue
			}
			s.publishState("ready", "configured", nil)

		case msg := <-ctrlSub.Channel():
			// unchanged except for new capability kind handled by adaptors
			if len(msg.Topic) < 6 {
				continue
			}
			kind, _ := msg.Topic[2].(string)
			idNum, ok := asInt(msg.Topic[3])
			if !ok || kind == "" {
				s.replyErr(msg, halerr.ErrInvalidCapAddr.Error())
				continue
			}
			key := capKey{kind: kind, id: idNum}
			devID, ok := s.capToDev[key]
			if !ok {
				s.replyErr(msg, halerr.ErrUnknownCap.Error())
				continue
			}
			method, _ := msg.Topic[5].(string)

			switch method {
			case consts.CtrlReadNow:
				if s.submitMeasure(devID, true) {
					s.bumpDevNext(devID, time.Now())
					s.conn.Reply(msg, types.ReadNowAck{OK: true}, false)
				} else {
					s.replyErr(msg, halerr.ErrBusy.Error())
				}
			case consts.CtrlSetRate:
				if p, ok := msg.Payload.(types.SetRate); ok && p.Period > 0 {
					// Clamp to 200 ms .. 1 h
					s.devPeriod[devID] = util.ClampDuration(p.Period, 200*time.Millisecond, time.Hour)
					s.bumpDevNext(devID, time.Now())
					s.conn.Reply(msg, types.SetRateAck{OK: true, Period: s.devPeriod[devID]}, false)
				} else {
					s.replyErr(msg, halerr.ErrInvalidPeriod.Error())
				}
			default:
				ent := s.devices[devID]
				if ent.adaptor == nil {
					s.replyErr(msg, halerr.ErrNoAdaptor.Error())
					continue
				}
				if res, err := ent.adaptor.Control(kind, method, msg.Payload); err == nil {
					// Successful device-specific control: reply with the typed result directly.
					s.conn.Reply(msg, res, false)
					// Optional TX echo for UART writes with typed payload.
					if kind == consts.KindUART && method == "write" && s.uartEcho[devID] {
						if w, ok := msg.Payload.(types.UARTWrite); ok {
							s.uartW.EmitTX(devID, w.Data)
						}
					}
				} else {
					if err == halcore.ErrUnsupported {
						s.replyErr(msg, halerr.ErrUnsupported.Error())
					} else {
						s.replyErr(msg, err.Error())
					}
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

		case ev := <-uartEv: // NEW
			s.handleUARTEvent(ev)
		}
	}
}

func (s *Service) applyConfig(ctx context.Context, cfg types.HALConfig) error {
	seen := map[string]struct{}{}

	for i := range cfg.Devices {
		d := &cfg.Devices[i]
		seen[d.ID] = struct{}{}

		if _, exists := s.devices[d.ID]; exists {
			continue
		}

		b, ok := registry.Lookup(d.Type)
		if !ok {
			continue
		}

		out, err := b.Build(registry.BuildInput{
			Ctx:        ctx,
			Buses:      s.buses,
			Pins:       s.pins,
			UARTs:      s.uarts, // NEW
			DeviceID:   d.ID,
			Type:       d.Type,
			ParamsJSON: d.Params,
			BusRefType: d.BusRef.Type,
			BusRefID:   d.BusRef.ID,
		})
		if err != nil {
			continue
		}

		if out.BusID != "" {
			if _, ok := s.workers[out.BusID]; !ok {
				w := worker.New(halcore.WorkerConfig{}, s.results)
				w.Start(ctx)
				s.workers[out.BusID] = w
			}
		}

		ad := out.Adaptor
		s.adaptors[d.ID] = ad
		entry := devEntry{adaptor: ad, busID: out.BusID, caps: map[string]int{}}

		for _, ci := range ad.Capabilities() {
			id := s.nextCapID[ci.Kind]
			s.nextCapID[ci.Kind]++

			entry.caps[ci.Kind] = id
			s.capToDev[capKey{kind: ci.Kind, id: id}] = d.ID

			s.pubRet(ci.Kind, id, consts.TokInfo, ci.Info)
			s.pubRet(ci.Kind, id, consts.TokState,
				types.CapabilityState{
					Link: types.LinkUp,
					TS:   time.Now(),
				})
		}
		s.devices[d.ID] = entry

		if out.SampleEvery > 0 {
			// Clamp to 200 ms .. 1 h for safety.
			p := util.ClampDuration(out.SampleEvery, 200*time.Millisecond, time.Hour)
			s.devPeriod[d.ID] = p
			// First reading shortly after configuration.
			s.devNextDue[d.ID] = time.Now().Add(200 * time.Millisecond)
		}

		// GPIO IRQ registration
		if out.IRQ != nil && out.IRQ.Pin != nil {
			cancel, err := s.gpioW.RegisterInput(out.IRQ.DevID, out.IRQ.Pin, out.IRQ.Edge, out.IRQ.DebounceMS, out.IRQ.Invert)
			if err == nil {
				s.gpioCancel[d.ID] = cancel
			}
		}

		// UART reader registration
		if out.UART != nil && out.UART.Port != nil {
			cancel, err := s.uartW.Register(ctx, uartio.ReaderCfg{
				DevID:         out.UART.DevID,
				Port:          out.UART.Port,
				Mode:          out.UART.Mode,
				MaxFrame:      out.UART.MaxFrame,
				IdleFlush:     time.Duration(out.UART.IdleFlushMS) * time.Millisecond,
				PublishTXEcho: out.UART.PublishTXEcho,
			})
			if err == nil {
				s.uartCancel[d.ID] = cancel
				s.uartEcho[d.ID] = out.UART.PublishTXEcho
			}
		}
	}

	// Tidy-up devices not in config
	for devID, ent := range s.devices {
		if _, ok := seen[devID]; ok {
			continue
		}
		for kind, id := range ent.caps {
			s.pubRet(kind, id, consts.TokInfo, nil)
			s.pubRet(kind, id, consts.TokState, types.CapabilityState{Link: types.LinkDown, TS: time.Now()})
			delete(s.capToDev, capKey{kind: kind, id: id})
		}
		if c, ok := s.gpioCancel[devID]; ok {
			c()
			delete(s.gpioCancel, devID)
		}
		if c, ok := s.uartCancel[devID]; ok {
			c()
			delete(s.uartCancel, devID)
		}
		delete(s.devices, devID)
		delete(s.adaptors, devID)
		delete(s.devPeriod, devID)
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
	period := s.devPeriod[devID]
	if period <= 0 {
		period = 200 * time.Millisecond
	}
	// Defensive clamp.
	period = util.ClampDuration(period, 200*time.Millisecond, time.Hour)
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

func (s *Service) handleUARTEvent(ev uartio.Event) {
	ent, ok := s.devices[ev.DevID]
	if !ok {
		return
	}
	if id, ok := ent.caps[consts.KindUART]; ok {
		evt := types.UARTEvent{
			Dir: func() types.UARTDir {
				if ev.Dir == "tx" {
					return types.UARTTx
				}
				return types.UARTRx
			}(),
			Data: append([]byte(nil), ev.Data...),
			N:    len(ev.Data),
			TS:   ev.TS,
		}
		s.conn.Publish(s.conn.NewMessage(
			capTopicInt(consts.KindUART, id, consts.TokEvent), evt, false))
		s.pubRet(consts.KindUART, id, consts.TokState,
			types.CapabilityState{Link: types.LinkUp, TS: ev.TS})
	}
}

func (s *Service) handleResult(r halcore.Result) {
	ent, ok := s.devices[r.ID]
	if !ok {
		return
	}
	now := time.Now()

	if r.Err != nil {
		for kind, id := range ent.caps {
			s.pubRet(kind, id, consts.TokState, types.CapabilityState{
				Link:  types.LinkDegraded,
				TS:    now,
				Error: r.Err.Error(),
			})
		}
		return
	}
	for _, rd := range r.Sample {
		id, ok := ent.caps[rd.Kind]
		if !ok {
			continue
		}
		s.conn.Publish(s.conn.NewMessage(
			capTopicInt(rd.Kind, id, consts.TokValue),
			rd.Payload,
			false,
		))
		s.pubRet(rd.Kind, id, consts.TokState, types.CapabilityState{Link: types.LinkUp, TS: now})
	}
}

func (s *Service) handleGPIOEvent(ev gpioirq.GPIOEvent) {
	ent, ok := s.devices[ev.DevID]
	if !ok {
		return
	}
	ts := ev.TS

	// Path 1: devices that expose a GPIO capability -> publish event + retained state
	if id, ok := ent.caps[consts.KindGPIO]; ok {
		// Event (non-retained)
		edge := map[halcore.Edge]types.Edge{
			halcore.EdgeNone:    types.EdgeNone,
			halcore.EdgeRising:  types.EdgeRising,
			halcore.EdgeFalling: types.EdgeFalling,
			halcore.EdgeBoth:    types.EdgeBoth,
		}[ev.Edge]
		// Event (non-retained)
		s.conn.Publish(s.conn.NewMessage(
			capTopicInt(consts.KindGPIO, id, consts.TokEvent),
			types.GPIOEvent{Edge: edge, Level: uint8(ev.Level), TS: ts},
			false,
		))
		// State (retained)
		s.pubRet(consts.KindGPIO, id, consts.TokState,
			types.GPIOState{Link: types.LinkUp, Level: uint8(ev.Level), TS: ts})
		return
	}

	// Path 2: non-GPIO devices that registered an IRQ (e.g. LTC4015 SMBALERT)
	if _, hasIRQ := s.gpioCancel[ev.DevID]; hasIRQ {
		if ev.Edge != halcore.EdgeFalling {
			return
		}

		// Best-effort immediate read; worker handles back-pressure/priorities.
		_ = s.submitMeasure(ev.DevID, true)
		s.bumpDevNext(ev.DevID, ev.TS)
	}
}

// ---- bus helpers & utils ----

func (s *Service) publishState(level, status string, err error) {
	pl := types.HALState{Level: level, Status: status, TS: time.Now()}
	if err != nil {
		pl.Error = err.Error()
	}
	s.conn.Publish(s.conn.NewMessage(bus.Topic{consts.TokHAL, consts.TokState}, pl, true))
}

func (s *Service) replyErr(req *bus.Message, code string) {
	if len(req.ReplyTo) == 0 {
		return
	}
	if code == "" {
		code = "error"
	}
	s.conn.Reply(req, types.ErrorReply{OK: false, Error: code}, false)
}

func capTopicInt(kind string, id int, suffix string) bus.Topic {
	return bus.Topic{consts.TokHAL, consts.TokCapability, kind, id, suffix}
}

func (s *Service) pubRet(kind string, id int, suffix string, p any) {
	s.conn.Publish(s.conn.NewMessage(capTopicInt(kind, id, suffix), p, true))
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
