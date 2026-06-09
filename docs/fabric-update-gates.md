# Fabric and updater hardware gates

This branch keeps the update path split into explicit build-time gates so the MCU firmware can be tested without accidentally enabling flash staging or reboot.

## Appliance idle gate

```sh
tinygo flash -stack-size=3KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1" main.go
```

Expected policy:

```text
transfer=stage-disabled
updater policy=safe-defaults:apply-disabled
```

This is the normal firmware boot/stability gate. Fabric runs on `uart1`, but `xfer_begin` is rejected with `stage_disabled`.

## Transfer-capable, flash-safe gate

```sh
tinygo flash -stack-size=3KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1 fabric_uart_hwtest" main.go
```

Expected policy:

```text
transfer=stage-controller:hwtest
updater policy=safe-defaults:apply-disabled
```

This uses the updater-owned stage controller, but the staging backend is a digest/count sink rather than the A/B flash writer. It is suitable for UART/Fabric transfer tests and cannot reboot into a staged image. Even if `fabric_apply_enabled` is accidentally combined with this gate, the updater remains on the safe `apply-disabled` policy.

## Standalone Fabric protocol gate

```sh
tinygo flash -stack-size=4KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1 fabric_uart_hwtest fabric_uart_selftest" \
  ./cmd/fabric-selftest
```

This firmware starts only the bus, updater test staging backend, one MCU Fabric session and a tiny in-process CM5 peer. It exercises Fabric hello, prepare-update RPC, transfer chunks, digest checks and xfer_done without the full appliance Reactor.

## Real flash staging gate

```sh
tinygo flash -stack-size=3KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1 fabric_stage_enabled" main.go
```

Expected policy:

```text
transfer=stage-controller:flash-stage
updater policy=safe-defaults:apply-disabled
```

This allows Fabric to stream a signed `.dcmcu` image into the production A/B prestage path. Commit/reboot remains disabled: `commit-update` still returns `commit_failed`. Use this only with a valid signed image and a CM5 peer.

## Real commit/reboot gate

```sh
tinygo flash -stack-size=3KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1 fabric_stage_enabled fabric_apply_enabled" main.go
```

Expected policy:

```text
transfer=stage-controller:flash-stage
updater policy=production-applier:commit-reboots
```

This is the first build that can accept `commit-update` and call the production A/B reboot applier. It should only be used once the flash staging gate has passed. The `fabric_apply_enabled` tag is deliberately effective only with `fabric_stage_enabled` and not with `fabric_uart_hwtest` or `fabric_uart_selftest`; other combinations remain on the safe `apply-disabled` policy.

## Detailed test plan

For step-by-step hardware instructions, expected logs and artefacts to collect, see `docs/hardware-test-plan.md`.
