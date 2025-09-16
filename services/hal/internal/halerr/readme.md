# HAL Error Codes

Central list of short, bus-friendly error values used in replies on control topics and internal signalling.

## Notable Values

- Service/control plane: `busy`, `invalid_period`, `invalid_capability_address`, `unknown_capability`, `no_adaptor`
- Build/config: `missing_bus_ref`, `unknown_bus`, `invalid_mode`, `unknown_pin`
- Generic/pass-through: `unsupported`

Keep messages short for constrained links and to standardise client handling.
