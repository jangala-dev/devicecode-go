# Bridge: Design Overview

This note explains the purpose of the `bridge` service, how it works at a high level, and how we will build it in phases. It reflects our updated approach: **`bridge` is business-logic only and uses HAL to access transports** (eg. UART) so other services remain hardware-agnostic.

## 1. Purpose

`bridge` links the local in-process bus on one device to the bus on another device. It forwards subscriptions (“interest”) and publications so services can interoperate across devices **as if local**, with optional prefix/remap of topics.

Example uses:

* Forward telemetry from a Pico to a Pi.
* Allow a Pi service to publish to `switch/...` handled on the switch device.
* Present remote services under a local namespace (eg. `pico/system/#`).

Longer term, multiple links enable small meshes.

## 2. Out of scope (for now)

* Acting as a full MQTT broker.
* Exactly-once delivery.
* Complex routing scripts inside `bridge`.
* End-to-end cryptography across hops (we will start with transport security and boundary ACLs).

## 3. Core concepts

* **Local bus**: the existing in-process pub/sub.
* **Topic**: list of tokens (eg. `{"system","ram"}`) with `+` (single) and `#` (multi) wildcards.
* **Interest**: the set of topics that have local subscribers. Drives selective forwarding.
* **Mapping**: rules to rewrite or prefix topics at the link boundary.
* **Transport**: a byte stream between peers. Implemented via **HAL** (eg. HAL-UART). Pluggable.
* **Envelope**: a small message header (IDs, QoS, flags) plus payload.

## 4. Architecture (single link)

Each device runs a `bridge` instance per peer.

1. **Configuration**: waits for JSON on `config/bridge` (retained). Hot-reload applies deltas.
2. **Transport**: uses HAL to open a stream (eg. `hal/uart/<cap>/open`), monitors liveness, and reconnects with back-off.
3. **Control plane**: synchronises **interest** (SUB/UNSUB), pings, errors.
4. **Data plane**: forwards PUB frames in both directions, applying mapping, QoS, and policies.
5. **Request–reply**: rewrites `reply_to` for cross-link correlation and routes replies back.
6. **State and metrics**: publishes retained health at `bridge/state` and counters under `bridge/metrics/#`.

`bridge` does not touch GPIO/UART directly; it consumes HAL capabilities (streams) and exposes its own status on the bus.

## 5. Configuration

Delivered to `config/bridge` as JSON. Minimum fields select transport; optional sections define mapping and policy.

Example (HAL-UART):

```json
{
  "version": 1,
  "peer_id": "pi-01",
  "transport": {
    "type": "hal-uart",
    "hal_uart": { "cap_id": "uart0", "baud": 115200, "read_timeout_ms": 200, "write_timeout_ms": 200 }
  },
  "mapping": [
    { "direction": "up",   "match": "#",            "prefix": "pico/" },
    { "direction": "down", "match": "switch/#",     "prefix": ""      }
  ],
  "policy": {
    "allow_pub_up":   ["system/#","sensor/#","power/#"],
    "allow_pub_down": ["switch/#","time/#"],
    "qos_default": 0
  }
}
```

Notes:

* **direction**: `up` means local→remote; `down` remote→local.
* Start with prefix-only mappings; keep patterns hierarchical (`+`, `#` supported).
* Allow-lists constrain what may be forwarded while security is staged.

## 6. Topic mapping

Mapping is applied **per direction** and **before** interest checks:

* Input topic → apply the first matching mapping rule (or identity).
* Resulting topic is used for interest matching and forwarding.
* Reverse mapping is applied on the inbound path to present messages “as if local” where configured.

Keep rules simple and deterministic. Avoid overlapping rules where order would be ambiguous.

## 7. Interest propagation (selective routing)

* `bridge` ref-counts local subscriptions by pattern.
* On local SUB/UNSUB, compute mapped patterns and send `SUB/UNSUB` frames to the peer.
* Only publications that match **known remote interest** (or a configured allow-list) are forwarded.
* On new interest, the peer returns retained messages for those patterns (bounded; see §10).

This avoids flooding and enables “as-if-local” behaviour.

## 8. Delivery semantics

* **QoS 0**: best effort. No ACK. Use for bulk telemetry and diagnostics.
* **QoS 1**: at-least-once. Per-link message IDs with ACK and retry. Handlers must be idempotent.
* **Ordering**: best effort per topic over a single link. No cross-topic ordering guarantees.
* **Expiry**: optional `expiry_ms`; expired messages are dropped before send or on receipt.

## 9. Request–reply across the link

* Outbound requests carry `reply_to` and `correlation_id`.
* `bridge` replaces `reply_to` with a **return-path alias** and holds a short-lived map `(alias → local topic)`.
* Replies received with that alias are rewritten back to the original local `reply_to`.
* Timeouts are enforced; errors are propagated as normal bus replies.

## 10. Retained messages

* When sending `SUB`, the subscriber may request retained sync.
* The peer responds with retained matches, subject to limits:

  * max retained payload size;
  * max items per pattern;
  * optional paging.
