# Board Test Harness for Pico (RP2040)

This harness exercises Pico-based boards using HAL. It sequences the power rails up and down for multimeter checks and verifies that values are arriving from the **LTC4015** (battery + charger) and **AHT20** (temperature + humidity). It reports **PASS/FAIL** on both UARTs and flashes the button LED (double short = PASS, single long = FAIL).

## Prerequisites

* Pico wired as per the selected setup (`pico_bb_proto_1`).
* Button LED connected, if you want to use the visual feedback.
* Access to two external UARTs for observation (see pins below).

## Build targets and pins

Build tags select the wiring plan and initial HAL config:

* `-tags "pico pico_bb_proto_1"`

  * UART0 TX/RX: GPIO **0/1**
  * UART1 TX/RX: GPIO **4/5**

> USB CDC still prints to the console; UART0 and UART1 are used by the test harness for PASS/FAIL lines as well.

## Build and flash

or:

```bash
tinygo flash -target pico -tags "pico pico_bb_proto_1" ./cmd/boardtest
```

## What the harness does

1. Starts the in-process HAL and waits for `hal/state=ready` (non-fatal timeout).
2. Opens `serial_raw` sessions on **uart0** and **uart1** and prints lines to both.
3. Repeatedly:

   * Powers **up** the rails in order, with a short delay between each.
   * Dwells with rails **up** so you can measure with a multimeter.
   * Powers **down** the rails in reverse order, again with a delay.
   * Checks that recent values have been received for:

     * `power/battery internal` (LTC4015)
     * `power/charger internal` (LTC4015)
     * `env/temperature core` (AHT20)
     * `env/humidity core` (AHT20)
   * Prints **[PASS]** or **[FAIL]** (listing any missing/stale readings).
   * Flashes LED:

     * PASS: two short flashes
     * FAIL: one long flash

HAL auto-polling is assumed; the harness only observes messages.

## Interpreting output

* USB console and both UARTs will show lines such as:

  * `rail up: cm5`
  * `[PASS] rails toggled; LTC4015 + AHT20 values observed recently`
  * `[FAIL] missing or stale: [battery humidity]`
* Button LED:

  * Two quick blinks = cycle passed.
  * One long blink = cycle failed.

## Adjustments (edit `main.go` constants)

* `powerSeq`: list of switch names to exercise. Must match your setup (e.g. `mpcie-usb`, `m2`, `mpcie`, `cm5`, `fan`, `boost-load`). Remove or add to fit the board.
* Timings:

  * `stepDelayUp` / `stepDelayDown`: delay between per-rail operations.
  * `dwellUp` / `dwellDown`: time held fully up or fully down.
  * `freshMaxAge`: how recent a sensor value must be to count as OK.
* `cyclesToRun`: set to a positive number to run a fixed count then halt (leave as `0` to loop).
