# Kilden Server SDK Specification

Status: **draft** · Spec version: **0.1**

This document is the single authority for the behavior of Kilden's five
server-side SDKs — PHP, Node (TypeScript), Python, Ruby and Go. The goal is
not "having SDKs": it is that all five behave **identically** given the same
input, and keep doing so two years from now. A pull request against any SDK
that changes behavior without changing this spec (and its test vectors) is
rejected by policy.

The companion artifacts in this repo are part of the spec, not extras:

- [`vectors/`](vectors/) — frozen test vectors every SDK must pass in CI.
- [`mockserver/`](mockserver/) — the mock capture server every SDK's
  integration tests run against, replacing per-language handwritten mocks
  (which is exactly where divergence hides).

## 1. Scope

These SDKs run in trusted backend code, hold a **secret** write key, and have
no notion of a browser session. They are not ports of the web SDK
(`kilden-sdk-js`), whose surface exists because of the browser: persisted
anonymous identity, autocapture, session replay.

The following do **not** exist server-side, deliberately. Do not port them
"for completeness":

- autocapture, session replay
- persisted super properties
- persisted `anonymous_id` / identity state of any kind
- `reset()`, `optOut()`
- `group()` — reserved, consistent with the web SDK: it must not exist yet

Each SDK implements this surface idiomatically — `snake_case` in Ruby and
Python, `camelCase` in PHP and TypeScript, functional `ClientOption`s in Go —
but the **semantics** are this document's and may not vary.

## 2. Public surface

Signatures below are pseudo-PHP; §12 maps them to each language's idiom.

### 2.1 Constructor

```php
$kilden = new Kilden\Client(string $secretWriteKey, array $options = []);
```

| Option           | Default                      | Meaning |
|------------------|------------------------------|---------|
| `host`           | `https://ingest.kilden.io`   | Base URL; `POST {host}/capture` and `POST {host}/decide` |
| `flush_at`       | `20`                         | Queue length that triggers a flush |
| `flush_interval` | `10`                         | Seconds between periodic flushes |
| `max_queue_size` | `10000`                      | Hard cap on queued events (contract 7) |
| `timeout`        | `3`                          | Seconds per HTTP request |
| `transport`      | `null`                       | Transport instance; `null` = autodetect |
| `debug`          | `false`                      | Verbose logging, `$`-prefix warnings |
| `enabled`        | `true`                       | `false` = full no-op (tests, CI, local dev) |

No other options exist. Future extension happens by adding keys here — never
by changing method signatures.

The constructor **may throw** (contract 2), and must when:

- the write key is missing or empty;
- the write key is a **public** key — any key with the `wk_` prefix. Public
  keys degrade server events to `source=client` and break the trust model
  (see §7). The error message must say to use the secret key and to keep it
  out of browsers;
- no transport is available (PHP: neither curl nor stream wrappers) and
  `enabled` is not `false`.

Anything after construction never throws (contract 1).

### 2.2 Methods

```php
$kilden->track(string $distinctId, string $event, array $properties = [], array $opts = []): void;
$kilden->identify(string $distinctId, array $traits = [], array $opts = []): void;
$kilden->alias(string $previousId, string $distinctId): void;
$kilden->isEnabled(string $flagKey, string $distinctId, array $opts = []): bool;
$kilden->getFeatureFlag(string $flagKey, string $distinctId, array $opts = []); // false | true | string
$kilden->flush(): void;   // blocking: drain the queue now
$kilden->close(): void;   // flush + stop the worker; idempotent
```

`$opts` on `track`/`identify` accepts exactly two keys:

- `timestamp` — event time as ISO 8601 UTC (§4.4). Default: now.
- `uuid` — event UUID for retry idempotency. Default: a fresh UUID v7.

`$opts` on `isEnabled`/`getFeatureFlag` accepts exactly two keys (§8):

- `person_properties` — map sent to `/decide`; overrides stored person traits
  for this evaluation only. Reserved for local evaluation later — the
  signature is frozen today so local eval arrives without an API change.
- `default` — what to return when Kilden cannot answer (timeout, network
  error, non-200, unknown flag). Defaults: `false`.

## 3. Behavior contracts

Numbered and testable. SDK READMEs may summarize them; this list is the law.

1. **The public API never throws in the hot path.** After construction,
   `track()`, `identify()`, `alias()`, `flush()`, `close()` and the flag
   methods never raise, whatever the input: invalid input is dropped and
   logged. Telemetry must never take down a customer request.
2. **The constructor is the one place that fails fast** (§2.1). A
   misconfigured SDK should die at boot, not at 3am in production.
3. **Never mutate customer data.** No trimming, lowercasing, normalizing or
   coercion of event names, distinct_ids, property keys or values. What the
   caller passed is what goes on the wire.
4. **`distinct_id` is required and explicit on every call.** The SDK holds no
   identity state. An empty `distinct_id` (or empty `event`) means the event
   is dropped with a warning. `distinct_id` and `event` must be strings; a
   non-string is dropped with a warning in dynamic languages and a compile
   error in typed ones. Events exceeding the wire limits (§4.2) are dropped
   client-side with a warning — the server would reject the whole batch
   otherwise.
5. **`$`-prefixed event names and property keys belong to Kilden.** Using
   them logs a warning when `debug` is on, and the event is **sent anyway** —
   never break customers.
6. **One UUID v7 per event, generated at call time** (or taken from
   `$opts['uuid']`). This is what makes retries idempotent: ClickHouse
   deduplicates by `uuid`. Generated UUIDs are lowercase canonical form,
   version 7; explicit UUIDs are sent verbatim (any RFC 4122 UUID the caller
   chose).
