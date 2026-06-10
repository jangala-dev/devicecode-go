# UARTX probe build

This tree vendors a local copy of `github.com/jangala-dev/tinygo-uartx` under
`third_party/tinygo-uartx` so that the firmware can expose loss-attribution
counters while we debug Fabric transfers over real UART.

Normal builds use the same UART API. To enable the extra serial diagnostics,
add the `uartx_probe` build tag to the Pico 2 build:

```sh
tinygo flash -stack-size=8KB -monitor -scheduler tasks \
  -target=pico2 \
  -tags "pico_bb_proto_1 fabric_uart_hwtest fabric_xfer_probe uartx_probe" \
  main.go
```

The probe prints compact lines from the HAL `serial_raw` session worker:

```text
[uartx-probe] uart1 reason periodic rx_hw ... rx_drop ... rx_oe ... rx_fe ... sess_rx_avail ...
```

The most useful fields are:

- `rx_hw`: bytes read from the PL011 data register by the UARTX ISR.
- `rx_enq`: bytes successfully enqueued into UARTX's ISR RX ring.
- `rx_read`: bytes drained from UARTX by the HAL serial session worker.
- `rx_drop`: bytes dropped because the UARTX ISR RX ring was full.
- `rx_oe`, `rx_fe`, `rx_pe`, `rx_be`: PL011 overrun, framing, parity and break errors.
- `rx_max`: maximum observed UARTX ISR RX ring occupancy.
- `sess_rx_avail` / `sess_rx_space`: bytes in the HAL session shmring from UART to Fabric.
- `sess_tx_avail` / `sess_tx_space`: bytes in the HAL session shmring from Fabric to UART.

When Fabric logs `chunk_digest_error` with a shortened `encoded_len`, compare the
nearest `[uartx-probe]` lines. If `rx_drop` or `rx_oe` increments, the loss is at
or below the UARTX ISR ring. If those counters remain flat while the HAL session
ring is full, the loss is likely at the session boundary. If all counters remain
flat, look higher in the line assembly / Fabric parser path.


## Current bounded-session test

The Pico 2 board setups now use symmetrical 512-byte HAL serial session rings
for both raw UART devices. The Pico 1 CM5 emulator opens its UART session with
the same 512/512 constraint. This is deliberately smaller than a Fabric transfer
line; Fabric must rely on streaming, flow control and retry rather than requiring
the HAL session ring to hold an entire JSONL frame.

The old 32-byte RX rings came from the earlier raw-JSON telemetry shape and are
now too small for bidirectional Fabric traffic. The 512-byte setting is intended
as a bounded engineering test, not as a hidden full-frame buffer.

The local UARTX copy should be treated as an instrumentation branch. Once the
cause is confirmed, port only the relevant counter or behavioural fix upstream.
