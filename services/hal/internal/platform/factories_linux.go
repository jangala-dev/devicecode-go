// services/hal/internal/platform/factories_linux.go
//go:build linux && arm64 && !(rp2040 || rp2350)

package platform

import (
	"devicecode-go/services/hal/internal/halcore"

	"tinygo.org/x/drivers"
)

// On standard Go builds, default to "not configured". Tests should inject fakes.
func DefaultI2CFactory() halcore.I2CBusFactory { return noI2CFactory{} }
func DefaultPinFactory() halcore.PinFactory    { return noPinFactory{} }

type noI2CFactory struct{}

func (noI2CFactory) ByID(string) (drivers.I2C, bool) { return nil, false }

type noPinFactory struct{}

func (noPinFactory) ByNumber(int) (halcore.GPIOPin, bool) { return nil, false }
