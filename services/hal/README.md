# `hal``

## Overview

HAL is a core service that sits on top of our `bus`. It exposes devices as **capabilities** with stable addresses, accepts **controls** via bus topics, and publishes **telemetry** (values, events, status) in a consistent shape. It also provides a provider-backed **resource registry** that arbitrates pins and buses on the target (here, RP2040).

What follows describes startup, configuration, topic taxonomy, control routing, telemetry, device lifecycle, resource management, and the concrete providers/devices we’ve included.

## Startup and configuration

* Entry point is `hal.Run(ctx, conn)`.
* A provider constructs `Resources` (`provider.NewResources()`), which contain a `ResourceRegistry` (for pins/buses) and will be populated with an `EventEmitter` by HAL.
* If a **compile-time** initial configuration is present (`provider.InitialHALConfig`), it is published to `config/hal` as a **retained** message:

  * Topic: `config/hal`
  * Payload: `types.HALConfig` (list of `types.HALDevice`)
* HAL core is initialised: `core.NewHAL(conn, res)` sets up:

  * Device map (`devID → Device`)
  * Capability index (`(domain, kind, name) → devID`)
  * A single producer channel `evCh` for device→HAL telemetry (bounded, len 32)
  * Injects itself as `Resources.Pub` so devices can emit events
* `HAL.Run` subscribes to:

  * `config/hal` for configuration updates
  * `hal/cap/+/+/+/control/+` for all capability controls
* The main loop:

  * Applies configuration messages as they arrive (idempotent/additive per device ID)
  * Declares itself **ready** (publishes retained `hal/state` with `Level:"ready"`) once at least one config has been applied
  * Rejects controls with `errcode.HALNotReady` until ready
  * Publishes all device telemetry from a single goroutine consuming `evCh`
  * Shuts down cleanly on `ctx.Done()` and publishes `hal/state` `Level:"stopped"`

## Device model and builders

Devices implement:

```go
type Device interface {
    ID() string
    Capabilities() []CapabilitySpec
    Init(ctx context.Context) error
    Control(cap CapAddr, method string, payload any) (EnqueueResult, error)
    Close() error
}
```

* **Builder registration**: Device types register a `core.Builder` against a string key (e.g. `"gpio_switch"`, `"pwm_out"`, `"aht20"`, `"serial_raw"`). `core.RegisterBuilder` guards against duplicates.
* **Instantiation** (`applyConfig`):

  1. For each `types.HALDevice` not yet present, look up the builder by `Type`.
  2. Call `Build(ctx, BuilderInput{ID, Type, Params, Res})`.
  3. Call `Init(ctx)`; on any fatal error HAL panics (our code opts to fail fast).
  4. Index **capabilities** and publish retained **info** and initial **status:down** per capability (see “Publication taxonomy”).
* **Control contract**: `Control` is **enqueue-only** from HAL’s point of view. A device returns `{OK:true}` to acknowledge acceptance, or `{OK:false, Error:<code>}`. If `error` is non-nil, HAL converts it to an error code via `errcode.Of(err)` and replies accordingly. All replies use the request–reply helpers on the bus.

## Publication taxonomy (topics and payloads)

Helpers in `core/topics.go` form the public surface:

* **Capability base**: `hal/cap/<domain>/<kind>/<name>`
* **Static info** (retained): `…/info` → `types.Info`
  Published when a capability is registered.
* **Status** (retained): `…/status` → `types.CapabilityStatus{Link, TS, Error}`
  Initial state is `LinkDown`; transitions to `LinkUp` (or `LinkDegraded` with `Error`) on telemetry processing.
* **Value** (retained): `…/value` → capability-specific value struct
  Published when a capability emits a “value” (non-event) update.
* **Event** (non-retained): `…/event` → event payload
  Optional tag path element: `…/event/<tag>` (e.g. `…/event/link_up`).
* **HAL state** (retained): `hal/state` → `types.HALState{Level, Status, TS}`.
* **Configuration** (retained): `config/hal` → `types.HALConfig` (input to HAL).

### Control addressing

Controls are sent to:

```
hal/cap/<domain>/<kind>/<name>/control/<verb>
```

