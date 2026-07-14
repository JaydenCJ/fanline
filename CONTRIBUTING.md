# Contributing to fanline

Issues, discussions and pull requests are all welcome.

## Getting started

You need Go ≥1.22; nothing else — the project has zero runtime dependencies.

```bash
git clone https://github.com/JaydenCJ/fanline && cd fanline
go build ./...
go test ./...
bash scripts/smoke.sh
```

`scripts/smoke.sh` builds the binary, mints real tokens, runs a hub on a
loopback port, and drives publish/tail/replay/auth through the actual CLI;
it must finish by printing `SMOKE OK`.

## Before you open a pull request

1. `gofmt -l .` reports nothing (formatting is enforced).
2. `go vet ./...` passes with no findings.
3. `go test ./...` passes (91 deterministic tests, no network beyond
   httptest loopback).
4. `bash scripts/smoke.sh` prints `SMOKE OK`.
5. Add tests for behavior changes; keep logic in pure, unit-testable
   modules — `token`, `ring`, `sse`, `channel`, and `hub` never touch a
   socket, only `server` and `client` speak HTTP.

## Ground rules

- Keep dependencies at zero; adding one needs strong justification in
  the PR.
- No outbound network calls, ever; the hub binds `127.0.0.1` by default
  and `--dev` must keep refusing non-loopback binds. No telemetry.
- Anything a client can send (tokens, Last-Event-ID, channel names, event
  names) is hostile input: validate it, add the malicious-shape test, and
  never let it panic the hub or forge SSE frames.
- Determinism first: tests inject the clock and epoch generator; new tests
  must not sleep or depend on wall-clock timing.
- Wire compatibility matters: the token prefix (`fl1`), event-ID shape
  (`<epoch>-<seq>`), and `fanline.ready` payload are contracts — breaking
  them needs a version bump and a CHANGELOG entry.
- Code comments and doc comments are written in English.

## Reporting bugs

Include the output of `fanline version`, the full server command line
(redact secrets), the client command or `EventSource` snippet, and — for
replay/gap issues — the `fanline.ready` payload plus the `Last-Event-ID`
the client sent, since those two values fully determine replay behavior.

## Security

Please do not open public issues for security problems (especially token
verification bypasses); use GitHub's private vulnerability reporting on
this repository instead.
