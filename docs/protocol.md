# fanline wire & token format

Reference for everything a client or a non-Go implementation needs. The
transport is standard Server-Sent Events (WHATWG HTML §9.2); fanline adds
conventions on top, never a custom framing.

## Channels

A channel name is 1–16 dot-separated segments, each 1–64 characters of
`a-z 0-9 _ -` (lowercase only — names appear in URLs and event IDs).
Examples: `orders`, `orders.eu`, `dash.tenant-42.cpu_load`.

Token *patterns* extend names with wildcards:

| Pattern | Matches | Does not match |
|---|---|---|
| `orders.eu` | `orders.eu` | `orders.us`, `orders.eu.returns` |
| `orders.*` | `orders.eu` | `orders`, `orders.eu.returns` |
| `orders.**` | `orders.eu`, `orders.eu.returns` | `orders`, `invoices.eu` |
| `**` | every valid channel | — |

`*` matches exactly one segment; `**` is only valid as the final segment
and matches one or more. Matching is structural — no regexes, no prefix
string matching, so `orders` can never leak into `orders2`.

## Tokens

```
fl1.<base64url(claims JSON)>.<base64url(HMAC-SHA256)>
```

- Base64 is the **raw (unpadded) URL alphabet**, so tokens are safe in a
  query string — required because `EventSource` cannot set headers.
- The MAC covers the literal string `fl1.<claims-b64>` and is keyed by the
  secret named in `kid`. Verifiers must compare in constant time and
  reject any token whose decoded signature is not exactly 32 bytes.
- Claims (all other fields must be rejected as malformed):

| Field | Type | Meaning |
|---|---|---|
| `kid` | string | key ID; selects the verification secret |
| `ch` | string | channel pattern (see above) |
| `cap` | string[] | any of `sub`, `pub`, `stats` |
| `iat` | int | issue time, unix seconds; up to 30 s future skew tolerated |
| `exp` | int, optional | expiry, unix seconds, **exclusive**; absent = never |

Verification order: structure → claims decode + validation → key lookup →
signature → time window. Every failure is indistinguishable to the client
(HTTP 401), except capability/pattern mismatches which are 403.

## Event IDs, replay, and gaps

Every published event gets an ID `"<epoch>-<seq>"`:

- `seq` — per-channel counter starting at 1, never reused.
- `epoch` — random hex chosen when the hub creates the channel. A restart
  (or idle-sweep + recreate) changes the epoch, which is how a client's
  stale `Last-Event-ID` is detected instead of resuming the wrong history.

A subscriber opens `GET /v1/sse/{channel}` with either:

1. `Last-Event-ID: <epoch>-<seq>` (header, or `?last_event_id=`) — resume
   after `seq`;
2. `?replay=N` — best-effort last N retained events;
3. neither — live events only.

The server always answers with a `retry:` hint and then a `fanline.ready`
event before anything else:

```
retry: 3000

event: fanline.ready
data: {"channel":"orders.eu","epoch":"52e7ac0d","replayed":2,"gap":false}
```

`gap` is `true` when continuity to the client's `Last-Event-ID` cannot be
proven: the events were evicted (ring capacity or TTL) or the epoch
changed. On `gap`, a client holding derived state should refetch its
baseline; the replayed events that follow are everything the hub still
retains. Event names beginning `fanline.` are reserved for such protocol
frames; publishers cannot use them.

## Slow-consumer policy

Fanout never blocks: each subscriber has a bounded buffer (`--sub-buffer`,
default 64 events). A subscriber that stops reading is disconnected once
its buffer fills. This is safe *because* of replay — `EventSource`
reconnects automatically, presents `Last-Event-ID`, and catches up. The
alternative (back-pressuring the publisher) would let one stuck client
stall a channel for everyone.

## Publish

```
POST /v1/publish/{channel}
Authorization: Bearer <token with pub>
X-Fanline-Event: created        (optional; or ?event=; default event type)

<raw body = event data, up to --max-body bytes>
```

Response: `{"id":"52e7ac0d-3","seq":3,"channel":"orders.eu","subscribers":1}`.
`subscribers` counts live deliveries, excluding consumers dropped as slow
during this publish. Event names are `[A-Za-z0-9_.-]{1,64}` — newlines are
rejected outright, so a publisher can never forge SSE frames in
subscribers' streams.
