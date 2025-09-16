# HAL Core Interfaces

Defines the adaptor contracts, bus abstractions, GPIO types, and worker configuration.

## Adaptor Interface

```go
type Adaptor interface {
  ID() string
  Capabilities() []CapInfo
  Trigger(ctx) (collectAfter time.Duration, err error)
  Collect(ctx) (Sample, error)
  Control(kind, method string, payload any) (result any, err error) // optional
}
```

* `Trigger/Collect` split allows start/convert/wait patterns; return `ErrNotReady` to request a bounded retry.
* `Capabilities` declare retained `info` and capability kinds.

## Telemetry

* `Reading{Kind, Payload, TsMs}`, `Sample` (slice of `Reading`)
* `CapInfo{Kind, Info}` for retained metadata

## Buses and GPIO

* `I2CBusFactory` → `ByID(id) (drivers.I2C, bool)`
* `I2C` subset: `Tx(addr, w, r []byte) error`
* `GPIOPin` and `IRQPin` with `Pull` and `Edge` enums
* `PinFactory` → `ByNumber(n) (GPIOPin, bool)`

## WorkerConfig

Time-outs, retry/back-off and queue sizing for measurement workers.

Keep this package free of platform specifics.
