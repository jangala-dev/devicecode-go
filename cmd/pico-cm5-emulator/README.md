# Pico CM5 emulator

This command builds a very small Pico 1 firmware that behaves like a CM5-side
Fabric peer for hardware bring-up. It is intended for the two-Pico setup:

```text
Pico 1 UART0 TX GP0  -> Pico 2 UART1 RX GP5
Pico 1 UART0 RX GP1  <- Pico 2 UART1 TX GP4
Pico 1 GND           <-> Pico 2 GND
```

Do not connect 3V3 or VSYS between the boards unless you are deliberately
powering one board from the other.

## Pico 2 under test

For the first physical UART protocol test, flash the Pico 2 with the hwtest
staging backend:

```sh
tinygo flash -stack-size=3KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1 fabric_uart_hwtest" main.go
```

This keeps production flash/apply disabled and stages into the safe digest/count
backend.

## Pico 1 emulator

Flash the Pico 1 with:

```sh
tinygo flash -stack-size=3KB -monitor -scheduler tasks \
  -target=pico -tags "pico_cm5_emulator" ./cmd/pico-cm5-emulator
```

The emulator opens UART0 at 115200 baud, sends a Fabric hello, calls
`cap/self/updater/main/rpc/prepare-update`, transfers a deterministic 1024-byte
blob to `updater/main`, commits the transfer, and waits for `xfer_done`.

When monitored over USB it prints each major phase. When running headless, the
onboard LED gives status:

```text
fast blink     failure
mostly-on blink pass / alive
```

This test does not send `commit-update` and cannot reboot the Pico 2.

### Timing note

The emulator uses a 180 second end-to-end script timeout. On the full Pico 2
appliance image the first `prepare-update` can be delayed by HAL and retained
state publication, so a shorter timeout can expire just as the transfer phase
begins. The emulator logs `prepare-update sent`, `xfer_begin sent`,
`xfer_ready received`, and `xfer_need next=0 received` to make it clear which
phase is blocking.

### JSONL reader note

The emulator reader treats UART as a byte stream, not as one read per line. It
only releases bytes from the RX ring up to the newline that completed the line.
If the same UART read span also contains the start of the next JSONL frame,
those bytes remain in the ring and are consumed by the next `readLine` call.
This is important because the reactive UART path can legitimately deliver
`...\n{` in one readable span when the peer is sending frames back-to-back.

### Chunk-size tags

By default the emulator uses 2048-byte chunks, matching the current advertised
`max_chunk_size` used by the MCU updater prepare response. This is the normal
large-transfer test shape:

```sh
tinygo flash -stack-size=8KB -monitor -scheduler tasks \
  -target=pico -tags "pico_cm5_emulator pico_cm5_payload_200k" \
  ./cmd/pico-cm5-emulator
```

Use 1024-byte chunks for an intermediate setting:

```sh
tinygo flash -stack-size=8KB -monitor -scheduler tasks \
  -target=pico -tags "pico_cm5_emulator pico_cm5_payload_200k pico_cm5_chunk_1024" \
  ./cmd/pico-cm5-emulator
```

Use 256-byte chunks as a stop-and-wait stress test:

```sh
tinygo flash -stack-size=8KB -monitor -scheduler tasks \
  -target=pico -tags "pico_cm5_emulator pico_cm5_payload_200k pico_cm5_chunk_256" \
  ./cmd/pico-cm5-emulator
```

### MCU transfer probe

For a targeted MCU-side transfer trace without the full `fabric_trace` frame dump,
flash the Pico 2 with `fabric_xfer_probe`:

```sh
tinygo flash -stack-size=8KB -monitor -scheduler tasks \
  -target=pico2 -tags "pico_bb_proto_1 fabric_uart_hwtest fabric_xfer_probe" main.go
```

The probe logs chunk receive offsets, write start/done, receiver retries,
digest/decode errors, stale/future chunks and commit start/done. It is intended
to explain receiver-driven retries during large transfers.
