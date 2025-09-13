// services/hal/workers_api.go
package hal

import "context"

// MeasurementWorker is the narrow contract the service relies on.
type MeasurementWorker interface {
	Submit(MeasureReq) bool
	Start(ctx context.Context)
}

// NewMeasurementWorker adapts the concrete constructor to the interface.
// Keeps call sites stable now and after we relocate the worker.
func NewMeasurementWorker(cfg WorkerConfig, sink chan<- Result) MeasurementWorker {
	return NewWorker(cfg, sink)
}

// GPIOIRQer is the narrow contract for the GPIO IRQ worker used by the service.
type GPIOIRQer interface {
	Start(ctx context.Context)
	Events() <-chan GPIOEvent
	RegisterInput(devID string, pin IRQPin, edge Edge, debounceMS int, invert bool) (func(), error)
	ISRDrops() uint32
}
