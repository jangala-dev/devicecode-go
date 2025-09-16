# HAL Utilities

Small utilities used across the service.

- `ResetTimer(t, d)` / `DrainTimer(t)`  
  Safe timer handling that avoids spurious wakeups.

- `DecodeJSON[T](src, *T)`  
  Accepts `[]byte`, `string`, or Go values; normalises to `T`.

- `BoolToInt(bool) int` and `ClampInt(v, lo, hi) int`  
  Minor helpers for payloads and bounds.

Prefer these over ad-hoc implementations to keep behaviour consistent.