* Parsed by `parseCapCtrl`. HAL looks up `(domain, kind, name)` in its capability index to find the owning device and calls `Device.Control`.
* Replies:

  * If device returned `{OK:true}` → HAL replies `types.OKReply{OK:true}`.
  * If `{OK:false, Error:…}` → HAL replies `types.ErrorReply{OK:false, Error:<code>}`.
  * If `Control` returned a non-nil `error` → mapped to `types.ErrorReply`.
  * If the request lacked `ReplyTo` → no reply (bus semantics).

## Telemetry path (device → HAL → bus)

Devices do not publish directly to the bus. They call `Resources.Pub.Emit(Event)`:

```go
type Event struct {
    Addr CapAddr     // {Domain, Kind, Name}
    Payload any
    TS int64
    Err string
    IsEvent bool
    EventTag string // optional subtopic for events
}
```

`HAL.Emit` enqueues the `Event` on `evCh` (non-blocking; drops if full), and the single run-loop goroutine processes it:

1. **Error case** (`Err != ""`): publish retained **status:degraded** with `Error`, and do **not** publish `value`/`event`.
2. **Success case**:

   * If `IsEvent`: publish `…/event` (or `…/event/<tag>`), non-retained.
   * Else: publish retained `…/value`.
   * Then publish retained `…/status` with `LinkUp` and current `TS`.

This guarantees: status reflects last observation; values are retained for late subscribers; events do not pollute retained state.

## Readiness and reply policy

* HAL only accepts controls after at least one configuration has been applied (a deliberate gate). Before that it replies with `HALNotReady`.
* `reply(...)` normalises error handling:

  * If `err` non-nil → map to `types.ErrorReply`.
  * Else if `enqOK` true → `types.OKReply`.
  * Else if `code==""` → default `errcode.Busy`.
  * Replies are only sent when the incoming message has a `ReplyTo`.

## Resource registry (provider) and arbitration

The provider encapsulates platform details and enforces safe sharing:

### Unified interfaces

* **Pins**:

  * Claim with a declared function: `ClaimPin(devID, pin, PinFunc)` where `PinFunc` is one of:

    * `FuncGPIOIn`, `FuncGPIOOut`, `FuncPWM` (extensible)
  * Returns a `PinHandle`, from which the device obtains a function-specific view:

    * `AsGPIO() GPIOHandle` (configure input with pull, configure output, Set/Get/Toggle)
    * `AsPWM() PWMHandle` (Configure, Set, Enable, Info, Ramp/StopRamp)
  * `ReleasePin(devID, pin)` — releases claim; provider resets the pin to input and performs any function-specific cleanup.

* **Transactional buses (I2C)**:

  * `ClaimI2C(devID, id ResourceID) (drivers.I2C, error)`

    * Returns a `drivers.I2C` that serialises access through a **per-bus worker goroutine** (owner pattern) to ensure mutual exclusion.
    * Optional per-call timeouts map to `errcode.Busy`/`Timeout`.
  * `ReleaseI2C(devID, id)`

* **Stream buses (UART)**:

  * `ClaimSerial(devID, id) (SerialPort, error)`

    * Returns a minimal `SerialPort` (blocking `Write`, `RecvSomeContext`).
    * May also implement `SerialConfigurator` (baud) and `SerialFormatConfigurator` (frame format).
    * UARTs are single-owner: a second claimant receives `errcode.Conflict`.
  * `ReleaseSerial(devID, id)`

* **Classification** (optional): `ClassOf(id)` reports whether a resource ID is transactional or stream, which can assist in device decisions.

### RP2040 provider specifics

* **Board description** (`boards.SelectedBoard`) defines GPIO range and controller identities (e.g. `i2c0`, `uart1`), plus convenient default pin numbers.
* **Resource plan** (`setups.ResourcePlan`) selects concrete wiring and operating parameters for this build (pins and frequencies for I2C, TX/RX pins and baud for UART).
* **Instantiated owners**:

  * I2C: for each configured bus, set up pins, configure frequency, and run one worker goroutine receiving `i2cReq{addr,w,r,done}`.
  * UART: configure pins and initial baud via `uartx`, wrap as `rp2SerialPort` implementing `SerialPort` (+ configurators).
