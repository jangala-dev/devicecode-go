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
	probe    uartxProbe

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
	s.probe.start(d.id, u, rxR, txR)

	for {
		made := false
		rxMade := false
		rxBackpressure := false

		// UART RX -> rxRing.
		//
		// RX is the only lossy edge in this chain. Drain the UARTX software RX
		// ring until it is empty, or until the session ring applies real
		// back-pressure. Do not switch to TX merely because some RX bytes were
		// published: during a peer chunk, the remote UART keeps sending and the
		// interrupt-side ring only has short-latency elasticity.
		for {
			p1, p2 := rxR.WriteAcquire()
			if len(p1) == 0 {
				s.probe.rxRingFull(d.id, u, rxR, txR)
				rxBackpressure = true
				break
			}
			n1 := u.TryRead(p1)
			if n1 == 0 {
				break
			}
			if n1 < len(p1) {
				rxR.WriteCommit(n1)
				s.probe.afterRX(d.id, u, rxR, txR, n1)
				made = true
				rxMade = true
				continue
			}
			n2 := 0
			if len(p2) > 0 {
				n2 = u.TryRead(p2)
			}
			rxR.WriteCommit(n1 + n2)
			s.probe.afterRX(d.id, u, rxR, txR, n1+n2)
			made = true
			rxMade = true
		}

		if rxBackpressure {
			// Downstream RX is full. Do not spin on UART readability, and do not rely
			// on an explicit scheduler yield. If there is no outbound work, block on
			// the only two edges that can make progress: the protocol consumer freeing
			// RX space, or the application producing TX work. If outbound work exists,
			// allow one small TX escape hatch; this lets a writer blocked in writeLine
			// finish, after which the same application goroutine can read and free
			// rxRing.
			made = false
			if txR.Available() == 0 {
				select {
				case <-s.ctx.Done():
					return
				case <-rxR.Writable():
				case <-txR.Readable():
				}
				continue
			}
		} else if rxMade {
			// RX made progress and the downstream ring still had room. Re-check RX
			// immediately before considering TX. This preserves the serial worker
			// as the short-latency drain for the UARTX ISR ring without involving a
			// scheduler hint.
			continue
		}

		// txRing -> UART TX.  Transmit under a small per-activation budget so
		// retained publications or diagnostic chatter cannot monopolise this
		// worker while the peer is sending a long chunk.  Under RX back-pressure,
		// the same budget also acts as a deadlock escape hatch for a writer whose
		// reader cannot run until writeLine completes.
		const txBudgetPerPass = 64
		txBudget := txBudgetPerPass
		for txBudget > 0 {
			p1, p2 := txR.ReadAcquire()
			if len(p1) == 0 {
				break
			}
			if len(p1) > txBudget {
				p1 = p1[:txBudget]
			}
			n1 := u.TryWrite(p1)
			if n1 == 0 {
				break
			}
			txBudget -= n1
			if n1 < len(p1) || txBudget == 0 {
				txR.ReadRelease(n1)
				s.probe.afterTX(d.id, u, rxR, txR, n1)
				made = true
				break
			}
			n2 := 0
			if len(p2) > 0 && txBudget > 0 {
				if len(p2) > txBudget {
					p2 = p2[:txBudget]
				}
				n2 = u.TryWrite(p2)
				txBudget -= n2
			}
			txR.ReadRelease(n1 + n2)
			s.probe.afterTX(d.id, u, rxR, txR, n1+n2)
			made = true
			if n2 == 0 || txBudget == 0 {
				break
			}
		}

		if made {
			continue
		}

		s.probe.periodic(d.id, u, rxR, txR)

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
