//go:build rp2040

package provider

import (
	"context"
	"sync"
	"time"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/hal/internal/provider/boards"
	"devicecode-go/services/hal/internal/provider/setups"
	"devicecode-go/x/mathx"
	"devicecode-go/x/ramp"
	"machine"

	uartx "github.com/jangala-dev/tinygo-uartx/uartx"
	"tinygo.org/x/drivers"
)

// Ensure the provider satisfies the contracts at compile time.
var _ core.ResourceRegistry = (*rp2Registry)(nil)

// -----------------------------------------------------------------------------
// GPIO handle
// -----------------------------------------------------------------------------

type rp2GPIO struct {
	p machine.Pin
	n int
}

func (r *rp2GPIO) Number() int { return r.n }

func (r *rp2GPIO) ConfigureInput(pull core.Pull) error {
	var mode machine.PinMode
	switch pull {
	case core.PullUp:
		mode = machine.PinInputPullup
	case core.PullDown:
		mode = machine.PinInputPulldown
	default:
		mode = machine.PinInput
	}
	r.p.Configure(machine.PinConfig{Mode: mode})
	return nil
}

func (r *rp2GPIO) ConfigureOutput(initial bool) error {
	r.p.Configure(machine.PinConfig{Mode: machine.PinOutput})
	r.p.Set(initial)
	return nil
}

func (r *rp2GPIO) Set(b bool) { r.p.Set(b) }
func (r *rp2GPIO) Get() bool  { return r.p.Get() }
func (r *rp2GPIO) Toggle() {
	if r.p.Get() {
		r.p.Low()
	} else {
		r.p.High()
	}
}

// -----------------------------------------------------------------------------
// PWM internals (RP2040)
// -----------------------------------------------------------------------------

// Local interface to avoid depending on an unexported concrete type in machine.
type pwmCtrl interface {
	Configure(cfg machine.PWMConfig) error
	Top() uint32
	Set(channel uint8, value uint32)
}

// Select controller handle for a given slice number (0..7).
func pwmGroupBySlice(slice uint8) pwmCtrl {
	switch slice {
	case 0:
		return machine.PWM0
	case 1:
		return machine.PWM1
	case 2:
		return machine.PWM2
	case 3:
		return machine.PWM3
	case 4:
		return machine.PWM4
	case 5:
		return machine.PWM5
	case 6:
		return machine.PWM6
	default:
		return machine.PWM7
	}
}

type sliceCfg struct {
	freqHz uint64
	users  int
}

// rp2PWM is the provider's per-pin PWM handle (channel-level).
type rp2PWM struct {
	mu sync.Mutex

	pin   int
	ctrl  pwmCtrl // controller for this pin's slice (PWM0..PWM7)
	chIdx uint8   // channel within controller: 0 => A, 1 => B
	slice int     // 0..7
	ch    rune    // 'A' or 'B'

	enabled bool

	// Logical config (per channel)
	reqTop uint16 // requested logical resolution (0..reqTop)
	freqHz uint64 // requested frequency

	// Hardware info (per slice/controller)
	hwTop uint32 // controller.Top() after Configure

	// Current logical level (0..reqTop)
	level uint16

	// Ramp state
	rampCancel chan struct{}
	rampAlive  bool

	// User accounting (slice-level): true once this handle has contributed
	// to slice users. Prevents double-counting on repeated Configure calls.
	registered bool
}

// caller holds lock
func (p *rp2PWM) setHW(logical uint16) {
	if p.hwTop == 0 || p.reqTop == 0 {
		return
	}
	logical = mathx.Min(logical, p.reqTop)
	// Scale from logical [0..reqTop] to hardware [0..hwTop].
	hw := (uint32(logical) * p.hwTop) / uint32(p.reqTop)
	p.ctrl.Set(p.chIdx, hw)
	p.level = logical
}

