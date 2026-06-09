# Hardware test plan: Fabric and MCU updater

This plan is intended for testing the Pico 2 MCU firmware against the CM5-side Devicecode stack.

The branch deliberately separates update risk into build-time gates. Do not skip directly to the commit/reboot gate unless the earlier gates have passed on the same board and wiring.

## Board and wiring assumptions

- Target: Pico 2.
- TinyGo scheduler: `tasks`.
- Normal telemetry/log monitor is over USB.
- MCU Fabric link is on `uart1` as `mcu-uart0`.
- CM5-side Fabric peer expects node `mcu`, peer `bigbox-cm5`, protocol `fabric-jsonl/1`.
- `uart0` remains the original local JSON telemetry stream.

## Common pass criteria

For each idle gate, let the firmware run for at least 60 seconds.

A gate passes if:

- the expected policy line appears;
- `uart1` Fabric session opens when applicable;
- no panic or stack overflow occurs;
- memory allocation returns to a stable band rather than increasing continuously;
- temperature and power events continue to be logged.

Stop and record the full log if any of these occur:

- `panic:`;
- `goroutine stack overflow`;
- repeated Fabric session open/close loops;
- allocation grows monotonically across several memory samples;
- no temperature or power output after boot.

## Gate 1: normal appliance idle

```sh
tinygo flash -stack-size=3KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1" main.go
```

Expected policy and Fabric mode:

```text
[updater] policy safe-defaults:apply-disabled
[uart1] fabric session opening node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-disabled
[uart1] fabric session opened node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-disabled
```

This is the product idle gate. Fabric runs, but update transfer is deliberately refused.

## Gate 2: transfer-capable, flash-safe appliance idle

```sh
tinygo flash -stack-size=3KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1 fabric_uart_hwtest" main.go
```

Expected policy and Fabric mode:

```text
[updater] policy safe-defaults:apply-disabled
[uart1] fabric session opening node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-controller:hwtest
[uart1] fabric session opened node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-controller:hwtest
```

This build exercises the updater-owned stage-controller boundary but uses the digest/count test staging backend. It must not reboot into a staged image. `fabric_apply_enabled` is intentionally ignored for hwtest/selftest builds, so this gate remains flash-safe.

## Gate 3: standalone Fabric protocol self-test

```sh
tinygo flash -stack-size=4KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1 fabric_uart_hwtest fabric_uart_selftest" \
  ./cmd/fabric-selftest
```

Expected success output:

```text
[fabric-selftest-fw] bootstrapping bus
[fabric-selftest-fw] updater started
[fabric-selftest-fw] starting fabric transfer self-test
[fabric-selftest-fw] ok xfer=selftest-xfer-1 bytes=1024 chunk=256 digest=61d42c9c
```

This image starts only the bus, updater hwtest staging backend, one MCU Fabric session, and a tiny in-process CM5 peer. It is the board-level protocol regression gate. It is not the product firmware image and is expected to use a 4 KB stack.

## Gate 4: real flash staging, commit disabled

```sh
tinygo flash -stack-size=3KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1 fabric_stage_enabled" main.go
```

Expected policy and Fabric mode:

```text
[updater] policy safe-defaults:apply-disabled
[uart1] fabric session opening node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-controller:flash-stage
[uart1] fabric session opened node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-controller:flash-stage
```

This build allows the CM5 peer to stream a valid signed `.dcmcu` image into the production A/B prestage path. `commit-update` remains disabled. Use this gate to test `prepare-update`, Fabric transfer, staging validation, and `xfer_done` without reboot risk.

Suggested CM5-side checks:

- Device sees the MCU component and updater capability.
- `prepare-update` returns an updater target of `updater/main`.
- the transfer target is `updater/main`.
- transfer completion is observed as `xfer_done`.
- `state/self/updater` reflects staged or equivalent post-stage state.
- `commit-update` is refused because apply is disabled.

## Gate 5: real commit and reboot

```sh
tinygo flash -stack-size=3KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1 fabric_stage_enabled fabric_apply_enabled" main.go
```

Expected policy and Fabric mode:

```text
[updater] policy production-applier:commit-reboots
[uart1] fabric session opening node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-controller:flash-stage
[uart1] fabric session opened node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-controller:flash-stage
```

This is the first build that can accept `commit-update` and arm reboot. Only run it after Gate 4 has passed with the same CM5 update path and a valid image. The production applier is enabled only when both `fabric_stage_enabled` and `fabric_apply_enabled` are present, and neither `fabric_uart_hwtest` nor `fabric_uart_selftest` is present.

Expected CM5-side result after commit:

- the CM5 update job reaches `awaiting_return` after commit;
- the MCU reboots;
- after reconnect, Device observes the expected `image_id` and a new `boot_id`;
- the CM5 Update service reconciles the job to `succeeded`.

## Artefacts to collect

For each gate, collect:

- exact TinyGo command;
- full serial monitor log from boot to at least 60 seconds, or through update completion;
- CM5-side update job id, if an update was attempted;
- final `state/device/component/mcu/software`;
- final `state/device/component/mcu/update` or equivalent update state;
- whether the MCU `boot_id` changed after commit.

## Known current baselines

The following have already been observed on Pico 2:

- Gate 1 passes at 3 KB with memory returning to roughly the 114-118 KB allocation band.
- Gate 2 passes at 3 KB with similar idle behaviour.
- Gate 3 passes at 4 KB after the low-stack Fabric codec changes.
- Gate 5 idle boot passes at 3 KB, before any CM5 update traffic.

The active production update path still needs testing with the CM5 peer and a valid signed `.dcmcu` artefact. Avoid combining `fabric_uart_hwtest` with Gate 4 or Gate 5; that tag deliberately selects the digest/count staging backend instead of real flash staging.
