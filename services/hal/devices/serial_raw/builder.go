package serial_raw

import (
	"context"
	"runtime"
	"sync/atomic"
	"time"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/otadiag"
	"devicecode-go/types"
	"devicecode-go/x/shmring"
	"devicecode-go/x/strconvx"
)

// ---- Parameters ----

type Params struct {
	Bus    string
	Domain string
	Name   string
	Baud   uint32
	RXSize int // power of two; default 512 if zero in SessionOpen
	TXSize int // power of two; default 512 if zero in SessionOpen
}

const (
	serialRawPumpRXBudget = 256
	serialRawPumpTXBudget = 256
	serialRawPumpGapWarn  = 20 * time.Millisecond
)

// ---- Device ----

type Device struct {
	id  string
	a   core.CapAddr
	res core.Resources

	busID string
	port  core.SerialPort

	cfgB core.SerialConfigurator
	cfgF core.SerialFormatConfigurator

	params Params

	sess  *session
	snCtr atomic.Uint32
}

type session struct {
	id uint32

	// Rings (SPSC); handles are exported to clients.
	rxHandle shmring.Handle
	rxRing   *shmring.Ring
	txHandle shmring.Handle
	txRing   *shmring.Ring

	// Reactor-owned observability. Single writer only.
	rxRingFull      uint32
	rxLogAt         time.Time
	rxLogHits       uint32
	rxPressureAt    time.Time
	rxPressureHits  uint32
	rxPumpGapAt     time.Time
	rxPumpGapHits   uint32
	lastRXPumpAt    time.Time
	lastRXPumpMoved int
	lastRXPumpDurMS int
	lastRXPumpGapMS int

	// Single worker (reactor) for the port.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

type serialRXDiagnostics interface {
	RXBuffered() int
	RXBufferCap() int
}

type serialRXErrorDiagnostics interface {
	RXDropCount() uint32
	RXOverrunCount() uint32
	RXBreakCount() uint32
	RXParityCount() uint32
	RXFramingCount() uint32
}

// ---- Builder registration ----

func Builder() core.Builder { return builder{} }

func init() { core.RegisterBuilder("serial_raw", Builder()) }

type builder struct{}

func (builder) Build(ctx context.Context, in core.BuilderInput) (core.Device, error) {
	p, ok := in.Params.(Params)
	if !ok {
		return nil, errcode.InvalidParams
	}
	if p.Bus == "" || p.Domain == "" || p.Name == "" {
		return nil, errcode.InvalidParams
	}

	// Claim the serial bus exclusively.
	sp, err := in.Res.Reg.ClaimSerial(in.ID, core.ResourceID(p.Bus))
	if err != nil {
		return nil, err
	}

	d := &Device{
		id:    in.ID,
		a:     core.CapAddr{Domain: p.Domain, Kind: types.KindSerial, Name: p.Name},
		res:   in.Res,
		busID: p.Bus,
		port:  sp,
		params: Params{
			Bus:    p.Bus,
			Domain: p.Domain,
			Name:   p.Name,
			Baud:   p.Baud,
			RXSize: p.RXSize,
			TXSize: p.TXSize,
		},
	}

	// Optional configurators.
	if c, ok := sp.(core.SerialConfigurator); ok {
		d.cfgB = c
	}
	if f, ok := sp.(core.SerialFormatConfigurator); ok {
		d.cfgF = f
	}

	return d, nil
}

// ---- core.Device ----

func (d *Device) ID() string { return d.id }

func (d *Device) Capabilities() []core.CapabilitySpec {
	info := types.SerialInfo{Bus: d.busID, Baud: d.params.Baud}
	return []core.CapabilitySpec{{
		Domain: d.a.Domain,
		Kind:   types.KindSerial,
		Name:   d.a.Name,
		Info:   types.Info{Driver: "serial_raw", Detail: info},
	}}
}

func (d *Device) Init(ctx context.Context) error {
	// Apply initial baud only if explicitly provided.
	if d.cfgB != nil && d.params.Baud > 0 {
		_ = d.cfgB.SetBaudRate(d.params.Baud)
	}

	// Publish initial degraded status while inactive.
	d.res.Pub.Emit(core.Event{
		Addr: d.a, Err: "initialising",
	})
	return nil
}

func (d *Device) Close() error {
	if d.sess != nil {
		d.stopSession()
	}
	if d.res.Reg != nil {
		d.res.Reg.ReleaseSerial(d.id, core.ResourceID(d.busID))
	}
	return nil
}

// ---- Controls ----

func (d *Device) Control(_ core.CapAddr, verb string, payload any) (core.EnqueueResult, error) {
	switch verb {
	case "session_open":
		req, code := core.As[types.SerialSessionOpen](payload) // zero value => apply defaults
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}

		if d.sess != nil {
			return core.EnqueueResult{OK: false, Error: errcode.Conflict}, nil
		}

		rxSize, txSize := req.RXSize, req.TXSize
		if rxSize == 0 {
			rxSize = coalescePow2(d.params.RXSize, 512)
		}
		if txSize == 0 {
			txSize = coalescePow2(d.params.TXSize, 512)
		}
		if !isPow2(rxSize) || !isPow2(txSize) || rxSize < 2 || txSize < 2 {
			return core.EnqueueResult{OK: false, Error: errcode.InvalidParams}, nil
		}

		d.startSession(rxSize, txSize)
		println(
			"[serial-raw]", "session_open",
			"uart", d.a.Name,
			"rx_size", strconvx.Itoa(rxSize),
			"tx_size", strconvx.Itoa(txSize),
		)

		rep := types.SerialSessionOpened{
			SessionID: d.sess.id,
			RXHandle:  uint32(d.sess.rxHandle),
			TXHandle:  uint32(d.sess.txHandle),
		}
		d.res.Pub.Emit(core.Event{
			Addr: d.a, Payload: rep, EventTag: "session_opened",
		})
		d.res.Pub.Emit(core.Event{
			Addr: d.a, EventTag: "link_up",
		})

		return core.EnqueueResult{OK: true}, nil

	case "session_close":
		// Accept zero-value payload (no fields)
		_, _ = core.As[types.SerialSessionClose](payload)
		if d.sess == nil {
			return core.EnqueueResult{OK: true}, nil
		}
		d.stopSession()
		d.res.Pub.Emit(core.Event{
			Addr: d.a, EventTag: "session_closed",
		})
		d.res.Pub.Emit(core.Event{
			Addr: d.a, Err: "session_closed",
		})
		return core.EnqueueResult{OK: true}, nil

	case "set_baud":
		if d.cfgB == nil {
			return core.EnqueueResult{OK: false, Error: errcode.Unsupported}, nil
		}
		req, code := core.As[types.SerialSetBaud](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		_ = d.cfgB.SetBaudRate(req.Baud)
		return core.EnqueueResult{OK: true}, nil

	case "set_format":
		if d.cfgF == nil {
			return core.EnqueueResult{OK: false, Error: errcode.Unsupported}, nil
		}
		req, code := core.As[types.SerialSetFormat](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		if req.DataBits == 0 || req.StopBits == 0 {
			return core.EnqueueResult{OK: false, Error: errcode.InvalidParams}, nil
		}
		par := "none"
		switch req.Parity {
		case types.ParityEven:
			par = "even"
		case types.ParityOdd:
			par = "odd"
		}
		if err := d.cfgF.SetFormat(req.DataBits, req.StopBits, par); err != nil {
			return core.EnqueueResult{OK: false, Error: errcode.MapDriverErr(err)}, nil
		}
		return core.EnqueueResult{OK: true}, nil

	default:
		return core.EnqueueResult{OK: false, Error: errcode.Unsupported}, nil
	}
}

// ---- Session lifecycle ----

func (d *Device) startSession(rxSize, txSize int) {
	rxh, rxr := shmring.NewRegistered(rxSize)
	txh, txr := shmring.NewRegistered(txSize)

	ctx, cancel := context.WithCancel(context.Background())
	s := &session{
		id:       d.snCtr.Add(1),
		rxHandle: rxh,
		rxRing:   rxr,
		txHandle: txh,
		txRing:   txr,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	d.sess = s

	go d.reactor(s)
}

func (d *Device) stopSession() {
	s := d.sess
	if s == nil {
		return
	}
	s.cancel()
	<-s.done

	// Drop registry mappings (rings remain usable by any lingering client only
	// if it retained the pointer, which it should not; we treat handles as the contract).
	shmring.Close(s.rxHandle)
	shmring.Close(s.txHandle)

	d.sess = nil
}

// ---- Reactor (single goroutine) ----

func (d *Device) logRingFullChange(s *session, force bool) {
	const rxLogMinInterval = 1 * time.Second

	hits := s.rxRingFull

	if !force {
		now := time.Now()
		if now.Sub(s.rxLogAt) < rxLogMinInterval {
			return
		}
		if hits == s.rxLogHits {
			return
		}
		s.rxLogAt = now
	} else {
		s.rxLogAt = time.Now()
	}

	println(
		"[serial-raw]", "rx_ring_full",
		"uart", d.a.Name,
		"hits", strconvx.Utoa64(uint64(hits)),
		"ring_avail", strconvx.Itoa(s.rxRing.Available()),
		"ring_space", strconvx.Itoa(s.rxRing.Space()),
		"ring_cap", strconvx.Itoa(s.rxRing.Cap()),
	)
	s.rxLogHits = hits
}

func (d *Device) appendRXPumpFields(s *session, fields []otadiag.Field, now time.Time) []otadiag.Field {
	if !s.lastRXPumpAt.IsZero() {
		fields = append(fields, otadiag.KV("since_rx_pump_ms", int(now.Sub(s.lastRXPumpAt)/time.Millisecond)))
	}
	fields = append(fields,
		otadiag.KV("last_pump_moved", s.lastRXPumpMoved),
		otadiag.KV("last_pump_dur_ms", s.lastRXPumpDurMS),
	)
	if s.lastRXPumpGapMS >= 0 {
		fields = append(fields, otadiag.KV("last_pump_gap_ms", s.lastRXPumpGapMS))
	}
	return fields
}

func appendRXErrorFields(port core.SerialPort, fields []otadiag.Field) []otadiag.Field {
	diag, ok := port.(serialRXErrorDiagnostics)
	if !ok {
		return fields
	}
	return append(fields,
		otadiag.KV("rx_drops", diag.RXDropCount()),
		otadiag.KV("rx_overrun", diag.RXOverrunCount()),
		otadiag.KV("rx_break", diag.RXBreakCount()),
		otadiag.KV("rx_parity", diag.RXParityCount()),
		otadiag.KV("rx_framing", diag.RXFramingCount()),
	)
}

func (d *Device) logDriverPressure(s *session, force bool) {
	const minInterval = 1 * time.Second

	diag, ok := d.port.(serialRXDiagnostics)
	if !ok {
		return
	}
	used := diag.RXBuffered()
	capacity := diag.RXBufferCap()
	if capacity <= 0 || used < 0 {
		return
	}
	threshold := (capacity * 3) / 4
	if threshold < 1 {
		threshold = 1
	}
	if !force && used < threshold {
		return
	}

	hits := s.rxPressureHits + 1
	now := time.Now()
	if !force {
		if now.Sub(s.rxPressureAt) < minInterval {
			return
		}
	} else {
		now = time.Now()
	}
	s.rxPressureAt = now
	s.rxPressureHits = hits

	fields := []otadiag.Field{
		otadiag.KV("uart", d.a.Name),
		otadiag.KV("hits", strconvx.Utoa64(uint64(hits))),
		otadiag.KV("driver_used", used),
		otadiag.KV("driver_cap", capacity),
		otadiag.KV("ring_avail", s.rxRing.Available()),
		otadiag.KV("ring_space", s.rxRing.Space()),
		otadiag.KV("ring_cap", s.rxRing.Cap()),
	}
	fields = d.appendRXPumpFields(s, fields, now)
	fields = appendRXErrorFields(d.port, fields)
	otadiag.Event("[serial-raw]", "rx_driver_pressure", otadiag.XferNone, fields...)

	if !s.lastRXPumpAt.IsZero() && now.Sub(s.lastRXPumpAt) >= serialRawPumpGapWarn {
		d.logRXPumpGap(s, used, capacity, now)
	}
}

func (d *Device) logRXPumpGap(s *session, used, capacity int, now time.Time) {
	const minInterval = 1 * time.Second

	if now.Sub(s.rxPumpGapAt) < minInterval {
		return
	}
	s.rxPumpGapAt = now
	s.rxPumpGapHits++
	fields := []otadiag.Field{
		otadiag.KV("uart", d.a.Name),
		otadiag.KV("hits", strconvx.Utoa64(uint64(s.rxPumpGapHits))),
		otadiag.KV("driver_used", used),
		otadiag.KV("driver_cap", capacity),
		otadiag.KV("ring_avail", s.rxRing.Available()),
		otadiag.KV("ring_space", s.rxRing.Space()),
		otadiag.KV("ring_cap", s.rxRing.Cap()),
		otadiag.KV("since_rx_pump_ms", int(now.Sub(s.lastRXPumpAt)/time.Millisecond)),
		otadiag.KV("last_pump_moved", s.lastRXPumpMoved),
		otadiag.KV("last_pump_dur_ms", s.lastRXPumpDurMS),
		otadiag.KV("last_pump_gap_ms", s.lastRXPumpGapMS),
	}
	fields = appendRXErrorFields(d.port, fields)
	otadiag.Event("[serial-raw]", "rx_pump_gap", otadiag.XferNone, fields...)
}

func (s *session) noteRXPump(moved int, started time.Time) {
	if moved <= 0 {
		return
	}
	now := time.Now()
	gapMS := -1
	if !s.lastRXPumpAt.IsZero() {
		gapMS = int(started.Sub(s.lastRXPumpAt) / time.Millisecond)
	}
	s.lastRXPumpAt = now
	s.lastRXPumpMoved = moved
	s.lastRXPumpDurMS = int(now.Sub(started) / time.Millisecond)
	s.lastRXPumpGapMS = gapMS
	if s.lastRXPumpGapMS < 0 {
		s.lastRXPumpGapMS = 0
	}
	if s.lastRXPumpDurMS >= 5 {
		otadiag.Event(
			"[serial-raw]", "rx_pump_slow", otadiag.XferNone,
			otadiag.KV("moved", moved),
			otadiag.KV("dur_ms", s.lastRXPumpDurMS),
			otadiag.KV("gap_ms", s.lastRXPumpGapMS),
		)
	}
}

func (d *Device) pumpRX(s *session, u core.SerialPort, rxR *shmring.Ring, budget int) bool {
	started := time.Now()
	moved := 0

	defer func() {
		if moved > 0 {
			s.noteRXPump(moved, started)
		}
	}()

	for moved < budget {
		d.logDriverPressure(s, false)
		p1, p2 := rxR.WriteAcquire()
		if len(p1) == 0 {
			s.rxRingFull++
			break
		}

		remaining := budget - moved
		p1 = limitSpan(p1, remaining)
		n1 := u.TryRead(p1)
		if n1 == 0 {
			break
		}
		n := n1
		moved += n1
		if n1 < len(p1) {
			rxR.WriteCommit(n)
			break
		}

		remaining = budget - moved
		if remaining > 0 && len(p2) > 0 {
			p2 = limitSpan(p2, remaining)
			n2 := u.TryRead(p2)
			n += n2
			moved += n2
			if n2 < len(p2) {
				rxR.WriteCommit(n)
				break
			}
		}

		rxR.WriteCommit(n)
	}

	return moved > 0
}

func (d *Device) reactor(s *session) {
	defer close(s.done)

	u := d.port
	rxR := s.rxRing // UART -> app
	txR := s.txRing // app  -> UART

	for {
		made := false

		if d.pumpRX(s, u, rxR, serialRawPumpRXBudget) {
			made = true
		}

		if d.pumpTX(u, txR, serialRawPumpTXBudget) {
			made = true
		}

		if made {
			select {
			case <-s.ctx.Done():
				d.logRingFullChange(s, true)
				return
			default:
			}
			runtime.Gosched()
			continue
		}

		// Idle: wait for any edge, then re-check.
		d.logRingFullChange(s, false)
		select {
		case <-s.ctx.Done():
			d.logRingFullChange(s, true)
			return
		case <-u.Readable():
		case <-u.Writable():
		case <-rxR.Writable():
		case <-txR.Readable():
		}
	}
}

func (d *Device) pumpTX(u core.SerialPort, txR *shmring.Ring, budget int) bool {
	moved := 0

	for moved < budget {
		p1, p2 := txR.ReadAcquire()
		if len(p1) == 0 {
			break
		}

		remaining := budget - moved
		p1 = limitSpan(p1, remaining)
		n1 := u.TryWrite(p1)
		if n1 == 0 {
			break
		}
		n := n1
		moved += n1
		if n1 < len(p1) {
			txR.ReadRelease(n)
			break
		}

		remaining = budget - moved
		if remaining > 0 && len(p2) > 0 {
			p2 = limitSpan(p2, remaining)
			n2 := u.TryWrite(p2)
			n += n2
			moved += n2
			if n2 < len(p2) {
				txR.ReadRelease(n)
				break
			}
		}

		txR.ReadRelease(n)
	}

	return moved > 0
}

func limitSpan(p []byte, max int) []byte {
	if max <= 0 {
		return p[:0]
	}
	if len(p) > max {
		return p[:max]
	}
	return p
}

// ---- Helpers ----

func isPow2(n int) bool { return n > 0 && (n&(n-1)) == 0 }

func coalescePow2(v, d int) int {
	if v <= 0 {
		return d
	}
	if !isPow2(v) {
		return d
	}
	if v < 2 {
		return 2
	}
	return v
}
