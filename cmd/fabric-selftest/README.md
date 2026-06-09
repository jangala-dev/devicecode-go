# Fabric self-test firmware

This is a narrow board-level protocol test image. It does not start the main
appliance Reactor, HAL polling, Telemetry, or the normal UART sessions.

It starts only:

- an in-memory bus;
- the Updater service with the `fabric_uart_hwtest` staging backend;
- one MCU-side Fabric session;
- a tiny in-process CM5 peer cross-wired through shmring UART-shaped transports.

It then performs `hello`, `prepare-update`, `xfer_begin`, `xfer_chunk*`,
`xfer_commit`, and waits for `xfer_done`. It does not call `commit-update` and it
does not exercise the production A/B flash writer.

Example Pico 2 run:

```sh
tinygo flash -stack-size=3KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1 fabric_uart_hwtest fabric_uart_selftest" \
  ./cmd/fabric-selftest
```

If this image needs more than 3 KB stack, the active Fabric transfer hot path
itself needs further stack reduction before production transfer is enabled at the
3 KB appliance gate.
