# LTC4015 Adaptor

Battery charger/PMIC over I²C with optional SMBALERT# IRQ support.

## Build

- MCU: wraps the TinyGo LTC4015 driver and exposes a device-agnostic surface.
- Host: simulator implementing `ltcDev` for tests.

## Capabilities

- `power` (voltages, currents, die temperature, BSR, DACs) with retained units in `info`.
- `charger` (phase, input-limit flags, faults) plus retained vendor extension containing a bitfield dictionary for decoding compact `raw` masks.
- `alerts` event stream when present.

## Sampling

- Continuous readiness (`Trigger` returns zero delay).  
- Default `SampleEvery` ~ 2 s (overridable in params).

## Controls

- `set_input_current_limit` → `{"mA": int}`
- `set_charge_current` → `{"mA": int}`
- `set_vin_uvcl` → `{"mV": int}`
- `apply_profile` → chemistry-aware settings (`lead_acid` / `lithium`)
- `read_alerts` → drains pending alert registers

## IRQ (optional)

- Configure `smbalert_pin` (input, active-low).  
- HAL registers a falling-edge IRQ with debounce and will priority-read on alerts.

## Params (subset)

```json
{
  "addr": 16,
  "cells": 4,
  "chem": "lead_acid",          // or "lithium" or "auto"
  "rsnsb_uohm": 1500,
  "rsnsi_uohm": 5000,
  "qcount_prescale": 1024,
  "targets_writable": true,
  "sample_every_ms": 2000,
  "smbalert_pin": 21,
  "irq_debounce_ms": 10,
  "force_meas_sys_on": true,
  "enable_qcount": true
}
````
