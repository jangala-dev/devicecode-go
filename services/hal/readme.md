# Overview

`hal` in Go is the **port of the DC-Lua HAL service** used on Linux-class devices, adapted for TinyGo (RP2040/Pico) and Go (Linux/arm64 - in the future). It retains the **message-driven, capability-based model** from the Lua version, but introduces changes to suit constrained MCU targets, stronger typing, and Go’s concurrency model.

Key properties:

* Runs with `hal.Run(ctx, conn)` and constructs a `Service` with **compile-time injected platform factories** (I²C, GPIO).
* Fully **configured over the in-process pub/sub bus** (`config/hal`).
* Exposes **capabilities** under `hal/capability/<kind>/<id>/…`, indexed per runtime.
* Uses a **registry of device builders** (via blank imports) to create adaptors that implement a minimal interface (`Trigger`, `Collect`, `Control`).
* Provides **periodic sampling**, **priority reads**, and **IRQ-driven events**, serialised per shared bus.
* Publishes retained **capability metadata** and **state**, and supports **request–reply control**.


# Differences from the Lua HAL

This service follows the same architectural goals as Lua HAL (device discovery, configuration via the bus, capability addressing), but some key changes have been made:

* **Typed interfaces:** Where Lua relied on dynamic tables and conventions, Go defines explicit interfaces (`Adaptor`, `CapInfo`, etc.), improving compile-time checking.
* **Goroutines, not fibers:** On MCUs the Lua “fibers” library is replaced by Go’s goroutines and channels. This keeps concurrency lightweight while simplifying integration with TinyGo.
* **Trigger/Collect model:** Retained from Lua, but codified as a `MeasureWorker` per bus to guarantee serialised access and prevent I²C contention.
* **Configuration model:** JSON config (`HALConfig`) is deserialised into typed structures; repeated application is idempotent.
* **Capability addressing:** Still `hal/capability/<kind>/<id>`, but IDs are assigned per service runtime (monotonic counter).
* **Platform abstraction:**

  * **RP2 builds:** use TinyGo drivers for I²C and GPIO, with IRQ support.
  * **Linux/arm64 builds:** provide stub factories (to allow testing and parity with existing Lua HAL, where drivers remain Lua-based).
* **Error handling:** Go version errs on safety—failed device builds or unsupported drivers do not panic; state is published as `"error"`.
* **Testability:** Factories and adaptor interfaces enable fake injection; more suitable for unit tests than Lua’s global state.


# Configuration and Topics

The service subscribes to:

* `config/hal` — JSON config (`HALConfig`) declaring devices.
* `hal/capability/+/+/control/+` — control RPCs.

A `HALConfig` defines `Devices[]` with:

* `id`, `type`, optional `params`,
* optional `bus_ref {type,id}` (for shared I²C buses).

On config application:

* New devices are built using a registered `Builder`.
* Output includes:

  * an `Adaptor`,
  * optional `BusID` (for serialised workers),
  * optional `SampleEvery` (periodic producer),
  * optional `IRQ` (for GPIO).
* Capabilities are discovered and published:

  * `…/info` (retained metadata),
  * `…/state` (retained, `"up"`/`"down"`).
* Devices absent from the latest config are removed gracefully, clearing retained docs and cancelling workers.


# Measurement Model

Each device that produces samples is managed by a per-bus `MeasureWorker`:

* Requests (`MeasureReq`) are queued; priority requests are retried promptly.
* `Trigger(ctx)` returns a delay until ready.
* `Collect(ctx)` retrieves data.
* On success, results are published to `…/value`.
* On error, device state is degraded (`…/state = "error"`).

Backoff, retries, and bounded queues ensure stability on constrained hardware.


# GPIO IRQ Events

A dedicated IRQ worker manages GPIO events:

* ISR handler enqueues edges into a bounded queue.
* A draining goroutine applies inversion, debounce, and edge filtering.
* Events are published on `hal/capability/gpio/<id>/event`.
* Retained state (`…/state`) also reflects current level.


# Service Lifecycle and Bus Interaction

* Service publishes lifecycle state retained at `hal/state` (`"idle"`, `"ready"`, `"stopped"`).
* Capabilities publish:

  * `…/info` (retained static metadata),
  * `…/state` (retained status),
  * `…/value` (non-retained streaming measurements),
  * `…/event` (non-retained GPIO events).
* Control requests at `…/control/<method>`:

  * Built-ins: `read_now`, `set_rate`.
  * Others: forwarded to the device adaptor.

Here’s a section you can drop into the README. It explains concurrency in the Go/TinyGo HAL and contrasts it with the Lua version.

# Concurrency Model

The Go/TinyGo HAL retains the cooperative, message-driven style of the Lua version but deliberately **minimises the number of active goroutines**.

In the Lua HAL, every service and driver typically spawned its own fiber, and many background loops ran concurrently. This was straightforward to express but could lead to dozens of active fibers even when most devices were idle. On MCU targets this is unsustainable.

In the Go/TinyGo design:

* **Per-bus workers** — Sampling devices share a single `MeasureWorker` goroutine per bus. This serialises I²C access and avoids allocating one goroutine per device.
* **Shared service timers** — A single scheduling loop manages periodic sampling across all devices. It re-arms itself to the earliest due event rather than keeping separate timers per device.
* **IRQ worker** — One bounded queue and drain goroutine handle all GPIO interrupts. Individual devices do not spawn their own listeners.
* **Bounded queues, short-lived goroutines** — Where possible, operations are performed inline in the worker loop. Short helper goroutines (eg. to satisfy a request–reply) are spawned but exit immediately after use.

As a result, the number of long-lived goroutines is bounded and small, regardless of device count. The model favours a few central workers that multiplex many devices, rather than many concurrent routines. This keeps memory footprint and scheduling overhead predictable, which is critical on TinyGo targets with limited RAM.


# Platform Targets

* **RP2 (TinyGo / Pico):**

  * Two default I²C buses (400 kHz),
  * GPIO factory with pin mapping and IRQ support,
  * Drivers implemented in TinyGo (eg. AHT20).

* **Host:**

  * Factories return stubs (no discovery),
  * Enables testing.


# Intended Properties

* Capability indexing consistent per runtime.
* Serialised access to shared buses (I²C safe).
* Bounded queues and safe drops for back-pressure control.
* Retained documents for observability.
* Idempotent configuration.
* Testable via fake factories.


# Summary

The Go/TinyGo HAL is a **typed, concurrency-safe re-implementation of the Lua HAL**, preserving the same bus-driven architecture but adapted to:

* run on constrained MCUs (Pico),
* provide stronger compile-time safety,
* integrate cleanly with Go’s goroutines and channels,
* and remain interoperable with the Lua services still running on Linux-class targets.

This ensures feature parity with the Lua implementation while preparing for a gradual migration of device drivers into Go, and supporting innovation on both MCU and Linux platforms.