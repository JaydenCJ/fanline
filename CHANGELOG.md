# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-07-13

### Added

- SSE pub/sub hub over plain HTTP: `GET /v1/sse/{channel}` streaming
  endpoint with `retry:` hints, comment keepalives, and
  `X-Accel-Buffering: no` for proxy friendliness; `POST /v1/publish/{channel}`;
  `GET /v1/stats`; `GET /v1/healthz`.
- Signed channel tokens (`fl1.<claims>.<HMAC-SHA256>`): channel patterns
  (`*` one segment, trailing `**` one or more), capabilities (`sub`,
  `pub`, `stats`), expiry with bounded clock-skew grace, key-ID based
  rotation, URL-safe encoding accepted as Bearer header or `?token=`.
- Per-channel replay ring (capacity + optional TTL) with
  `<epoch>-<seq>` event IDs; reconnect via `Last-Event-ID` replays
  exactly the missed events, `?replay=N` serves best-effort history, and
  a `fanline.ready` frame reports `replayed` and an honest `gap` flag on
  eviction or hub restart.
- Non-blocking fanout with bounded per-subscriber buffers: slow consumers
  are disconnected (and recover via replay) instead of stalling a channel;
  idle channels are swept automatically under a live-channel limit.
- CLI in one static binary: `serve`, `token new` / `token inspect`
  (offline, reproducible with `--now`), `publish`, `tail`
  (`--replay`, `--last-id`, `--max`, `--json`), `version`; configuration
  via flags or `FANLINE_ADDR` / `FANLINE_KEYS` / `FANLINE_URL` /
  `FANLINE_TOKEN`.
- Safety defaults: binds `127.0.0.1`, no outbound connections, no
  telemetry, publish body limits, SSE-injection-proof event-name
  validation, and `--dev` (auth off) refusing non-loopback binds.
- Runnable examples (`examples/dashboard.html`, `examples/publish-loop.sh`)
  and a wire/token format reference (`docs/protocol.md`).
- 91 deterministic offline tests (unit + httptest HTTP integration +
  in-process CLI) and `scripts/smoke.sh`.

[0.1.0]: https://github.com/JaydenCJ/fanline/releases/tag/v0.1.0