func (p *rp2PWM) Configure(freqHz uint64, top uint16) error {
	top = mathx.Max(top, 1)
	freqHz = mathx.Max(freqHz, 1)

	globalPWM.mu.Lock()
	defer globalPWM.mu.Unlock()

	sc := globalPWM.slice[p.slice]
	if sc == nil {
		sc = &sliceCfg{}
		globalPWM.slice[p.slice] = sc
	}

	if sc.users == 0 {
		// First writer configures the controller period for this slice.
		period := PeriodFromHz(freqHz)
		if err := p.ctrl.Configure(machine.PWMConfig{Period: period}); err != nil {
			return err
		}
		sc.freqHz = freqHz
		sc.users = 1
		p.registered = true
	} else {
		// Already in use. If this handle has not yet been counted, enforce
		// frequency compatibility and then count it. If it *has* been counted,
		// allow reconfiguration only if this is the sole user.
		if !p.registered {
			if sc.freqHz != freqHz {
				return errcode.Conflict
			}
			sc.users++
			p.registered = true
		} else if sc.users == 1 && sc.freqHz != freqHz {
			// Sole user may reconfigure the slice frequency.
			period := PeriodFromHz(freqHz)
			if err := p.ctrl.Configure(machine.PWMConfig{Period: period}); err != nil {
				return err
			}
			sc.freqHz = freqHz
		} else if sc.freqHz != freqHz {
			return errcode.Conflict
		}
	}

	// Switch pin to PWM function and cache tops.
	machine.Pin(p.pin).Configure(machine.PinConfig{Mode: machine.PinPWM})

	p.mu.Lock()
	p.freqHz = freqHz
	p.reqTop = top
	p.hwTop = p.ctrl.Top()
	p.mu.Unlock()

	return nil
}

func (p *rp2PWM) Set(level uint16) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Cancel any active ramp to avoid contention.
	if p.rampAlive {
		close(p.rampCancel)
		p.rampAlive = false
	}
	p.setHW(level)
}

func (p *rp2PWM) Enable(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Model enable as "drive current level" vs "drive 0".
	if on {
		p.setHW(p.level)
	} else {
		p.setHW(0)
	}
	p.enabled = on
}

func (p *rp2PWM) Info() (int, rune, int) { return p.slice, p.ch, p.pin }

func (p *rp2PWM) StopRamp() {
	p.mu.Lock()
	if p.rampAlive {
		close(p.rampCancel)
		p.rampAlive = false
	}
	p.mu.Unlock()
}

func (p *rp2PWM) Ramp(to uint16, durationMs uint32, steps uint16, _ core.PWMRampMode) bool {
	// Immediate set when degenerate.
	if steps == 0 || durationMs == 0 {
		p.Set(to)
		return true
	}

	p.mu.Lock()
	if p.rampAlive || p.reqTop == 0 {
		p.mu.Unlock()
		return false
	}
	tgt := mathx.Min(to, p.reqTop)
	start := p.level
	cancel := make(chan struct{})
	p.rampCancel, p.rampAlive = cancel, true
	top := p.reqTop
	p.mu.Unlock()

	go func() {
		defer func() { p.mu.Lock(); p.rampAlive = false; p.mu.Unlock() }()
		tick := func(d time.Duration) bool {
			select {
			case <-cancel:
				return false
			case <-time.After(d):
				return true
			}
		}
		ramp.StartLinear(start, tgt, top, durationMs, steps, tick, func(lvl uint16) {
			p.mu.Lock()
			p.setHW(lvl)
			p.mu.Unlock()
		})
	}()
	return true
}

// Global PWM policy: per-slice frequency compatibility.
var globalPWM struct {
	mu    sync.Mutex
	slice map[int]*sliceCfg // slice index -> cfg
}

func init() {
	globalPWM.slice = make(map[int]*sliceCfg)
}

// -----------------------------------------------------------------------------
// PinHandle implementation
// -----------------------------------------------------------------------------

