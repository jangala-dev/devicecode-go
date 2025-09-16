# HAL Constants

Topic tokens, control verbs, capability kind markers, and link state values used across the service.

- Tokens: `config`, `hal`, `capability`, `info`, `state`, `value`, `control`, `event`
- Control: `read_now`, `set_rate`
- Kinds: `gpio` (others are device-declared)
- Link: `up`, `down`, `degraded`

Keep these stable to avoid client breakage.
