package ltc4015

// PinInput returns logical level of an input pin.
type PinInput func() bool

// SMBALERT is active-low; this helper keeps the driver portable.
func (d *Device) AlertActive(get PinInput) bool { return !get() }

// SMBus ARA handshake. Returns true if LTC4015 identified itself.
func (d *Device) AcknowledgeAlert() (bool, error) {
	var r [1]byte
	if err := d.i2c.Tx(ARAAddress, nil, r[:]); err != nil {
		return false, err
	}
	expected := byte((d.addr << 1) | 1)
	return r[0] == expected, nil
}

// I2C 16-bit word operations (Little-endian: LOW then HIGH).

func (d *Device) readWord(reg byte) (uint16, error) {
	d.w[0] = reg
	if err := d.i2c.Tx(d.addr, d.w[:1], d.r[:2]); err != nil {
		return 0, err
	}
	return uint16(d.r[0]) | uint16(d.r[1])<<8, nil
}

func (d *Device) readS16(reg byte) (int16, error) {
	u, err := d.readWord(reg)
	return int16(u), err
}

func (d *Device) writeWord(reg byte, val uint16) error {
	d.w[0] = reg
	d.w[1] = byte(val)      // low
	d.w[2] = byte(val >> 8) // high
	return d.i2c.Tx(d.addr, d.w[:3], nil)
}
