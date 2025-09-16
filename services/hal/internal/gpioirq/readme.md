# GPIO IRQ Worker

Moves fast ISR callbacks into a safe worker queue and emits high-level GPIO events.

## Design

- ISR handler reads pin and **non-blocking** sends to `isrQ`. Drops are counted.
- Worker applies inversion, software debounce, and edge detection against the last logical level.
- Emits `GPIOEvent{DevID, Level(0/1), Edge, TS}` on `Events()`.

## API

- `New(isrBuf, outBuf int) *Worker`
- `Start(ctx)`
- `RegisterInput(devID string, pin IRQPin, edge Edge, debounceMS int, invert bool) (cancel func(), err error)`
- `Events() <-chan GPIOEvent`
- `ISRDrops() uint32`

Use from `service` to publish `gpio/event` and retained level state, or to trigger priority reads (e.g. SMBALERT#).
