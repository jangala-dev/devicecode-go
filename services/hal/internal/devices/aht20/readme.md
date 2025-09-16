# AHT20 Adaptor

Temperature and humidity sensor over I²C.

## Build

- MCU (`rp2040 || rp2350`): wraps the TinyGo AHT20 driver.
- Host: simulator producing deterministic readings for tests.

## Capabilities

- `temperature` → retained `info` includes units (`C`) and precision (0.1).  
- `humidity` → retained `info` includes units (`%RH`) and precision (0.1).

Values are emitted on `hal/capability/<kind>/<id>/value` with deci-units:
- `temperature`: `{ "deci_c": <int>, "ts_ms": <int> }`
- `humidity`: `{ "deci_percent": <int>, "ts_ms": <int> }`

## Sampling

- Declares `SampleEvery` ~ 2 s by default.
- Uses two-phase `Trigger/Collect`; `ErrNotReady` indicates the worker should retry after back-off.

## Params

```json
{ "addr": 56 } // default 0x38
````
