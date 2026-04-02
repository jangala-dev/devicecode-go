//go:build !(rp2040 || rp2350)

package provider

import (
	"time"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/hal/internal/provider/setups"

	"tinygo.org/x/drivers"
)

var (
	SelectedPlan     setups.ResourcePlan
	InitialHALConfig core.HALConfig
)

type hostRegistry struct{}

func NewResources() core.Resources {
	return core.Resources{Reg: hostRegistry{}}
}

func (hostRegistry) ClassOf(id core.ResourceID) (core.BusClass, bool) {
	return 0, false
}

func (hostRegistry) ClaimI2C(devID string, id core.ResourceID) (drivers.I2C, error) {
	return nil, errcode.Unsupported
}

func (hostRegistry) ReleaseI2C(devID string, id core.ResourceID) {}

func (hostRegistry) ClaimSerial(devID string, id core.ResourceID) (core.SerialPort, error) {
	return nil, errcode.Unsupported
}

func (hostRegistry) ReleaseSerial(devID string, id core.ResourceID) {}

func (hostRegistry) ClaimPin(devID string, pin int, fn core.PinFunc) (core.PinHandle, error) {
	return nil, errcode.Unsupported
}

func (hostRegistry) ReleasePin(devID string, pin int) {}

func (hostRegistry) SubscribeGPIOEdges(devID string, pin int, sel core.GPIOEdge, debounce time.Duration, buf int) (core.GPIOEdgeStream, error) {
	return nil, errcode.Unsupported
}

func (hostRegistry) UnsubscribeGPIOEdges(devID string, pin int) {}