type rp2PinHandle struct {
	n    int
	fn   core.PinFunc
	gpio *rp2GPIO
	pwm  *rp2PWM
}

func (h *rp2PinHandle) Pin() int { return h.n }

func (h *rp2PinHandle) AsGPIO() core.GPIOHandle {
	if h.fn != core.FuncGPIOIn && h.fn != core.FuncGPIOOut {
		panic("pin not claimed for GPIO")
	}
	return h.gpio
}

func (h *rp2PinHandle) AsPWM() core.PWMHandle {
	if h.fn != core.FuncPWM {
		panic("pin not claimed for PWM")
	}
	return h.pwm
}

// -----------------------------------------------------------------------------
// I²C owner (one worker per bus)
// -----------------------------------------------------------------------------

// request posted to the per-bus worker
type i2cReq struct {
	addr uint16
	w, r []byte
	done chan error // buffered(1); worker replies best-effort
}

// per-bus owner that hosts a single worker goroutine
type i2cOwner struct {
	id   core.ResourceID
	hw   *machine.I2C
	reqs chan i2cReq
	quit chan struct{}
}

func newI2COwner(id core.ResourceID, hw *machine.I2C) *i2cOwner {
	o := &i2cOwner{
		id:   id,
		hw:   hw,
		reqs: make(chan i2cReq, 16),
		quit: make(chan struct{}),
	}
	go o.loop()
	return o
}

func (o *i2cOwner) loop() {
	for {
		select {
		case req := <-o.reqs:
			err := o.hw.Tx(req.addr, req.w, req.r)
			// best-effort reply; do not block the worker
			select {
			case req.done <- err:
			default:
			}
		case <-o.quit:
			return
		}
	}
}

func (o *i2cOwner) stop() { close(o.quit) }

// driversI2C adapts the owner to tinygo.org/x/drivers.I2C.
// It posts a request and optionally enforces a per-call timeout.
type driversI2C struct {
	o       *i2cOwner
	timeout time.Duration // 0 => no deadline
}

// Ensure compile-time conformance with drivers.I2C
var _ drivers.I2C = (*driversI2C)(nil)

func (d *driversI2C) Tx(addr uint16, w, r []byte) error {
	req := i2cReq{addr: addr, w: w, r: r, done: make(chan error, 1)}

	if d.timeout <= 0 {
		// Unbounded enqueue (blocks until space is available)
		d.o.reqs <- req
	} else {
		// Bounded enqueue
		t := time.NewTimer(d.timeout)
		select {
		case d.o.reqs <- req:
			if !t.Stop() {
				<-t.C
			}
		case <-t.C:
			return errcode.Busy
		}
	}

	// Completion
	if d.timeout <= 0 {
		return <-req.done
	}
	t := time.NewTimer(d.timeout)
	defer t.Stop()
	select {
	case err := <-req.done:
		return err
	case <-t.C:
		return errcode.Timeout
	}
}

// -----------------------------------------------------------------------------
// Resource registry (GPIO + PWM + I2C)
// -----------------------------------------------------------------------------

type rp2Registry struct {
	mu sync.Mutex

	// Unified pin ownership.
	pinOwners map[int]pinOwner // pin -> owner
	gpioMap   map[int]*rp2GPIO // pin -> GPIO view (cached)
	pwmMap    map[int]*rp2PWM  // pin -> PWM view (cached; for reset/release)

	// I2C
	i2cOwners map[core.ResourceID]*i2cOwner

	// UART
	uartPorts  map[core.ResourceID]*rp2SerialPort
	uartOwners map[core.ResourceID]string // <- NEW: bus id -> devID
}

type pinOwner struct {
	devID string
	fn    core.PinFunc
}