* **PWM**:

  * Per-pin `rp2PWM` controls a **slice** (`PWM0..7`) and **channel** (A/B).
  * Global policy enforces **per-slice frequency compatibility**:

    * First user sets slice frequency.
    * Additional users must request the same frequency.
    * A sole user may reconfigure the slice.
    * Reference counts maintained so the last user clears frequency.
  * `Ramp` runs in a goroutine with cooperative cancellation. Steps are scaled from logical `0..top` to hardware `0..ctrl.Top()`.
  * On `ReleasePin` for a PWM claimant: stop ramp, drive duty to zero safely, fix up slice user accounting, and return the pin to input.
* **Shutdown**: provider implements `Close()` to stop background workers (e.g. I2C owners).

## Device implementations included

### `gpio_dout` (LED/Switch)

* **Builder** claims a GPIO pin as `FuncGPIOOut`. One implementation serves two roles:

  * `gpio_switch` (defaults to domain `power`)
  * `gpio_led` (defaults to domain `io`)
* **Capability**: exactly one, kind determined by role (`KindSwitch` or `KindLED`).
* **Init**: configure output with initial level (honouring `ActiveLow`), publish current value via HAL immediately.
* **Control verbs**:

  * `set`:

    * For switch: payload `types.SwitchSet{On bool}`
    * For LED: payload `types.LEDSet{Level uint8}` (0 or 1 in this device)
  * `toggle`
  * `read` (re-emits current value)
* All controls are synchronous and return `OK` once enqueued; HAL replies with `OKReply`.
* **Close**: release the pin via registry.

### `pwm_out`

* **Builder** claims a pin as `FuncPWM` and propagates desired `FreqHz` and `Top`.
* **Capability**: kind `PWM` with detail `{Pin, FreqHz, Top}`.
* **Init**: configure PWM; on error, emit a degraded status event (HAL converts to `status:degraded`).
* **Control verbs**:

  * `set`: payload `types.PWMSet{Level uint16}` → sets duty and emits retained value.
  * `ramp`: payload `types.PWMRamp{To uint16, DurationMs uint32, Steps uint16, Mode uint8}` → starts cooperative ramp (returns `Busy` if already ramping).
  * `stop_ramp`
* **Close**: stop ramp and release the pin.

### `aht20` (temperature/humidity over I2C)

* **Builder** claims an I2C bus (`ClaimI2C`), wraps TinyGo `drivers.I2C`, initialises device struct with address defaulting to `0x38`.
* **Init**: set up capability addresses; defers I2C configuration until first read.
* **Control verbs**:

  * `read`: launches a goroutine (guards with `reading` flag) to:

    * Configure driver (idempotent), trigger a read with bounded polling, map errors to `errcode`.
    * Convert values to `types.TemperatureValue{DeciC}` and `types.HumidityValue{RHx100}`, clamped to safe ranges.
    * Emit two value events (temp & humidity) with a common timestamp.
* **Close**: release I2C claim.

### `shtc3` (temperature/humidity over I2C)

* Same pattern as `aht20` but uses `tinygo.org/x/drivers/shtc3`. The `readOnce` sequence does wake → read → sleep, converts units, clamps, and emits two value events.

### `serial_raw` (UART pass-through with shared memory rings)

* **Builder** claims a UART via `ClaimSerial` (single-owner policy). Records configurator interfaces if available.
* **Init**: apply baud (explicit or default 115200). Emit initial degraded status (`Err:"initialising"`) so consumers see the port before a session is opened.
* **Control verbs**:

  * `session_open` (optional sizes, power-of-two):

    * Creates RX/TX shared-memory rings (via `shmring`), starts two goroutines:

      * `rxLoop`: blocks on `RecvSomeContext`, writes bytes into RX ring; logs overflow if any.
      * `txLoop`: waits for TX ring readability, drains and writes synchronously to the port.
    * Emits:

      * `…/event/session_opened` with `{SessionID, RXHandle, TXHandle}`
      * `…/event/link_up` (tagged)
    * Returns `OK` or `Conflict` if already open.
  * `session_close`:

    * Gracefully stops loops, closes rings, emits `…/event/session_closed` and a degraded status (`Err:"session_closed"`).
  * `set_baud`: uses `SerialConfigurator`; accepts `float64` or `uint32` payload.
  * `set_format`: uses `SerialFormatConfigurator`; payload `{databits:uint8, stopbits:uint8, parity:"none"|"even"|"odd"}`.
