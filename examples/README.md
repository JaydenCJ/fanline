# fanline examples

Both examples run against a local hub. Start one first (dev mode skips
tokens so the examples work with zero setup; see the note below for the
authenticated variant):

```bash
go build -o fanline ./cmd/fanline
./fanline serve --dev --cors-origin '*' &
```

## dashboard.html — a live dashboard with no client library

```bash
bash examples/publish-loop.sh &          # feeds metrics.demo twice a second
open examples/dashboard.html             # or xdg-open / just open it in a browser
```

The page is a single static file: one `EventSource`, one event listener,
and a `fanline.ready` handler that shows the replay/gap status. Kill and
restart the hub while the page is open to watch the automatic reconnect
and the `gap` indicator fire.

## publish-loop.sh — a shell publisher

Publishes a small JSON payload to `metrics.demo` every 0.5 s using the
`fanline publish` CLI. Ctrl-C to stop.

## Using tokens instead of --dev

```bash
./fanline serve --keys main=demo-secret --cors-origin '*' &
TOKEN=$(./fanline token new --keys main=demo-secret --channel 'metrics.*' --cap sub,pub --ttl 24h)
FANLINE_TOKEN=$TOKEN bash examples/publish-loop.sh &
# then open dashboard.html and paste the token into the token field
```
