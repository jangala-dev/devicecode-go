package serial_raw

import (
	"context"
	"sync/atomic"
	"time"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/types"
	"devicecode-go/x/fmtx"
	"devicecode-go/x/shmring"
)

type Params struct {
	Bus    string
	Domain string
	Name   string
	Baud   uint32
	RXSize int // power of two; default 512
	TXSize int // power of two; default 512
}

type Device struct {
	id     string
	a      core.CapAddr
	res    core.Resources
	params Params

	busID string // <- BUS IDENTIFIER TO CLAIM

	port core.SerialPort
	cfgB core.SerialConfigurator
	cfgF core.SerialFormatConfigurator

	sess  *session
	snCtr atomic.Uint32
}

type session struct {
	id     uint32
	rxH    shmring.Handle
	rxR    *shmring.Ring
	txH    shmring.Handle
	txR    *shmring.Ring
	quit   chan struct{}
	rxDone chan struct{}
	txDone chan struct{}
}

func Builder() core.Builder { return builder{} }

type builder struct{}

func (builder) Build(ctx context.Context, in core.BuilderInput) (core.Device, error) {
	// Strongly-typed params only.
	var p Params
	switch v := in.Params.(type) {
	case Params:
		p = v
	case *Params:
		if v == nil {
			return nil, errcode.InvalidParams
		}
		p = *v
	default:
		return nil, errcode.InvalidParams
	}

	if p.Bus == "" {
		return nil, errcode.InvalidParams
	}
	if p.RXSize <= 0 {
		p.RXSize = 512
	}
	if p.TXSize <= 0 {
		p.TXSize = 512
	}
	if (p.RXSize&(p.RXSize-1)) != 0 || (p.TXSize&(p.TXSize-1)) != 0 {
		return nil, errcode.InvalidParams
	}

	if p.Domain == "" || p.Name == "" {
		return nil, errcode.InvalidParams
	}

	return &Device{
		id:     in.ID,
		a:      core.CapAddr{Domain: p.Domain, Kind: string(types.KindSerial), Name: p.Name},
		res:    in.Res,
		params: p,
		busID:  p.Bus,
	}, nil
}

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
	// Claim configured bus exclusively
	p, err := d.res.Reg.ClaimSerial(d.id, core.ResourceID(d.busID))
	if err != nil {
		return err
	}
	d.port = p
	if c, ok := p.(core.SerialConfigurator); ok {
		d.cfgB = c
	}
	if f, ok := p.(core.SerialFormatConfigurator); ok {
		d.cfgF = f
	}

	if d.cfgB != nil && d.params.Baud > 0 {
		_ = d.cfgB.SetBaudRate(d.params.Baud)
	}

	// Publish initial LinkDown status while we are inactive.
	d.res.Pub.Emit(core.Event{
		Addr: core.CapAddr{Domain: d.a.Domain, Kind: string(types.KindSerial), Name: d.a.Name},
		TS:   time.Now().UnixNano(),
		Err:  "initialising",
	})
	return nil
}

func (d *Device) Close() error {
	if d.sess != nil {
		d.stopSession()
	}
	// Serial ports are long-lived; provider can manage underlying lifetime.
	if d.res.Reg != nil {
		d.res.Reg.ReleaseSerial(d.id, core.ResourceID(d.busID))
	}
	return nil
}

type sessionOpenReq struct {
	RXSize int
	TXSize int
}
type sessionOpenRep struct {
	SessionID uint32
	RXHandle  uint32
	TXHandle  uint32
}
type sessionCloseReq struct {
	SessionID uint32
}

