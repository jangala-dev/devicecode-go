package drvshim

import "devicecode-go/services/hal/internal/core"

// I2C adapts a core.I2COwner to tinygo.org/x/drivers.I2C.
type I2C struct {
	o         core.I2COwner
	timeoutMS int
}

func NewI2C(owner core.I2COwner) I2C {
	return I2C{o: owner, timeoutMS: 25}
}

func (s I2C) WithTimeout(ms int) I2C {
	if ms > 0 {
		s.timeoutMS = ms
	}
	return s
}

// Tx delegates to the owner. The owner must support repeated-start when w and r are both non-nil.
func (s I2C) Tx(addr uint16, w, r []byte) error {
	return s.o.Tx(addr, w, r, s.timeoutMS)
}
