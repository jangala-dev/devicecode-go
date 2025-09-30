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

	domain := p.Domain
	if domain == "" {
		domain = "io"
	}
	name := p.Name
	if name == "" {
		name = in.ID
	}

	return &Device{
		id:     in.ID,
		a:      core.CapAddr{Domain: domain, Kind: string(types.KindSerial), Name: name},
		res:    in.Res,
		params: p,
		busID:  p.Bus,
	}, nil
}

func (d *Device) ID() string { return d.id }

func (d *Device) Capabilities() []core.CapabilitySpec {
	// Provide discoverability for bus and initial baud without requiring new strong types.
	detail := map[string]any{"bus": d.busID}
	if d.params.Baud > 0 {
		detail["baud"] = d.params.Baud
	}
	return []core.CapabilitySpec{{
		Domain: d.a.Domain,
		Kind:   types.KindSerial,
		Name:   d.a.Name,
		Info:   types.Info{Driver: "serial_raw", Detail: detail},
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

	if d.cfgB != nil {
		if d.params.Baud > 0 {
			_ = d.cfgB.SetBaudRate(d.params.Baud)
		} else {
			_ = d.cfgB.SetBaudRate(115200) // default only when not explicitly set
		}
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
		req := sessionOpenReq{RXSize: d.params.RXSize, TXSize: d.params.TXSize}
		if m, ok := payload.(map[string]any); ok {
			if v, ok := m["RXSize"].(float64); ok {
				req.RXSize = int(v)
			}
			if v, ok := m["TXSize"].(float64); ok {
				req.TXSize = int(v)
			}
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
		case float64:
			_ = d.cfgB.SetBaudRate(uint32(v))
			return core.EnqueueResult{OK: true}, nil
		case uint32:
			_ = d.cfgB.SetBaudRate(v)
			return core.EnqueueResult{OK: true}, nil
		default:
			return core.EnqueueResult{OK: false, Error: errcode.InvalidPayload}, nil
		}

	case "set_format":
		// payload: {databits:uint8, stopbits:uint8, parity:"none"|"even"|"odd"}
		if d.cfgF == nil {
			return core.EnqueueResult{OK: false, Error: errcode.Unsupported}, nil
		}
		m, ok := payload.(map[string]any)
		if !ok {
			return core.EnqueueResult{OK: false, Error: errcode.InvalidPayload}, nil
		}
		var (
			db, sb uint8
			par    string
		)
		if v, ok := m["databits"].(float64); ok {
			db = uint8(v)
		}
		if v, ok := m["stopbits"].(float64); ok {
			sb = uint8(v)
		}
		if v, ok := m["parity"].(string); ok {
			par = v
		}
		if db == 0 || sb == 0 || par == "" {
			return core.EnqueueResult{OK: false, Error: errcode.InvalidParams}, nil
		}
		if err := d.cfgF.SetFormat(db, sb, par); err != nil {
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
		Addr: core.CapAddr{Domain: d.a.Domain, Kind: string(types.KindSerial), Name: d.a.Name},
		TS:   time.Now().UnixNano(),
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
