# HAL on TinyGo (`hal.go`) — overview and differences from `hal.lua`

This note explains the Hardware Abstraction Layer (HAL) service design for TinyGo (`services/hal`) and how it relates to `hal.lua`. It is a temporary handover summary.

It covers:

* what HAL is for;
* how services interact with HAL over the bus;
* where `hal.go` matches `hal.lua`;
* where `hal.go` intentionally differs; and
* how to extend `hal.go` safely.

## 1. Purpose and scope

**Purpose.** HAL is the single point of contact with hardware and the operating system. Other services (e.g. `bridge`, `power`, `sensor`) must not call drivers or manipulate pins directly. They use a uniform pub/sub interface provided by HAL.

**Scope.** In TinyGo, `hal.go` currently covers:

* configuration via JSON / `map[string]any` on `config/hal`;
* capability discovery and retained metadata under `hal/capability/...`;
* periodic measurement for sensors (e.g. AHT20 temperature / humidity) via a worker;
* **event-driven GPIO input using interrupts**, delivered via a dedicated IRQ worker (no polling);
* GPIO/PWM control via request–reply; and
* publication of values, events and state.

It does **not yet** implement dynamic hot-plug detection, UART stream handles, or a general device inventory topic. These are on the roadmap (Section 14).

---

## 2. High-level similarities with `hal.lua`

Both HALs:

* expose capabilities over the bus under `hal/capability/...`;
* publish **retained** metadata (`…/info`) and state (`…/state`) so late subscribers can discover what exists;
* provide a **uniform control surface** at `hal/capability/<kind>/<id>/control/<method>`;
* keep hardware access inside HAL, aiding testability and safety.

---

## 3. Key differences at a glance

| Area                      | `hal.lua`                                              | `hal.go` (TinyGo)                                                                                                     |
| ------------------------- | ------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------- |
| Concurrency model         | Fibres, queues, dynamic fan-in/out                     | One worker per physical bus; bounded queues; timer-driven scheduling                                                  |
| Configuration             | Device managers discover and signal connect/disconnect | Static JSON config on `config/hal` (hot-reload). No hot-plug yet                                                      |
| Capability addressing     | `hal/capability/<capability>/<instance-id>`            | `hal/capability/<kind>/<id:int>`; `id` is a per-kind index stable for the boot                                        |
| Measurement pipeline      | Driven by managers and capability fibres               | Worker serialises `Trigger`/`Collect`; retries on `ErrNotReady`                                                       |
| **GPIO inputs**           | Often polled or driver-specific                        | **Interrupt-driven, event-oriented** via a dedicated IRQ worker; ISR does a non-blocking send into a buffered channel |
| Device inventory          | `hal/device/<type>/<id>` retained                      | Not yet implemented (planned)                                                                                         |
| Streaming I/O (e.g. UART) | Control + event channels under capability              | Planned: handle-based RX/TX topics (Section 8)                                                                        |

---

## 4. Bus contract in `hal.go`

### 4.1 Common topics

* **Service state (retained)**
  `hal/state` → JSON, e.g.
  `{ "level": "ready", "status": "configured", "ts_ms": 1736172000000 }`

* **Capability info (retained)**
  `hal/capability/<kind>/<id:int>/info` → small JSON map.

* **Capability state (retained)**
  `hal/capability/<kind>/<id:int>/state` → JSON with link/health and, where relevant, last known level/value metadata.

* **Control (request–reply)**
  `hal/capability/<kind>/<id:int>/control/<method>`; replies carry `{ "ok": true, ... }` or `{ "ok": false, "error": "…" }`.

### 4.2 Sensors (e.g. AHT20)

* **Values (live; not retained)**
  `hal/capability/temperature/<id>/value` → `{ "deci_c": <int>, "ts_ms": <int64> }`
  `hal/capability/humidity/<id>/value` → `{ "deci_percent": <int>, "ts_ms": <int64> }`

* **Control methods**

  * `read_now`
  * `set_rate` with `{ "period_ms": <int> }`

### 4.3 GPIO

* **Info (retained)**
  `hal/capability/gpio/<id>/info` → e.g.
  `{ "pin": 3, "mode": "input", "pull": "up", "invert": false, "schema_version": 1 }`

* **State (retained)**
  `hal/capability/gpio/<id>/state` → e.g.
  `{ "link": "up", "level": 0|1, "ts_ms": <int64> }`
  (`level` is last known logical level after inversion.)

* **Events (live; not retained)**
  `hal/capability/gpio/<id>/event` →
  `{ "edge": "rising"|"falling", "level": 0|1, "ts_ms": <int64> }`

* **Control methods**

  * `configure_input` `{ "pull": "up"|"down"|"none" }`
  * `configure_output` `{ "initial": 0|1 }`
  * `set` `{ "level": 0|1 }`
  * `get` `{}` → `{ "level": 0|1 }`
  * `toggle` `{}`

**Notes.**
GPIO inputs are **interrupt-driven**. The ISR performs a register read and a **non-blocking** send into a buffered channel. Debounce and edge classification occur in the worker goroutine, off the ISR path.

### 4.4 PWM (control-only, if configured)

* **Info (retained)**
  `hal/capability/pwm/<id>/info` → e.g. `{ "pin": 25, "freq_hz": 1000, "schema_version": 1 }`

* **Control methods**

  * `configure` `{ "freq_hz": <int> }`
  * `set_duty` `{ "permille": 0..1000 }`
  * `off` `{}`