* **Close**: stop session if present and release the UART.

## Control routing and replies in detail

1. A client sends a control to e.g. `hal/cap/power/switch/mpcie/control/set` with payload `types.SwitchSet{On:true}` and a `ReplyTo`.
2. HAL parses and resolves the owning device via `capIndex`.
3. Calls `Device.Control(...)`:

   * If it returns `(EnqueueResult{OK:true}, nil)` → immediate `OKReply`.
   * If it returns `(EnqueueResult{OK:false, Error:X}, nil)` → `ErrorReply{Error:X}`.
   * If it returns `(_, err)` → map `err` to `ErrorReply`.
4. Devices carry out the action and typically emit an immediate value, which HAL converts into retained `…/value` and `…/status LinkUp`.

This model ensures **non-blocking** controls and **single-threaded** publication.

## Concurrency and back-pressure

* HAL core serialises:

  * Configuration application and control handling in the main `Run` loop.
  * All telemetry publication through the single `evCh` consumer.
* `EventEmitter.Emit` is non-blocking:

  * If `evCh` is full (32), the event is **dropped** (device sees `false` return). Devices that must not lose updates should either coalesce or retry.
* Provider components introduce their own concurrency (e.g. I2C worker per bus; PWM ramp goroutine; serial RX/TX goroutines) but present **safe, arbitrated** interfaces to devices.

## Error mapping and status semantics

* Device driver errors are mapped to short machine codes (`errcode.MapDriverErr`) and emitted as `Event{Err:code}`. HAL responds by:

  * Publishing `…/status` with `LinkDegraded` and `Error:code`.
  * Suppressing `value`/`event` for that emission.
* On successful emissions, HAL always publishes `LinkUp`.

## Build-time setup and initial configuration

* Build tags select:

  * **Board** (`//go:build pico`)
  * **Setup** (`pico_rich_dev` or `pico_bb_proto_1`) with a `ResourcePlan` and a `types.HALConfig SelectedSetup`.
* `provider.setup_selected.go` copies `SelectedPlan` and `SelectedSetup` into:

  * `provider.SelectedPlan` used to instantiate resource owners
  * `provider.InitialHALConfig` published at startup (retained), so HAL becomes **ready** immediately without waiting for an external `config/hal`.

## Worked examples

### 1) Turning on a power switch

* Send: topic `hal/cap/power/switch/cm5-5v/control/set`, payload `types.SwitchSet{On:true}`, with `ReplyTo`.
* HAL replies `{"OK":true}`.
* Device sets GPIO, emits `types.SwitchValue{On:true}`; HAL publishes:

  * Retained `hal/cap/power/switch/cm5-5v/value` → `{On:true}`
  * Retained `hal/cap/power/switch/cm5-5v/status` → `{Link:"up"}`

## 2) Reading AHT20 once

* Send: topic `hal/cap/env/temperature/core/control/read` (and similarly humidity if desired), payload empty, with `ReplyTo`.
* HAL replies `{"OK":true}` (if not busy).
* Device goroutine reads sensor and emits two values (temp, humidity).
* HAL publishes retained values under `…/value` and updates both statuses to `LinkUp`.

### 3) Opening a raw serial session

* Send: `hal/cap/io/serial/uart0/control/session_open` with optional map payload `{ "RXSize": 1024, "TXSize": 1024 }`, with `ReplyTo`.
* HAL replies `{"OK":true}`.
* Device emits `…/event/session_opened` with ring handles and `…/event/link_up`. Status becomes `LinkUp`.
* To write, the peer places bytes into the TX ring; to read, it consumes from the RX ring. Close with `session_close`.

## Operational guarantees and trade-offs

* **Discovery**: late subscribers receive retained `info`, last `value`, and last `status` for each capability.
* **Routing**: control topics are validated structurally; unknown capabilities return `UnknownCapability`.
* **Back-pressure**: control path is non-blocking; telemetry path is bounded and may drop under sustained load (devices can observe `Emit` return `false`).
* **Isolation**: resources (pins, UART, I2C) are arbitrated centrally; misbehaving devices cannot steal a resource already claimed by another.
* **Determinism**: all bus publications originate from one goroutine, avoiding interleaving races in message order.
