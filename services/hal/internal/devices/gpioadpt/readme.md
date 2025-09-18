# GPIO Adaptor

Simple GPIO input/output device with optional IRQ support.

## Modes

- `input` with `pull` (`up` | `down` | `none`) and optional `irq` (edges: `rising` | `falling` | `both`; `debounce_ms`).
- `output` with `initial` level and optional `invert`.

## Capability

- Exposes a single `gpio` capability with retained `info` `{pin, mode, invert, [pull], ...}`.

## Events and State

- On IRQ, publishes:
  - `event` (non-retained): `{edge:"rising|falling", level:0|1, ts_ms}`
  - `state` (retained): `{link:"up", level:0|1, ts_ms}`

## Controls

- Inputs: `get` → `{level:0|1}`
- Outputs: `set` → payload `{"level": bool}`; `toggle`

## Params

```json
{
  "pin": 17,
  "mode": "input",
  "pull": "up",
  "invert": false,
  "irq": {"edge": "falling", "debounce_ms": 10}
}
````