func NewResourceRegistry(plan setups.ResourcePlan) *rp2Registry {
	r := &rp2Registry{
		pinOwners:  make(map[int]pinOwner),
		gpioMap:    make(map[int]*rp2GPIO),
		pwmMap:     make(map[int]*rp2PWM),
		i2cOwners:  make(map[core.ResourceID]*i2cOwner),
		uartPorts:  make(map[core.ResourceID]*rp2SerialPort),
		uartOwners: make(map[core.ResourceID]string),
	}

	// Instantiate I2C owners from the provided plan (pins and frequency).
	for _, p := range plan.I2C {
		var hw *machine.I2C
		switch p.ID {
		case "i2c0":
			hw = machine.I2C0
		case "i2c1":
			hw = machine.I2C1
		default:
			continue
		}
		// Configure pins & bus frequency.
		sda := machine.Pin(p.SDA)
		scl := machine.Pin(p.SCL)
		sda.Configure(machine.PinConfig{Mode: machine.PinI2C})
		scl.Configure(machine.PinConfig{Mode: machine.PinI2C})
		hw.Configure(machine.I2CConfig{
			SCL:       scl,
			SDA:       sda,
			Frequency: p.Hz,
		})
		r.i2cOwners[core.ResourceID(p.ID)] = newI2COwner(core.ResourceID(p.ID), hw)
	}

	// UART setup
	for _, u := range plan.UART {
		var hw *uartx.UART
		switch u.ID {
		case "uart0":
			hw = uartx.UART0
		case "uart1":
			hw = uartx.UART1
		default:
			continue
		}
		// Configure pins and baud. Defaults inside uartx will apply if zero.
		_ = hw.Configure(uartx.UARTConfig{
			BaudRate: u.Baud,
			TX:       machine.Pin(u.TX),
			RX:       machine.Pin(u.RX),
		})
		r.uartPorts[core.ResourceID(u.ID)] = &rp2SerialPort{u: hw}
	}

	return r
}

func (r *rp2Registry) ClassOf(id core.ResourceID) (core.BusClass, bool) {
	switch string(id) {
	case "i2c0", "i2c1":
		return core.BusTransactional, true
	case "uart0", "uart1":
		return core.BusStream, true
	}
	return 0, false
}

// Transactional buses (I2C)
func (r *rp2Registry) ClaimI2C(devID string, id core.ResourceID) (drivers.I2C, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o := r.i2cOwners[id]
	if o == nil {
		return nil, errcode.UnknownBus
	}
	return &driversI2C{o: o, timeout: 250 * time.Millisecond}, nil
}

func (r *rp2Registry) ReleaseI2C(devID string, id core.ResourceID) {
	// Owners are long-lived per bus; nothing to do here.
}

// Serial
func (r *rp2Registry) ClaimSerial(devID string, id core.ResourceID) (core.SerialPort, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	p := r.uartPorts[id]
	if p == nil {
		return nil, errcode.UnknownBus
	}
	if owner, taken := r.uartOwners[id]; taken && owner != "" && owner != devID {
		return nil, errcode.Conflict // serial bus already claimed
	}
	// Record (or reaffirm) ownership.
	r.uartOwners[id] = devID
	return p, nil
}

func (r *rp2Registry) ReleaseSerial(devID string, id core.ResourceID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if owner, ok := r.uartOwners[id]; ok && owner == devID {
		delete(r.uartOwners, id)
	}
}

// Unified pin claims
func (r *rp2Registry) inBoardRange(n int) bool {
	min, max := boards.SelectedBoard.GPIOMin, boards.SelectedBoard.GPIOMax
	return n >= min && n <= max
}

func (r *rp2Registry) lookupGPIO(n int) *rp2GPIO {
	if g, ok := r.gpioMap[n]; ok {
		return g
	}
	h := &rp2GPIO{p: machine.Pin(n), n: n}
	r.gpioMap[n] = h
	return h
}