func (d *Device) Control(cap core.CapAddr, verb string, payload any) (core.EnqueueResult, error) {
	switch verb {
	case "session_open":
		// Strong payload; nil uses defaults.
		req := sessionOpenReq{RXSize: d.params.RXSize, TXSize: d.params.TXSize}
		switch v := payload.(type) {
		case nil:
			// keep defaults
		case types.SerialSessionOpen:
			if v.RXSize != 0 {
				req.RXSize = v.RXSize
			}
			if v.TXSize != 0 {
				req.TXSize = v.TXSize
			}
		case *types.SerialSessionOpen:
			if v != nil {
				if v.RXSize != 0 {
					req.RXSize = v.RXSize
				}
				if v.TXSize != 0 {
					req.TXSize = v.TXSize
				}
			}
		default:
			return core.EnqueueResult{OK: false, Error: errcode.InvalidPayload}, nil
		}
		if (req.RXSize&(req.RXSize-1)) != 0 || (req.TXSize&(req.TXSize-1)) != 0 {
			return core.EnqueueResult{OK: false, Error: errcode.InvalidParams}, nil
		}
		if d.sess != nil {
			return core.EnqueueResult{OK: false, Error: errcode.Conflict}, nil
		}
		d.startSession(req.RXSize, req.TXSize)
		rep := sessionOpenRep{SessionID: d.sess.id, RXHandle: uint32(d.sess.rxH), TXHandle: uint32(d.sess.txH)}
		d.res.Pub.Emit(core.Event{
			Addr:     core.CapAddr{Domain: d.a.Domain, Kind: string(types.KindSerial), Name: d.a.Name},
			Payload:  rep,
			TS:       time.Now().UnixNano(),
			IsEvent:  true,
			EventTag: "session_opened",
		})
		return core.EnqueueResult{OK: true}, nil

	case "session_close":
		// Accept nil or explicit empty struct.
		switch payload.(type) {
		case nil, types.SerialSessionClose, *types.SerialSessionClose:
			// ok
		default:
			return core.EnqueueResult{OK: false, Error: errcode.InvalidPayload}, nil
		}
		if d.sess == nil {
			return core.EnqueueResult{OK: true}, nil
		}
		d.stopSession()
		d.res.Pub.Emit(core.Event{
			Addr: d.a, TS: time.Now().UnixNano(),
			IsEvent: true, EventTag: "session_closed",
		})
		d.res.Pub.Emit(core.Event{
			Addr: d.a, TS: time.Now().UnixNano(),
			Err: "session_closed",
		})
		return core.EnqueueResult{OK: true}, nil

	case "set_baud":
		if d.cfgB == nil {
			return core.EnqueueResult{OK: false, Error: errcode.Unsupported}, nil
		}
		switch v := payload.(type) {
		case types.SerialSetBaud:
			_ = d.cfgB.SetBaudRate(v.Baud)
			return core.EnqueueResult{OK: true}, nil
		case *types.SerialSetBaud:
			if v == nil {
				return core.EnqueueResult{OK: false, Error: errcode.InvalidPayload}, nil
			}
			_ = d.cfgB.SetBaudRate(v.Baud)
			return core.EnqueueResult{OK: true}, nil
		default:
			return core.EnqueueResult{OK: false, Error: errcode.InvalidPayload}, nil
		}

	case "set_format":
		// payload: {databits:uint8, stopbits:uint8, parity:"none"|"even"|"odd"}
		if d.cfgF == nil {
			return core.EnqueueResult{OK: false, Error: errcode.Unsupported}, nil
		}
		var req types.SerialSetFormat
		switch v := payload.(type) {
		case types.SerialSetFormat:
			req = v
		case *types.SerialSetFormat:
			if v == nil {
				return core.EnqueueResult{OK: false, Error: errcode.InvalidPayload}, nil
			}
			req = *v
		default:
			return core.EnqueueResult{OK: false, Error: errcode.InvalidPayload}, nil
		}
		if req.DataBits == 0 || req.StopBits == 0 {
			return core.EnqueueResult{OK: false, Error: errcode.InvalidParams}, nil
		}
		var par string
		switch req.Parity {
		case types.ParityEven:
			par = "even"
		case types.ParityOdd:
			par = "odd"
		default:
			par = "none"
		}
		if err := d.cfgF.SetFormat(req.DataBits, req.StopBits, par); err != nil {

			return core.EnqueueResult{OK: false, Error: errcode.MapDriverErr(err)}, nil
		}
		return core.EnqueueResult{OK: true}, nil

	default:
		return core.EnqueueResult{OK: false, Error: errcode.Unsupported}, nil
	}
}

func (d *Device) startSession(rxSize, txSize int) {
	rxh, rxr := shmring.New(rxSize)
	txh, txr := shmring.New(txSize)
	s := &session{
		id:     d.snCtr.Add(1),
		rxH:    rxh,
		rxR:    rxr,
		txH:    txh,
		txR:    txr,
		quit:   make(chan struct{}),
		rxDone: make(chan struct{}),
		txDone: make(chan struct{}),
	}
	d.sess = s
	go d.rxLoop(s)
	go d.txLoop(s)
	// LinkUp now that session is ready
	d.res.Pub.Emit(core.Event{
		Addr: d.a, TS: time.Now().UnixNano(),
		IsEvent: true, EventTag: "link_up",
	})
}

func (d *Device) stopSession() {
	s := d.sess
	if s == nil {
		return
	}
	close(s.quit)
	<-s.rxDone
	<-s.txDone
	shmring.Close(s.rxH)
	shmring.Close(s.txH)
	d.sess = nil
}

func (d *Device) rxLoop(s *session) {
	defer close(s.rxDone)
	tmp := make([]byte, 256)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// cancellation goroutine
	go func() {
		<-s.quit
		cancel()
	}()

	for {
		n, err := d.port.RecvSomeContext(ctx, tmp)
		if n > 0 {
			w := s.rxR.WriteFrom(tmp[:n])
			if w < n {
				fmtx.Printf("[serial %s] rx overflow: lost=%d\n", d.id, n-w)
			}
		}
		if err != nil {
			// Return on context cancellation; otherwise brief back-off.
			if ctx.Err() != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		// Early-exit if quit is closed and no blocking read is pending.
		select {
		case <-s.quit:
			return
		default:
		}
	}
}

func (d *Device) txLoop(s *session) {
	defer close(s.txDone)
	tmp := make([]byte, 256)
	for {
		// Block until there is data or quit
		if s.txR.Available() == 0 {
			select {
			case <-s.txR.Readable():
			case <-s.quit:
				return
			}
		}
		// Drain available data.
		for {
			n := s.txR.ReadInto(tmp)
			if n == 0 {
				break
			}
			_, _ = d.port.Write(tmp[:n]) // blocking write
		}
		select {
		case <-s.quit:
			return
		default:
		}
	}
}

func init() { core.RegisterBuilder("serial_raw", Builder()) }
