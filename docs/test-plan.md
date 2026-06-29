# Big Box MCU Fabric/update hardware test guide

This guide is for testing the Pico 2 MCU firmware on real Big Box hardware, with the CM5/Lua Devicecode side acting as the Fabric sender.

## Current baseline

The current MCU firmware has passed the following local gates:

* idle appliance firmware at 3 KB stack;
* two-Pico Fabric transfer testing with a Pico 1 CM5 emulator;
* 200 KiB streamed transfer at 2,048-byte chunks;
* transfer completion through `xfer_done`;
* receiver-driven retry of damaged chunks;
* 512/512 HAL serial session rings;
* yield-free `serial_raw` bounded pump;
* stable large transfer at 6 KB stack.

Important stack baseline:

```text
3 KB: idle/non-transfer firmware only
5 KB: known to overflow during transfer
6 KB: tested successfully for transfer
8 KB: diagnostic/probe headroom only
```

Use **6 KB** for real Big Box transfer, staging and commit tests.

## Hardware assumptions

* MCU target: Pico 2.
* TinyGo scheduler: `tasks`.
* USB monitor is connected to the Pico 2 for MCU logs.
* CM5/Lua side is the real Fabric peer and update sender.
* MCU Fabric link is on `uart1`.
* Fabric protocol is JSONL over UART.
* CM5 should see the MCU as node `mcu`.
* The updater target is `updater/main`.
* Normal appliance telemetry continues during the test.

## General test rules

Do not start with the commit/reboot build.

Proceed in this order:

1. idle boot;
2. transfer-safe hwtest;
3. real flash staging with commit disabled;
4. commit/reboot only after staging succeeds.

For each MCU build, record:

* exact TinyGo command;
* full MCU USB monitor log;
* CM5/Lua update job id;
* CM5-side transfer result;
* final MCU software/update state observed by Device service;
* whether the MCU rebooted;
* whether `boot_id` changed after commit.

Stop and capture the full logs if any of these occur:

```text
panic:
goroutine stack overflow
repeated Fabric session open/close
allocation grows monotonically over several samples
Fabric transfer does not complete or retry
unexpected reboot before commit
```

## Gate 1: idle appliance boot

Purpose: confirm the normal appliance firmware boots and remains stable.

```sh
tinygo flash -stack-size=3KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1" main.go
```

Expected MCU log:

```text
[updater] policy safe-defaults:apply-disabled
[uart1] fabric session opening node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-disabled
[uart1] fabric session opened node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-disabled
```

Expected behaviour:

* no update transfer is accepted;
* `xfer_begin` should be refused or ignored as staging disabled;
* temperature/power logs continue;
* memory remains in a stable band.

Run for at least 60 seconds.

## Gate 2: transfer protocol hwtest

Purpose: test real CM5/Lua Fabric transfer without writing to production flash.

```sh
tinygo flash -stack-size=6KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1 fabric_uart_hwtest" main.go
```

Expected MCU log:

```text
[updater] policy safe-defaults:apply-disabled
[uart1] fabric session opening node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-controller:hwtest
[uart1] fabric session opened node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-controller:hwtest
```

Expected CM5/Lua behaviour:

* CM5 sends Fabric `hello`;
* MCU replies `hello_ack`;
* CM5 calls `prepare-update`;
* MCU returns target `updater/main`;
* CM5 streams the update body using `xfer_begin`, `xfer_chunk`, `xfer_commit`;
* MCU returns `xfer_ready`, `xfer_need`, and finally `xfer_done`.

This gate uses the updater-owned stage-controller path, but the backend is the safe digest/count hwtest sink. It must not reboot and must not write a real staged image.

Receiver retries are acceptable if the transfer completes. A retry means the MCU detected a bad chunk digest, kept the same offset, and requested that offset again. It is a recovery mechanism, not a failed test.

Pass criteria:

```text
prepare-update succeeds
xfer_done is observed
MCU does not reboot
no panic or stack overflow
heartbeat_stop reason transfer_done appears, if OTA diagnostics are enabled
```

## Gate 3: real flash staging, commit disabled

