# Measurement Worker

Per shared bus worker that coordinates `Trigger/Collect` with time-outs and retry/back-off.

## Behaviour

- `Submit(MeasureReq{ID, Adaptor, Prio})` enqueues a request. Priority requests may bypass congestion.
- `Trigger` is called under `TriggerTimeout`; returns a hint for due time.
- On due, `Collect` is called under `CollectTimeout`.
  - `ErrNotReady` → bounded retry with `RetryBackoff` up to `MaxRetries`.
  - Other errors → emit error result; if a priority re-read was requested while pending, re-trigger once.
- Results are emitted to the service via a fan-in channel.

## Defaults

If not configured:
- `TriggerTimeout=100ms`, `CollectTimeout=250ms`, `RetryBackoff=15ms`, `MaxRetries=6`, input queue size 16.

Tune via `halcore.WorkerConfig` if a builder needs different timings.
