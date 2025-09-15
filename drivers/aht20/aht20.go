// Package aht20 provides a driver for the AHT20 temperature/humidity sensor.
// It exposes a two-phase measurement API:
//
//	d.Trigger()              // start a measurement (fast)
//	err := d.Collect(&s)     // fetch when ready; returns ErrNotReady while busy
//
// For convenience, d.Read() performs trigger + bounded polling until ready.
//
// NOTE: I2C.Tx MUST perform a write followed by a repeated-start read when both
// w and r are provided, without releasing the bus.
//
// The driver avoids floating-point on the hot path; fixed-point helpers return
// tenths of units (deci-°C and deci-%RH).
package aht20

import (
	"errors"
	"time"

	"tinygo.org/x/drivers"
)

// I2C address.
const Address = 0x38

// Commands and status bits (per datasheet/common driver practice).
const (
	cmdTrigger    = 0xAC
	cmdInitialize = 0xBE
	cmdSoftReset  = 0xBA
	cmdStatus     = 0x71

	statusBusy       = 0x80
	statusCalibrated = 0x08
)

// Errors returned by the driver.
var (
	ErrTimeout  = errors.New("aht20: timeout")
	ErrNotReady = errors.New("aht20: not ready")
	ErrProtocol = errors.New("aht20: protocol error")
)

// Config controls non-hardware behaviour. All fields are optional.
type Config struct {
	// Address defaults to 0x38 if zero.
	Address uint16
	// PollInterval is used by Read() between Collect() attempts for ErrNotReady.
	// Default 15 ms.
	PollInterval time.Duration
	// CollectTimeout bounds the total wait in Read(). Default 250 ms.
	CollectTimeout time.Duration
	// TriggerHint is a nominal conversion time used only as a hint (no sleep is
	// performed in Trigger). Default 80 ms. Exposed to callers who want to
	// schedule Collect themselves without using Read().
	TriggerHint time.Duration
}

// Device wraps an I2C connection to an AHT20 device.
type Device struct {
	bus     drivers.I2C
	Address uint16

	cfg      Config
	buf      [7]byte // reuse buffer to avoid allocations
	humidity uint32  // last raw humidity sample
	temp     uint32  // last raw temperature sample
}

// New creates a new AHT20 connection. The I2C bus must already be configured.
// This function only creates the Device object; it does not touch the device.
func New(bus drivers.I2C) Device {
	return Device{
		bus:     bus,
		Address: Address,
	}
}

// Configure initialises the device if needed and applies optional config.
// Backwards-compatible with the previous signature; it may be called with no cfg.
func (d *Device) Configure(cfgs ...Config) {
	if len(cfgs) > 0 {
		c := cfgs[0]
		if c.Address != 0 {
			d.Address = c.Address
		}
		if c.PollInterval <= 0 {
			c.PollInterval = 15 * time.Millisecond
		}
		if c.CollectTimeout <= 0 {
			c.CollectTimeout = 250 * time.Millisecond
		}
		if c.TriggerHint <= 0 {
			c.TriggerHint = 80 * time.Millisecond
		}
		d.cfg = c
	} else {
		d.cfg = Config{
			Address:        d.Address,
			PollInterval:   15 * time.Millisecond,
			CollectTimeout: 250 * time.Millisecond,
			TriggerHint:    80 * time.Millisecond,
		}
	}

	// Check initialisation state.
	st, _ := d.Status() // ignore error; will attempt init anyway
	if st&statusCalibrated != 0 {
		return // device is already initialised
	}

	// Force initialisation; tolerate devices that do not ACK immediately.
	_ = d.bus.Tx(d.Address, []byte{cmdInitialize, 0x08, 0x00}, nil)
	// Small guard delay; callers should not expect an immediate ready sample.
	time.Sleep(10 * time.Millisecond)
}

// Reset issues a soft reset. Give the device ~20ms afterwards before using.
func (d *Device) Reset() {
	_ = d.bus.Tx(d.Address, []byte{cmdSoftReset}, nil)
}