Purpose: test production staging of a valid signed `.dcmcu` image without allowing reboot.

```sh
tinygo flash -stack-size=6KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1 fabric_stage_enabled" main.go
```

Expected MCU log:

```text
[updater] policy safe-defaults:apply-disabled
[uart1] fabric session opening node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-controller:flash-stage
[uart1] fabric session opened node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-controller:flash-stage
```

Expected CM5/Lua behaviour:

* `prepare-update` succeeds;
* transfer target is `updater/main`;
* CM5 streams a valid signed `.dcmcu` artefact;
* MCU stages the image through the production flash staging path;
* transfer reaches `xfer_done`;
* `commit-update` is refused because apply is disabled.

Pass criteria:

```text
xfer_done observed
staging state is visible to CM5/Device service
commit-update is refused safely
MCU does not reboot
no panic or stack overflow
```

Do not include `fabric_uart_hwtest` in this gate. That tag deliberately selects the safe digest/count backend instead of production flash staging.

## Gate 4: real commit and reboot

Purpose: test the full production update path, including commit and reboot.

Only run this after Gate 3 has passed on the same hardware, wiring and CM5/Lua sender.

```sh
tinygo flash -stack-size=6KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1 fabric_stage_enabled fabric_apply_enabled" main.go
```

Expected MCU log:

```text
[updater] policy production-applier:commit-reboots
[uart1] fabric session opening node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-controller:flash-stage
[uart1] fabric session opened node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-controller:flash-stage
```

Expected CM5/Lua behaviour:

* update is prepared;
* signed `.dcmcu` image is transferred and staged;
* CM5 calls `commit-update`;
* MCU accepts commit;
* MCU reboots;
* after reconnect, CM5 observes the expected new image identity and a changed `boot_id`;
* CM5 update job resolves to succeeded.

Pass criteria:

```text
commit-update succeeds
MCU reboots intentionally
new boot_id observed
expected image_id observed
CM5 update job reaches succeeded
```

## Recommended CM5/Lua sender settings

Use the MCU-advertised `max_chunk_size` from `prepare-update`.

Current expected value:

```text
max_chunk_size: 2048
```

The sender should treat `xfer_need.next` as authoritative. If the MCU re-requests an earlier offset, resend from that offset. Do not assume monotonically increasing acknowledgements on a UART link.

Expected sender behaviour:

```text
send xfer_begin
wait for xfer_ready
wait for xfer_need next=N
send xfer_chunk offset=N
repeat until next == size
send xfer_commit
wait for xfer_done
```

A correct sender must tolerate:

```text
duplicate xfer_need
same-offset retry
ack timeout and resend
session restart with a new peer sid
```

## Notes on buffers and retries

The current MCU transport deliberately uses bounded serial session rings rather than full-frame buffering.

Current target constraint:

```text
HAL serial session RX/TX: 512/512
Fabric chunk size: 2048
```

This means Fabric must behave as a streaming protocol. It must not require the HAL serial session ring to hold a complete JSONL transfer frame.

Occasional chunk retries are acceptable. The important property is that the MCU detects corrupted chunks, does not advance the offset, and requests the same offset again.

A retry is suspicious only if:

```text
the same offset repeats many times
transfer never reaches xfer_done
the MCU panics or overflows stack
CM5 sees impossible future offsets
commit occurs without a completed transfer
```

## Known current limitations

* 5 KB stack overflows during transfer.
* 6 KB stack is the current tested transfer baseline.
* Diagnostic probes can perturb UART timing.
* Gate 4 can reboot the MCU and should only be run with a known-good signed image and a planned recovery path.

## Summary of commands

Idle only:

```sh
tinygo flash -stack-size=3KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1" main.go
```

Safe transfer hwtest:

```sh
tinygo flash -stack-size=6KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1 fabric_uart_hwtest" main.go
```


Real staging, no reboot:

```sh
tinygo flash -stack-size=6KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1 fabric_stage_enabled" main.go
```

Real staging, commit and reboot:

```sh
tinygo flash -stack-size=6KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1 fabric_stage_enabled fabric_apply_enabled" main.go
```
