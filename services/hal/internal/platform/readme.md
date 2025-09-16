# Platform Factories

Provides platform-specific implementations for I²C and GPIO used by the HAL.

## Build Tags

- `rp2040 || rp2350` → MCU build using TinyGo `machine` and driver interfaces.
- `!rp2040 && !rp2350` → Host build with fakes/simulators for tests.

## I²C

- `DefaultI2CFactory()`  
  - MCU: configures `i2c0` and `i2c1` at 400 kHz on board defaults.  
  - Host: exposes inert `HostI2C` instances (`i2c0`, `i2c1`) that record last TX.

## GPIO

- `DefaultPinFactory()`  
  - MCU: numbers map directly to `machine.Pin(n)` (RP2 GP0..GP28); interrupt support via `SetInterrupt`.
  - Host: `FakePin` with level, toggle, IRQ edge selection, and software debounce.

## Guidance

- Keep the `halcore` interfaces stable (I²C subset and GPIO/IRQ).  
- Add new buses or mappings here; the service consumes via factories injected at `hal.Run`.