// Status reads and returns the status byte. If the transaction fails, 0, err
// is returned. (A legacy error-less Status() is kept below for compatibility.)
func (d *Device) Status() (byte, error) {
	data := []byte{0}
	if err := d.bus.Tx(d.Address, []byte{cmdStatus}, data); err != nil {
		return 0, err
	}
	return data[0], nil
}

// Trigger starts a measurement. It is a quick register write with no blocking.
// After Trigger, the device needs time to convert; see d.TriggerHint().
func (d *Device) Trigger() error {
	// Ensure the device has been configured at least once.
	if d.cfg.PollInterval == 0 {
		d.Configure()
	}
	return d.bus.Tx(d.Address, []byte{cmdTrigger, 0x33, 0x00}, nil)
}

// TriggerHint returns the nominal conversion time to wait before attempting Collect.
func (d *Device) TriggerHint() time.Duration {
	if d.cfg.TriggerHint > 0 {
		return d.cfg.TriggerHint
	}
	return 80 * time.Millisecond
}

// Collect attempts to read one measurement into the device cache and the
// provided sample. If the device is not ready yet, ErrNotReady is returned.
// Any bus error is returned as-is.
func (d *Device) Collect(out *Sample) error {
	data := d.buf[:]
	if err := d.bus.Tx(d.Address, nil, data); err != nil {
		return err
	}
	// Check status bits in byte 0.
	if (data[0]&statusCalibrated) == 0 || (data[0]&statusBusy) != 0 {
		return ErrNotReady
	}
	// Parse raw values.
	hraw := (uint32(data[1]) << 12) | (uint32(data[2]) << 4) | (uint32(data[3]) >> 4)
	traw := (uint32(data[3]&0x0F) << 16) | (uint32(data[4]) << 8) | uint32(data[5])

	d.humidity = hraw
	d.temp = traw

	if out != nil {
		out.RawHumidity = hraw
		out.RawTemp = traw
	}
	return nil
}

// Read performs a full measurement cycle: Trigger followed by bounded
// polling until Collect succeeds or the timeout elapses.
func (d *Device) Read() error {
	if err := d.Trigger(); err != nil {
		return err
	}
	deadline := time.Now().Add(d.cfg.CollectTimeout)
	for {
		var s Sample
		err := d.Collect(&s)
		switch err {
		case nil:
			return nil
		case ErrNotReady:
			if time.Now().After(deadline) {
				return ErrTimeout
			}
			time.Sleep(d.cfg.PollInterval)
			continue
		default:
			return err
		}
	}
}

// Sample holds raw readings.
type Sample struct {
	RawHumidity uint32
	RawTemp     uint32
}

// Fixed-point conversion helpers operating on Sample.

func (s Sample) DeciRelHumidity() int32 {
	return (int32(s.RawHumidity) * 1000) / 0x100000
}

func (s Sample) DeciCelsius() int32 {
	return ((int32(s.RawTemp) * 2000) / 0x100000) - 500
}

// Backwards-compatible accessors (operate on the last cached sample).

func (d *Device) RawHumidity() uint32 { return d.humidity }
func (d *Device) RawTemp() uint32     { return d.temp }

// RelHumidity returns relative humidity in percent (float). Prefer DeciRelHumidity for fixed-point.
func (d *Device) RelHumidity() float32 {
	return (float32(d.humidity) * 100) / 0x100000
}

// DeciRelHumidity returns tenths of %RH.
func (d *Device) DeciRelHumidity() int32 {
	return (int32(d.humidity) * 1000) / 0x100000
}

// Celsius returns °C (float). Prefer DeciCelsius for fixed-point.
func (d *Device) Celsius() float32 {
	return (float32(d.temp)*200.0)/0x100000 - 50
}

// DeciCelsius returns tenths of °C.
func (d *Device) DeciCelsius() int32 {
	return ((int32(d.temp) * 2000) / 0x100000) - 500
}

// ---- Legacy helpers retained for compatibility ----

// StatusLegacy mirrors the old signature (no error result). Deprecated.
func (d *Device) StatusLegacy() byte {
	st, _ := d.Status()
	return st
}
