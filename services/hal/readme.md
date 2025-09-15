# Overview

`hal` is a **self-contained, message-driven HAL service** that:

* Starts with `hal.Run(ctx, conn)` and constructs a `Service` with **injected platform factories** for I2C and GPIO.
* Is **configured entirely via the in-process bus** (topic `config/hal`), and exposes **capability endpoints** under `hal/capability/<kind>/<id>/…`.
* Uses a **registry of device builders** (blank-import registrations) to construct device-specific **adaptors** that implement a small HAL interface (`Trigger`, `Collect`, `Control`).
* Manages **periodic sampling** and **priority on-demand reads** through per-bus **measurement workers** that implement a split-phase Trigger/Collect cycle with backoff and retries.
* Publishes **retained capability metadata and state**, **event/value messages**, and supports **request–reply controls**.

# Configuration and topics

* The service subscribes to:

  * `config/hal` — JSON config (`HALConfig`) declaring devices.
  * `hal/capability/+/+/control/+` — control RPCs to specific capability instances.

* A `HALConfig` contains `Devices[]`, each with:

  * `id`, `type`, optional `params`, and an optional shared-bus reference (`bus_ref {type,id}`, e.g. I2C).

* On configuration:

  * For each device not yet present, the service looks up a **Builder** by `type` in the **registry**, calls `Build` with factories and JSON params, and receives a `BuildOutput`:

    * `Adaptor` (implements `halcore.Adaptor`),
    * optional `BusID` (groups devices sharing the same physical bus),
    * optional `SampleEvery` (declares this device as a periodic producer),
    * optional `IRQ` request (GPIO edge events).
  * If a `BusID` is provided, a **per-bus `MeasureWorker`** is created (once) and started to serialise access on that bus.
  * The adaptor’s **capabilities** are discovered (`Capabilities() []CapInfo`). For each capability `kind`, the service assigns a **monotonic integer ID** (per `kind`) and publishes:

    * `hal/capability/<kind>/<id>/info` (retained) — the `CapInfo.Info` map.
    * `hal/capability/<kind>/<id>/state` (retained) — `{"link":"up","ts_ms":…}` upon configuration.
  * If `SampleEvery>0`, the device is scheduled for periodic sampling; period is **clamped to \[200 ms, 1 h]**.
  * If `IRQ` is requested and the pin supports interrupts, the **GPIO IRQ worker** registers an ISR and will emit debounced, edge-filtered events.

* Devices absent from the latest config are **removed idempotently**:

  * Their `info` is cleared (retained `nil`), `state` set to `"down"`, IRQs cancelled, and internal maps tidied.

# Capability addressing and control

* Capability address = `(kind, id)` where `id` is the service-assigned integer. The mapping `(kind,id) -> deviceID` is stored internally.
* Control requests are received on:

  * `hal/capability/<kind>/<id>/control/<method>` with optional payload and an optional reply-to.
* Built-in methods handled by the service:

  * `read_now` — attempts a **priority measurement** for the owning device (queues with `Prio=true`).
  * `set_rate` — updates the device’s periodic sampling period (`{"period_ms":N}`), clamped.
* Other methods are **forwarded to the adaptor’s `Control`** and the result/error is returned.
* Replies use the bus request–reply helper (`ReplyTo` topic), returning `{"ok":true,…}` or `{"ok":false,"error":…}`.

# Measurement model (Trigger/Collect)

* Each device that samples data is serviced by a **`MeasureWorker` tied to its BusID**:

  * **`Submit(MeasureReq{ID, Adaptor, Prio})`** enqueues a trigger request (bounded channel; priority path retries briefly).
  * Worker loop:

    * On request: **`Trigger(ctx)`** with timeout (default 100 ms) returns a **delay hint** `after` until data is ready. The worker records a `collectItem{due = now+after}`.
    * On timer: when `due` arrives, **`Collect(ctx)`** with timeout (default 250 ms):

      * On success: emits `Result{ID, Sample}`.
      * On `ErrNotReady`: retries up to `MaxRetries` (default 6) with **short backoff** (default 15 ms).
      * On other error: emits a failure. If a **priority re-read was requested while pending**, the worker **re-triggers immediately** once to service the priority request.