7. **The in-memory queue is bounded.** At `max_queue_size` the **new** event
   is dropped (never the old ones), a warning is logged, and a dropped-events
   counter the caller can read is incremented. Never block the caller's
   thread, never grow without bound.
8. **Retries: exponential backoff with jitter, honoring `Retry-After`.**
   Retryable: 429, 5xx, timeouts, network errors. Not retryable: any other
   4xx — the batch is dropped and logged (retrying a 401 is spam). Policy in
   §4.3.
9. **Fork safety (Python, Ruby).** Preforking servers (gunicorn, puma,
   unicorn) fork after import: the child inherits the parent's queue and a
   dead worker thread. The SDK must detect a PID change on every enqueue and
   respond by discarding the inherited queue and starting a fresh worker.
   Discarding is correct: the parent still owns those events; sending them
   from the child too would duplicate them.
10. **Shutdown.** `close()` is explicit and idempotent. Each SDK also
    registers a best-effort automatic hook (`register_shutdown_function`,
    `atexit`, `at_exit`, none in Go — Go programs call `Close()`). The final
    flush has a deadline of **10 seconds**; whatever has not drained by then
    is dropped and logged. A process must never hang on telemetry.
11. **Batch payload** (§4.1): `POST {host}/capture` with `write_key`,
    `sent_at` and `batch[]`. `Content-Encoding: gzip` when the language does
    it cheaply (§4.5). No JWT anywhere: server-side, the secret key **is**
    the authentication (`source=server`, `verified=true`).
12. **Secret key only.** The constructor rejects public (`wk_`) keys with a
    clear error (§2.1). Symmetric to the platform: `/decide` rejects secret
    keys server-side with 403.

## 4. Wire protocol

### 4.1 `POST {host}/capture`

Headers:

- `Content-Type: application/json`
- `Content-Encoding: gzip` — optional, §4.5
- `User-Agent: kilden-<lang>/<version>` (e.g. `kilden-php/0.1.0`)

Body — exactly these keys, nothing else:

```json
{
  "write_key": "sk_...",
  "sent_at": "2026-07-14T12:34:56.789Z",
  "batch": [
    {
      "uuid": "0197fa10-7a2b-7c3d-8e4f-5a6b7c8d9e0f",
      "event": "order_completed",
      "distinct_id": "user_42",
      "properties": { "revenue": 99.9, "currency": "CLP" },
      "timestamp": "2026-07-14T12:34:56.702Z"
    }
  ]
}
```

- `sent_at` is stamped **when the request is built** (not when the event was
  queued); the server uses it for clock-skew correction
  (`t_real = t_event + (server_now - sent_at)`).
- `properties` is always present, `{}` when empty. It is an opaque JSON
  object to the platform — never typed, never validated beyond being JSON.
- `timestamp` is per event: the `$opts['timestamp']` value, or the wall
  clock at call time.

### 4.2 Limits (server-enforced, SDK pre-validated)

| Limit | Value | SDK behavior |
|---|---|---|
| Events per request | 1000 | flush in chunks of ≤1000 |
| `event` length | 200 bytes | drop event + warn |
| `distinct_id` length | 512 bytes | drop event + warn |
| Request body | 5 MiB (after compression) | keep batches well under; oversize response is not retryable |

### 4.3 Response handling

| Response | SDK action |
|---|---|
| `200` (`{"status":"ok"}`) | done. Note: quota-exceeded projects also get `200` and events are dropped server-side — deliberate, so SDKs do not retry |
| `400` | drop batch + log (malformed — retrying cannot fix it) |
| `401` | drop batch + log (unknown write key) |
| `403` | drop batch + log (origin not allowed) |
| `413` | drop batch + log |
| `429` | retry, waiting `Retry-After` seconds when the header is present |
| `5xx` | retry |
| timeout / network error / corrupt response | retry |

Retry policy, frozen: up to **3 retries** per request (4 attempts total).
Backoff before retry *n* (1-based) is `min(0.5 * 2^(n-1), 30)` seconds —
0.5s, 1s, 2s — multiplied by a random jitter factor in `[0.5, 1.5]`. A
`Retry-After: <seconds>` header on 429 replaces the computed backoff for that
retry (no jitter). When retries are exhausted the batch is dropped, logged,
and counted in the dropped-events counter.

Failed batches are **not** re-queued into the main queue (they would shuffle
ordering and could evict fresh events); the retry loop owns the batch until
success or exhaustion.

### 4.4 Timestamp format

Frozen for every SDK: `YYYY-MM-DDTHH:MM:SS.mmmZ` — UTC, exactly three
fractional digits, `Z` suffix, no offset form. Both `sent_at` and event
`timestamp`s. Caller-supplied timestamps are converted to this form (a
formatting obligation on the SDK, not a mutation of data). Caller values that
cannot be interpreted as a time make the event drop with a warning
(contract 1: never throw).

### 4.5 Compression

gzip the body when it is bigger than **1024 bytes** and the language's
standard library makes it cheap (all five qualify). `Content-Encoding: gzip`
exactly — no `x-gzip`, no lists. The 5 MiB body limit applies to the
compressed bytes.

### 4.6 Event kinds

| Call | `event` | `properties` |
|---|---|---|
| `track(id, name, props)` | `name`, verbatim | `props` |
| `identify(id, traits)` | `$identify` | `{"$set": traits}` (empty object allowed) |
| `alias(previousId, distinctId)` | `$alias` | `{"$alias": distinctId}`, and the envelope `distinct_id` is `previousId` |

`alias` attaches `distinctId` as a new identity of the person `previousId`
already resolves to — mirror of the platform's resolver, where
`distinct_id` must be the **existing** identity and `properties.$alias` the
one being added. Both ids empty-checked per contract 4.
