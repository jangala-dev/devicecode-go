package serial_raw

import (
	"context"
	"sync/atomic"
	"time"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/types"
	"devicecode-go/x/shmring"
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

	// Single worker (reactor) for the port.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
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

		// --- Device-level hygiene: drain spurious RX before signalling link up ---
		// Discard any pre-existing or immediately-arriving bytes on the UART RX path.
		// Uses a short quiet window so this remains bounded and non-blocking.
		{
			const quiet = 5 * time.Millisecond     // time with no bytes before we stop
			const maxTotal = 15 * time.Millisecond // absolute cap as a safeguard

			tmp := make([]byte, 64)
			tStart := time.Now()
			tQuiet := time.Now().Add(quiet)

			for {
				// Non-blocking attempt to pull any pending bytes.
				if n := d.port.TryRead(tmp); n > 0 {
					// Extend the quiet window after activity.
					tQuiet = time.Now().Add(quiet)
				} else {
					// No bytes right now. If we have been quiet long enough, or we have
					// reached the absolute bound, stop draining.
					now := time.Now()
					if now.After(tQuiet) || now.Sub(tStart) >= maxTotal {
						break
					}
					// Wait for either a UART RX edge or a very short back-off, then re-check.
					select {
					case <-d.port.Readable():
					case <-time.After(time.Millisecond):
					}
				}
			}
		}
		// --- end hygiene ---

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

func (d *Device) reactor(s *session) {
	defer close(s.done)

	u := d.port
	rxR := s.rxRing // UART -> app
	txR := s.txRing // app  -> UART

	for {
		made := false

		// UART RX -> rxRing (use spans; fill p1 completely before p2)
		for {
			p1, p2 := rxR.WriteAcquire()
			if len(p1) == 0 {
				break
			}
			n1 := u.TryRead(p1)
			if n1 == 0 {
				break
			}
			if n1 < len(p1) {
				rxR.WriteCommit(n1)
				made = true
				continue
			}
			n2 := 0
			if len(p2) > 0 {
				n2 = u.TryRead(p2)
			}
			rxR.WriteCommit(n1 + n2)
			made = true
		}

		// txRing -> UART TX (use spans; drain p1 completely before p2)
		for {
			p1, p2 := txR.ReadAcquire()
			if len(p1) == 0 {
				break
			}
			n1 := u.TryWrite(p1)
			if n1 == 0 {
				break
			}
			if n1 < len(p1) {
				txR.ReadRelease(n1)
				made = true
				continue
			}
			n2 := 0
			if len(p2) > 0 {
				n2 = u.TryWrite(p2)
			}
			txR.ReadRelease(n1 + n2)
			made = true
		}

		if made {
			continue
		}

		// Idle: wait for any edge, then re-check.
		select {
		case <-s.ctx.Done():
			return
		case <-u.Readable():
		case <-u.Writable():
		case <-rxR.Writable():
		case <-txR.Readable():
		}
	}
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