* The service consumes results:

  * On error: publishes degraded `state` for each capability of that device.
  * On success: for each `Reading{Kind, Payload, TsMs}` in the `Sample`, publishes:

    * `hal/capability/<kind>/<id>/value` (non-retained) with the reading payload,
    * updates retained `state` to `"up"` with timestamp.

# GPIO IRQ events

* A separate **`gpioirq.Worker`** manages ISR-safe edge capture:

  * ISR handler reads the pin level and **non-blocking** sends to a bounded `isrQ`; drops are counted atomically (`ISRDrops()`).
  * A single goroutine drains `isrQ`, applies **optional inversion**, **software debounce** (per-device `debounce_ms`), and **edge selection** (rising/falling/both).
  * Qualifying events are forwarded on `outQ` as `{DevID, Level (0/1), Edge, TS}`.
* The service, on each event for a device with `"gpio"` capability:

  * Publishes `hal/capability/gpio/<id>/event` (non-retained) including `edge`, `level`, `ts_ms`.
  * Updates retained `state` with current level.

# Bus interactions and retained documents

* Service lifecycle state is published retained at `hal/state` with `{"level":…,"status":…,"ts_ms":…}`:

  * Starts `"idle"/"awaiting_config"`, transitions to `"ready"/"configured"`, and `"stopped"/"context_cancelled"` on exit.
* Capability documents:

  * `…/info` (retained): static metadata from the adaptor (`unit`, `precision`, `schema_version`, `driver`, etc.).
  * `…/state` (retained): link/status, and for GPIO also last `level`.
  * `…/value` (non-retained): streaming measurements.
  * `…/event` (non-retained): edge events for GPIO.

# Platform abstraction and build targets

* **Build tags** split platform code:

  * **RP2 (TinyGo on RP2040(Pico)/RP2350(Pico 2)):** `factories_rp2.go` supplies:

    * Two pre-configured I2C buses (`i2c0`, `i2c1`) at 400 kHz on board-default pins.
    * A GPIO factory mapping logical numbers to `machine.Pin(n)` with input/output and IRQ support (rising/falling/both).
  * **Linux/arm64 (Pi 5)**: `factories_linux.go` supplies **no-op factories** (tests must inject fakes; service won’t “discover” real pins/buses by default).
* Device drivers follow the same pattern:

  * **AHT20**:

    * RP2 build wraps a TinyGo driver, exposes a small internal `aht20Device` interface; adaptor configures timings (poll interval, collect timeout, trigger hint).
    * Linux/arm64 build provides a **stub** that returns “not supported” (intended for future `devicecode-go` on current Lua targets).
  * **GPIO**:

    * Builder configures pin mode, pull, inversion, optional IRQ.
    * Adaptor implements `Control`:

      * **Inputs**: `get` → `{"level":0|1}` (observes inversion).
      * **Outputs**: `set {"level":bool}` (observes inversion), or `toggle`.
    * Not a periodic producer (no Trigger/Collect).

# Scheduling, timing and limits

* Periodic sampling is driven by a single service timer:

  * Next-due per device is tracked; the timer is re-armed to the **earliest due**; `util.ResetTimer` handles safe reset/drain.
  * Periods are clamped (`200 ms` minimum, `1 h` maximum).
* Utility helpers include JSON decoding from `any` (`DecodeJSON`), bounded integer clamp, and simple error formatting.

# Error handling and idempotence

* Unknown device types, failed builds, or missing buses/pins are **ignored** (no panic); the service reports `"error"` via `hal/state` when config decode/apply fails as a whole.
* Applying the same config twice is **idempotent** (devices already present are skipped).
* On shutdown, IRQ registrations are cancelled best-effort.

# Intended properties

* **Capability indexing** per service runtime (IDs assigned in order of discovery per `kind`).
* **Serialised access per shared bus** via the per-`BusID` worker to avoid I2C contention.
* **Back-pressure aware** paths: bounded queues for ISR and worker inputs; non-blocking emits with safe drops where necessary.
* **Testability**: small `halcore` interfaces, platform factories, and a registration mechanism enable fake injection on test builds (to be ported).
* **Observability hooks**: retained `state` documents, explicit degraded states on errors, and ISR drop counters (exposed programmatically).