func (r *rp2Registry) ClaimPin(devID string, n int, fn core.PinFunc) (core.PinHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.inBoardRange(n) {
		return nil, errcode.UnknownPin
	}
	if owner, inUse := r.pinOwners[n]; inUse && owner.devID != "" {
		return nil, errcode.PinInUse
	}

	ph := &rp2PinHandle{n: n, fn: fn}

	switch fn {
	case core.FuncGPIOIn, core.FuncGPIOOut:
		ph.gpio = r.lookupGPIO(n)

	case core.FuncPWM:
		// Determine slice and controller for this pin.
		sliceNum, err := machine.PWMPeripheral(machine.Pin(n))
		if err != nil {
			return nil, errcode.Unsupported
		}
		ctrl := pwmGroupBySlice(sliceNum)
		// Channel within the slice: even pin => A(0), odd pin => B(1).
		chIdx := uint8(n & 1)
		chRune := 'A'
		if chIdx == 1 {
			chRune = 'B'
		}
		ph.pwm = &rp2PWM{
			pin:   n,
			ctrl:  ctrl,
			chIdx: chIdx,
			slice: int(sliceNum),
			ch:    chRune,
		}
		// Cache for later cleanup on release.
		r.pwmMap[n] = ph.pwm

	default:
		return nil, errcode.Unsupported
	}

	r.pinOwners[n] = pinOwner{devID: devID, fn: fn}
	return ph, nil
}

func (r *rp2Registry) ReleasePin(devID string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if owner, ok := r.pinOwners[n]; ok && owner.devID == devID {
		// PWM-specific cleanup (stop ramp, duty=0, slice accounting).
		if owner.fn == core.FuncPWM {
			if p, okp := r.pwmMap[n]; okp && p != nil {
				// Stop any active ramp and drive 0 safely.
				p.StopRamp()
				p.mu.Lock()
				// setHW respects reqTop/hwTop; if never configured, drive via controller.
				if p.hwTop != 0 && p.reqTop != 0 {
					p.setHW(0)
				} else {
					p.ctrl.Set(p.chIdx, 0)
				}
				p.enabled = false
				p.mu.Unlock()

				// Slice user accounting.
				globalPWM.mu.Lock()
				if sc := globalPWM.slice[p.slice]; sc != nil && p.registered && sc.users > 0 {
					sc.users--
					if sc.users == 0 {
						sc.freqHz = 0
					}
					p.registered = false
				}
				globalPWM.mu.Unlock()
			}
		}

		// In all cases, put the pin back to input.
		if g, ok2 := r.gpioMap[n]; ok2 && g != nil {
			g.p.Configure(machine.PinConfig{Mode: machine.PinInput})
		} else {
			machine.Pin(n).Configure(machine.PinConfig{Mode: machine.PinInput})
		}

		// Drop cached PWM view if present.
		delete(r.pwmMap, n)
		delete(r.pinOwners, n)
	}
}

func PeriodFromHz(hz uint64) uint64 {
	if hz == 0 {
		return 0 // or panic
	}
	return uint64(time.Second) / hz
}

// Close stops background workers (e.g. per-bus I2C goroutines).
func (r *rp2Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.i2cOwners {
		if o != nil {
			o.stop()
		}
	}
}

// ---- rp2SerialPort: adapts uartx to core.SerialPort (+optional configurators) ----
type rp2SerialPort struct{ u *uartx.UART }

func (p *rp2SerialPort) Write(b []byte) (int, error) { return p.u.Write(b) }
func (p *rp2SerialPort) RecvSomeContext(ctx context.Context, buf []byte) (int, error) {
	return p.u.RecvSomeContext(ctx, buf)
}
func (p *rp2SerialPort) SetBaudRate(br uint32) error { p.u.SetBaudRate(br); return nil }

// Parity strings: "none","even","odd"
func (p *rp2SerialPort) SetFormat(databits, stopbits uint8, parity string) error {
	var par uartx.UARTParity
	switch parity {
	case "even":
		par = uartx.ParityEven
	case "odd":
		par = uartx.ParityOdd
	default:
		par = uartx.ParityNone
	}
	return p.u.SetFormat(databits, stopbits, par)
}
