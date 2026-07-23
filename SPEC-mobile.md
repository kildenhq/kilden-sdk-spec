# Kilden Mobile Client SDK Specification

Status: **draft** · Spec version: **0.1** (versioned together with
[SPEC.md](SPEC.md), see its §11)

This document is the authority for the **mobile-session surface** of Kilden's
mobile client SDK (`@kilden-io/expo`, Expo / React Native). It is a companion
to [SPEC.md](SPEC.md), not a replacement: the wire protocol (§4), batching
(§5), identity signing (§6), trust model (§7) and flag semantics (§8) of the
server spec govern the mobile SDK unchanged. What this document adds is the
part the server spec deliberately excludes — sessions, screens, app
lifecycle and exception capture — which exists only on stateful clients.

Everything specified here travels **inside `properties`** (opaque to
capture, per SPEC.md §4.5): no new envelope fields, no mock-server changes.

## 1. Sessions — `$session_id`

Every event the mobile SDK emits carries `properties.$session_id`:

- A **UUID v7**, generated client-side.
- **Rotation**: a new id is generated when more than **30 minutes** have
  passed since the last activity. Activity is *any emitted event* — there is
  no other clock. Rotation happens lazily, at the next emit.
- **Persistence**: the pair `{id, last}` (`last` = epoch ms of last
  activity) is stored under the key `kilden_session` in the SDK's key-value
  storage, so an app restart within the window continues the same session.
  Without working storage the SDK degrades to an in-memory session — events
  are never dropped or left unstamped because storage failed.
- An explicit `$session_id` passed by the caller in event properties wins
  over the stamp (consistent with the context-merge rule: explicit
  properties always win). The override is accepted **verbatim and
  unvalidated** — properties are opaque end to end (SPEC.md §4.5) and the
  SDK never rejects or rewrites them. A non-UUID override simply forms its
  own session key downstream; keeping overrides well-formed is the
  caller's responsibility.

These are the same semantics as the web SDK (`kilden-sdk-js/src/session.ts`)
so a "session" means one thing across the product: the `web_sessions`
aggregation and the panel's session join (`$session_id` extraction) consume
web and mobile events identically.

## 2. Screens — `$screen`

`screen(name, properties?)` emits a `$screen` event with
`properties.$screen_name = name`.

The one non-negotiable rule: **`$screen_name` is the static route name,
never the resolved URL or params** — `user/[id]`, not `user/ana@mail.com`.
Route params routinely contain PII; the static name never does. Navigation
integrations must source the name from the navigation library's route *name*
(react-navigation `route.name`, which expo-router derives from the file
path) and must never interpolate params into it.

A navigation integration collapses consecutive duplicates (re-renders of the
same route emit nothing) and reports the current route on attach.

## 3. App lifecycle — `$app_opened` / `$app_backgrounded`

Driven by the platform's app-state source (React Native `AppState`). Default
**on**; disabled with `trackAppLifecycle: false`. Without a lifecycle source
(plain Node, tests, missing adapter) no lifecycle event is ever emitted.

- `$app_opened` with `properties.$from_background`:
  - `false` — cold start (client construction with a lifecycle source).
  - `true` — return to `active` from a non-active state.
- `$app_backgrounded` — transition from `active` to `background` or
  `inactive`. The iOS `inactive → background` hop emits **once**, not twice.

Both count as session activity, so returning to the foreground after more
than 30 minutes starts a new session (rotation on the `$app_opened` emit).

## 4. Exceptions — `$exception` (opt-in)

Off by default; enabled with `captureExceptions: true`. The SDK wraps the
runtime's global error handler (`ErrorUtils` on React Native) and emits, per
uncaught error:

| Property | Content |
|---|---|
| `$exception_type` | `error.name`, or `"Error"` for non-Error throwables |
| `$exception_message` | the message, **scrubbed** (below) |
| `$exception_fatal` | the runtime's `isFatal` flag as a boolean |
| `$exception_stack` | scrubbed stack, capped at 4000 chars (omitted when absent) |

Ordering per uncaught error: the SDK **first** enqueues the `$exception`
event and, when fatal, initiates a flush; **then** it delegates to the
previously installed handler (dev red screen, crash reporters). Delegation
always happens, even when reporting itself fails. Delivery of a fatal
exception is best-effort: the flush is initiated before delegation, but a
previous handler that terminates the process can still preempt the network
round trip — `persistQueue: true` closes that gap (the event is on disk and
delivers on the next launch).

### Scrubbing

Exception messages routinely drag PII along (emails in auth errors, ids and
card numbers interpolated into strings). Before entering the queue, message
and stack are transformed, **in this order**:

1. Email addresses → `[email]`.
2. Contiguous digit runs of 6 or more → `[digits]` (card numbers, numeric
   ids, unformatted phone numbers; short numbers like line/row references
   survive).
3. Message capped at 1000 characters (applied last, after redaction).

Known limitation, accepted for v1: digits split by separators are **not**
normalized before matching — a formatted phone like `555-123-4567` has no
6-digit contiguous run and survives scrubbing. Implementations must not
silently "improve" on this individually; widening the contract is a spec
change so every SDK keeps the identical privacy guarantee.

This is a **deliberate, documented exception** to SPEC.md's contract 3
("never mutate customer data"): like timestamp formatting, it is an
obligation of the surface, not a mutation of the customer's data — the
customer never chose to send an uncaught error's payload, and unscrubbed
messages would turn a crash into a data leak.

## 5. Session recording config

The `/decide` response's `sessionRecording` block (`{enabled, sampleRate}`,
already served — see SPEC.md §8.4) is parsed and exposed by the mobile SDK:

- An absent or **structurally invalid** block (missing, `null`, a
  non-object, an array) leaves the previous value (initially `null`)
  untouched — backward compatibility is unconditional.
- A structurally valid block is always adopted, field by field: `enabled`
  is `true` only for a literal `true`; a `sampleRate` outside `[0, 1]` or
  non-numeric falls back to `1` (the block is still adopted — only the
  field falls back).
- The config is project-level: it survives `identify()`/`reset()`.

**Nothing records in this phase.** The parsed config is deliberate wiring
for the future mobile replay kill-switch (wrapper card `expo-1`, phase 2):
when replay ships, `enabled: false` from the server must stop recording
without an SDK release.
