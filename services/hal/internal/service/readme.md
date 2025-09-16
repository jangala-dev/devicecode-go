# HAL Service

Owns the event loop, device lifecycle, capability mapping, measurement scheduling, GPIO IRQ handling, and bus I/O.

## Bus Topics (outbound)

- Retained:  
  - `hal/state` → `{level, status, ts_ms, [error]}`
  - `hal/capability/<kind>/<id>/info`  
  - `hal/capability/<kind>/<id>/state`
- Live data:  
  - `hal/capability/<kind>/<id>/value`
  - `hal/capability/gpio/<id>/event`

## Bus Topics (inbound)

- Config: `config/hal` → `HALConfig`
- Control: `hal/capability/<kind>/<id:int>/control/<method>` (request–reply)

Supported generic control methods:
- `read_now` (best-effort priority sample)
- `set_rate` with `{"period_ms": <int>}` (clamped 200 ms … 1 h)

Device-specific controls are forwarded to the adaptor `Control` method, returning `{ok:false,"error":"unsupported"}` where not implemented.

## Scheduling

- Per shared bus, a `MeasureWorker` serialises `Trigger/Collect`.
- The service tracks next-due times per device and arms a single timer to the earliest.

## GPIO

- Registers IRQs on request from builders.
- For `gpio` capability devices: publishes `event` and retained state.
- For non-GPIO devices with IRQs (e.g. LTC4015 SMBALERT#): triggers priority reads on falling edges.

## Resilience

- Idempotent config application (add/retain/remove).
- Bounded queues; best-effort delivery on the internal bus; compact error codes via `halerr`.