---

## 5. Configuration (`config/hal`)

HAL applies configuration published to `config/hal`. Current shape:

```json
{
  "version": 1,
  "buses": [
    { "id": "i2c0", "type": "i2c", "impl": "tinygo", "params": { "freq_hz": 400000 } }
  ],
  "devices": [
    { "id": "aht20-0", "type": "aht20",
      "bus_ref": { "id": "i2c0", "type": "i2c" },
      "params": { "addr": 56 } },

    { "id": "pwr_en", "type": "gpio",
      "params": { "pin": 2, "mode": "output", "initial": 1 } },

    { "id": "smbalert", "type": "gpio",
      "params": { "pin": 3, "mode": "input", "pull": "up",
                  "irq": { "edge": "falling", "debounce_ms": 2 } } }

    // Optional PWM example:
    // { "id": "led1", "type": "pwm",
    //   "params": { "pin": 25, "freq_hz": 1000, "initial_permille": 0 } }
  ]
}
```

* `pin` numbers are provided by a platform `PinFactory`.
* For GPIO inputs, `irq.edge` may be `rising`, `falling`, `both`, or `none`. `debounce_ms` is applied in the worker (not the ISR).
* Unknown device types are ignored. Removing a device clears its retained `…/info` and marks `…/state` as `down`.

---

## 6. Concurrency and scheduling

* **I²C and similar sensors:** one worker per physical bus serialises `Trigger`/`Collect`, with bounded retries on `ErrNotReady`. Periodic cadences and ad-hoc `read_now` requests are co-ordinated.
* **GPIO inputs:** one **IRQ worker** manages all configured input pins that support interrupts. The ISR path is minimal (read + non-blocking channel send). The worker applies debounce, derives edges, and emits `gpio` events.

This arrangement keeps hardware access predictable and bounded in memory and CPU.

---

## 7. Adaptors

Adaptors wrap concrete drivers and implement:

```go
type Adaptor interface {
  ID() string
  Capabilities() []CapInfo
  Trigger(ctx context.Context) (time.Duration, error)
  Collect(ctx context.Context) (Sample, error)
  Control(kind, method string, payload any) (any, error)
}
```

* Sensor adaptors (e.g. AHT20) implement `Trigger/Collect`.
* **GPIO adaptor is control-only** (no polling or goroutines); events come from the IRQ worker.
* PWM adaptor is control-only.

Adaptors must not spawn goroutines or publish directly.

---

## 8. Streaming capabilities (UART) — proposed shape (unchanged)

Planned handle-based model for `bridge`:

* Open: `hal/uart/<cap-id>/open` → `{ "ok": true, "handle": "...", "mtu": 512 }`
* Data plane: `hal/uart/<handle>/tx` (bytes), `hal/uart/<handle>/rx` (bytes); `…/state` retained
* Close: `hal/uart/<handle>/close`

---

## 9. Consuming HAL from other services

* Discover capabilities via retained `…/info` topics.
* Subscribe to `…/value` (sensors) and `…/event` (GPIO inputs) as required.
* Use request–reply for control.
* Do not hard-code numeric ids; resolve from retained messages or configuration.
* Monitor `…/state` for health.

---

## 10. Migration notes from `hal.lua`

* Topic root remains `hal/capability/...`.
* `hal.lua` used polling or driver-specific signalling for some GPIO. TinyGo HAL uses a standardised **interrupt + worker** model; consumers should switch to `…/event`.
* Device inventory topics are not yet present; rely on capability `…/state` until introduced.
* Control methods are explicit per capability family rather than arbitrary driver pass-throughs.

---

## 11. Extending `hal.go`

1. Add an adaptor (wrap driver; respect timeouts; avoid goroutines).
2. Extend config schema minimally to pass parameters.
3. Publish retained `…/info` and `…/state`, and live `…/value` or `…/event` as appropriate.
4. Add well-named control methods and document payloads.
5. Keep ISR paths minimal; use workers and bounded queues for any event sources.

---

## 12. Error handling and resilience

* All driver calls are time-bounded (worker config).
* Use `ErrNotReady` to signal transient sensor readiness.
* Queues are bounded; low-priority submissions may be rejected under load.
* GPIO ISR channel is buffered; overflow is counted and can be surfaced via metrics.
* `…/state.link` uses `up`, `degraded`, or `down` to inform consumers.

---

## 13. Testing

* Use fake factories (I²C, GPIO) to run unit tests without hardware.
* Contract tests should validate:

  * retained `info/state` creation and removal;
  * sensor `value` payload shapes;
  * GPIO `event` payloads and debounce behaviour;
  * control request–reply semantics and error reporting.

---

## 14. Roadmap

* Device inventory topics: `hal/device/<type>/<id>` (retained).
* UART/TCP stream capabilities for `bridge` (handle-based RX/TX).
* Multiple bus workers (SPI, UART) using the same scheduling policy and priority classes.
* Hot-plug managers where the platform permits.
* Optional ACLs at the HAL boundary for sensitive controls.
* Stable capability identifiers persisted across boots if required.

---

## 15. Summary

`hal.go` maintains the essential properties of `hal.lua`: isolation of hardware access, a uniform bus contract, and retained discoverability. For TinyGo it adds a predictable worker model for sensors and a standardised, interrupt-driven GPIO pipeline. Services written against HAL remain independent of specific hardware choices, supporting rapid and safe iteration.