* Clearing a retained topic remotely must propagate (retained `nil`).

## 11. Loop prevention and de-duplication

* Each frame carries:

  * `origin_id` (stable per device),
  * per-origin `msg_id`,
  * `ttl` (hop limit).
* A small LRU (`origin_id`,`msg_id`) per link drops duplicates under QoS 1 and prevents loops.
* `ttl` defaults to 8; frames with `ttl==0` are dropped.

## 12. Backpressure and priorities (progressive)

* Separate send queues per class:

  * **control** (SUB/UNSUB, PING, ACK) — highest;
  * **ops** (telemetry, state) — medium;
  * **bulk** (logs, dumps) — lowest.
* High-water marks per queue. Policy: stall lower classes or drop bulk first.
* Optional **credit/window** for slow links (UART).

## 13. Errors, health, and metrics

* Retained state: `bridge/state`

  * `level`: `idle` | `up` | `degraded` | `error`
  * `status`: short machine string (eg. `link_established`, `dial_failed_retrying`)
  * `ts_ms`, and `error` where relevant
* Metrics under `bridge/metrics/...` (non-retained):

  * `tx_msgs`, `rx_msgs`, `tx_bytes`, `rx_bytes`
  * `qos1_retries`, `duplicates_dropped`
  * `queue_depth/{control|ops|bulk}`
  * `rtt_ms`, `reconnects`

## 14. Security (staged)

* **Phase 1**: assume trusted UART; apply allow-lists at the bridge boundary.
* **Phase 2**: TLS for IP transports; per-device identity; topic-level ACLs.
* **Phase 3**: key rotation via control topics; audit events for critical writes.

## 15. Data format

**Frames** (length-prefixed):

* `SUB`, `UNSUB`, `PUB`, `ACK`, `PING`, `PONG`, `ERR`, `CLOSE`.

**Envelope** (CBOR or MsgPack):

```
{
  version: u8,
  msg_id: u32,            // per origin
  origin_id: bytes[16],   // device UUID
  topic: []token,         // list of tokens
  headers: {              // small map
    qos: 0|1,
    retained: bool,
    expiry_ms: u32,
    schema: u16,
    content_type: "..."
  },
  reply_to: []token?,     // optional
  correlation_id: u32?,   // optional
  payload: bytes,
  ttl: u8
}
```

## 16. HAL integration

Transports are opened and managed **via HAL**:

* Example: HAL-UART

  * `bridge` requests `hal/uart/<cap_id>/open` and receives a handle.
  * I/O uses `hal/uart/<handle>/rx` and `…/tx` topics.
  * `bridge` does not manipulate pins or UART directly.
* Other transports (TCP/WebSocket) may be provided by HAL in the same pattern, or by direct sockets on larger devices. The bridge transport registry abstracts this.

## 17. Phased delivery

### Phase 1 — Minimal telemetry uplink for ISOC Bolt (Pico → Pi)

* Config on `config/bridge`.
* HAL-UART transport, liveness (ping/pong), reconnect with back-off.
* Fixed allow-list for topics; unidirectional forwarding (Pico→Pi).
* QoS 0 only.
* Retained `bridge/state`.

**Done when**: 1 Hz telemetry appears on the Pi under a prefix (eg. `pico/system/#`) with ≤ 1 s p99 latency across link flaps.

### Phase 2 — Selective routing and bidirectional link

* Interest propagation (SUB/UNSUB) both ways.
* Mapping applied per direction; “as-if-local” topics.
* QoS 1 with ACK/retry; basic loop prevention (origin\_id + ttl).
* Request–reply routing across the link.
* Initial metrics.

**Done when**: local services publish/subscribe across the link unchanged; only interested topics traverse the link.

### Phase 3 — Resilience, policy, and security

* Priority queues and backpressure.
* Store-and-forward with expiry for intermittent links.
* Allow/deny lists and per-topic limits (rate, size).
* TLS for IP links and device identity; UART remains trusted or PSK-wrapped.
* Bounded retained sync.

### Phase 4 — Multi-peer / mesh and interoperability

* Multiple peers per device; de-dup across several hops.
* Optional discovery.
* Adapters: eg. MQTT bridge that maps the internal envelope to MQTT 5 safely.

## 18. Testing

* **Unit**: frame codec, config parsing, mapping resolution, back-off, state publication.
* **Property**: QoS 1 idempotence under duplicate deliveries; loop-freedom with ttl.
* **Link emulator**: delay, loss, duplication, corruption.
* **Integration**: Pico↔Pi over HAL-UART stub; retained sync; request–reply path.
* **Load**: queue limits, shedding policies, UART saturation.

## 19. Developer guidance

* Keep the service entry stable: `Run(ctx, conn)`; transport registration via a small interface.
* Separate **mapping** from **interest**. Mapping decides names; interest decides forwarding.
* Prefer small, composable components: transport, framing, router, supervisor.
* Defaults should be safe: small caps, conservative retries, allow-lists.
* Publish clear errors and health; avoid silent drops. Where policy drops occur, expose counters.
