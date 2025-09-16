# Devices (Adaptors)

Each subfolder implements a device adaptor:
- Registers a `Builder` in `init()`.
- Constructs a platform device via factories (`halcore`).
- Exposes capabilities (`Capabilities()`), measurement cycle (`Trigger/Collect`), and optional `Control` methods.
- Provides MCU and host build paths via build tags.

Current adaptors:
- `aht20`: temperature/humidity over I²C.
- `ltc4015`: charger/PMIC over I²C with optional SMBALERT# IRQ.
- `gpio`: simple GPIO input/output with optional IRQ.

Add new devices by following the pattern in these packages.
