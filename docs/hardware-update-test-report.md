# MCU Update Hardware Test Status

Date: 2026-06-15

Reference plan: `devicecode-go/docs/test-plan.md`

## Summary

The current hardware run is Gate 2: transfer protocol hwtest. The harness builds the MCU with `pico_bb_proto_1 fabric_uart_hwtest`, so the MCU should use the safe digest/count hwtest sink and must not write production flash or intentionally reboot.

The run did not reach MCU transfer or Fabric handshake. It failed on the CM5 before the Fabric link opened: Fabric is waiting for its UART transport dependency.

## 2026-06-15 Gate 2 Attempt 1: Fabric Transport Dependency Missing

Command:

```sh
FW_MAJOR=15 CM5_UI_URL=http://172.28.100.1:8080 go run ./cmd/fw-update-e2e -n 1 -timeout 15m -link-timeout 30s
```

Result:

```text
wait link: timed out waiting for fabric link mcu-uart0
fabric status svc state="waiting_for_dependency" ready=false reason="config_changed" last_error="config_changed" links=1
```

Evidence from the harness diagnostics:

```text
cfg_hal uart_ports=[uart0:/dev/ttyAMA0]
svc/hal/status state="running"
raw/host/uart_manager/status=<missing>
raw/host/uart_manager/cap/uart/uart0/status=<missing>
raw/host/uart_uart0/status state="available"
raw/host/uart_uart0/cap/uart/uart0/status state="available"
cap/uart/uart0/status state="available"
```

Confirmed over the public CM5 state API:

```sh
curl -fsS http://172.28.100.1:8080/api/state/cfg/fabric
curl -fsS http://172.28.100.1:8080/api/state/raw/host/uart_manager/cap/uart/uart0/status
curl -fsS http://172.28.100.1:8080/api/state/raw/host/uart_uart0/cap/uart/uart0/status
curl -fsS http://172.28.100.1:8080/api/state/svc/fabric/status
```

Observed:

```text
cfg/fabric transport.source="uart_manager"
raw/host/uart_manager/cap/uart/uart0/status = null
raw/host/uart_uart0/cap/uart/uart0/status payload.available=true
svc/fabric/status state="waiting_for_dependency" dependency="transport"
```

Diagnosis:

Fabric config expects transport `source="uart_manager"`, `class="uart"`, `id="uart0"`, but the live CM5 HAL state exposes the raw UART capability under `source="uart_uart0"`. Fabric therefore never sees its required transport dependency become available, so it stays in `waiting_for_dependency` and never creates the `mcu-uart0` session.

This is not currently an MCU receive or `hello_ack` problem. The CM5-side Fabric link has not opened.

SSH follow-up found the exact Lua bug. The CM5 was running the expected files and config, but `services/hal.lua` built a source-candidate array like `{ meta.source_id, meta.source, ... }` and iterated it with `#candidates`. For UART, `meta.source_id` is nil and `meta.source` is `"uart_manager"`. In Lua, a sequence whose first element is nil has length 0, so HAL never checked `meta.source` and fell back to `<class>_<id>`, producing `uart_uart0`.

Fix: make `device_source_id()` test each candidate explicitly instead of using a nil-sensitive Lua array. A regression test now covers a device with `meta.source` and no `meta.source_id`.

## 2026-06-15 Gate 2 Attempt 2: Fabric Session Stuck In Hello

Command:

```sh
FW_MAJOR=15 CM5_UI_URL=http://172.28.100.1:8080 go run ./cmd/fw-update-e2e -n 1 -timeout 15m -link-timeout 30s
```

Result after copying the Lua HAL source fix to the CM5:

```text
raw/host/uart_manager/status state="available"
raw/host/uart_manager/cap/uart/uart0/status state="available"
fabric status svc state="running" ready=true links=1
wait link: timed out waiting for fabric link mcu-uart0
last state=hello
diagnosis=session stuck in hello; UART opened but no peer hello/ack observed
```

This confirms the first failure is fixed: Fabric now sees the configured UART
transport and starts the `mcu-uart0` link. The remaining failure is the Fabric
handshake itself.

Live CM5 state during the failure:

```text
state/fabric/link/mcu-uart0/component/session phase="hello"
established=false
wire_errors=0
bad_frame_count=0
local_node="bigbox-cm5"
cap/uart/uart0/status open=true path="/dev/ttyAMA0" baud=115200 mode="8N1"
```

The available MCU serial log for this run only shows telemetry and memory lines.
It does not contain the expected hardware Fabric boot lines:

```text
[uart0] fabric session opening node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-controller:hwtest
[uart0] fabric session opened node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=stage-controller:hwtest
```

Current diagnosis:

- The CM5-side dependency and link startup are now correct.
- The E2E command builds `pico2-a-b/build/devicecode_sealed.elf`, but it does
  not flash that image before waiting for the link. The already-running MCU
  must therefore already be a Gate 2 Fabric UART bootstrap image.
- If the MCU was not flashed with the `pico_bb_proto_1 fabric_uart_hwtest`
  image, the CM5 will sit in `hello` exactly like this.
- The CM5 UART is also currently in cooked tty mode (`icanon`, `echo`,
  `icrnl`, `opost/onlcr` enabled). That should be fixed in the Lua UART driver
  because Fabric wants a raw byte stream, but it is a separate CM5-side hygiene
  issue from proving whether the MCU is running the correct Fabric image.

Next diagnostic step: start the MCU serial watcher before flashing/rebooting
the MCU, flash the E2E-built `pico2-a-b/build/devicecode_sealed.elf`, and
confirm the `[uart0] fabric session opening/opened` lines appear before running
the update harness again.

## Gate Classification

This is Gate 2 from `devicecode-go/docs/test-plan.md`.

Why:

- TinyGo tags include `pico_bb_proto_1 fabric_uart_hwtest`.
- Stack baseline is 6 KB.
- The intended backend is the safe digest/count hwtest sink.
- The run should prove Fabric transfer protocol only; it should not prove production flash staging or commit/reboot.

The run failed before the Gate 2 transfer criteria could start.

## Next Actions

Confirm what source convention the CM5 runtime is actually using:

```sh
grep -n '"source": "uart' ./configs/bigbox-v1-cm-2.json
grep -n "source_id\|source = 'uart" src/services/hal/managers/uart.lua /usr/lib/lua/services/hal/managers/uart.lua 2>/dev/null
curl -s http://172.28.100.1:8080/api/state/raw/host/uart_manager/cap/uart/uart0/status
curl -s http://172.28.100.1:8080/api/state/raw/host/uart_uart0/cap/uart/uart0/status
```

If the CM5 is not running the expected `final-uart` source, redeploy it and rerun Gate 2.

If the CM5 is running the expected source and still publishes only `uart_uart0`, update the Lua Fabric config and devhost tests to use the same raw HAL UART source observed on hardware.

## Current Repo Notes

`devicecode-lua/final-uart` currently configures Fabric with `transport.source = "uart_manager"`, while the hardware run observed `uart_uart0`.

Devhost tests also expose the test UART adapter as `uart_manager`, so they do not currently catch this hardware source mismatch.

This report is intentionally uncommitted and can be shared as a gist.
