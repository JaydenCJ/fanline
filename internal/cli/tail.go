package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/JaydenCJ/fanline/internal/client"
	"github.com/JaydenCJ/fanline/internal/sse"
)

func runTail(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := newFlagSet("fanline tail", stderr)
	var (
		baseURL = fs.String("url", envDefault(getenv, "FANLINE_URL", "http://127.0.0.1:8787"), "hub base URL (or FANLINE_URL)")
		ch      = fs.String("channel", "", "channel to stream")
		tok     = fs.String("token", getenv("FANLINE_TOKEN"), "subscribe token (or FANLINE_TOKEN); omit for --dev hubs")
		lastID  = fs.String("last-id", "", "resume after this event id (sent as Last-Event-ID)")
		replay  = fs.Int("replay", 0, "replay the last N retained events before going live")
		maxN    = fs.Int("max", 0, "exit after N data events (0 streams forever)")
		asJSON  = fs.Bool("json", false, "print events as JSON lines instead of id<TAB>event<TAB>data")
	)
	if code, done := parseFlags(fs, args); done {
		return code
	}
	if *ch == "" {
		fmt.Fprintln(stderr, "fanline tail: --channel is required")
		return ExitUsage
	}
	if *maxN < 0 || *replay < 0 {
		fmt.Fprintln(stderr, "fanline tail: --max and --replay must be non-negative")
		return ExitUsage
	}

	seen := 0
	opts := client.StreamOptions{
		BaseURL: *baseURL,
		Channel: *ch,
		Token:   *tok,
		LastID:  *lastID,
		Replay:  *replay,
		OnReady: func(r client.Ready) {
			// Connection metadata goes to stderr so stdout stays a clean
			// event stream for pipes.
			fmt.Fprintf(stderr, "# connected channel=%s epoch=%s replayed=%d gap=%v\n",
				r.Channel, r.Epoch, r.Replayed, r.Gap)
		},
	}
	err := client.Stream(context.Background(), opts, func(e sse.Event) error {
		printEvent(stdout, e, *asJSON)
		seen++
		if *maxN > 0 && seen >= *maxN {
			return client.Stop
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "fanline tail: %v\n", err)
		return ExitRuntime
	}
	if *maxN > 0 && seen < *maxN {
		fmt.Fprintf(stderr, "fanline tail: stream ended after %d of %d events\n", seen, *maxN)
		return ExitRuntime
	}
	return ExitOK
}

func printEvent(w io.Writer, e sse.Event, asJSON bool) {
	name := e.Name
	if name == "" {
		name = "message" // the SSE default event type
	}
	if asJSON {
		out, _ := json.Marshal(map[string]string{"id": e.ID, "event": name, "data": e.Data})
		fmt.Fprintln(w, string(out))
		return
	}
	fmt.Fprintf(w, "%s\t%s\t%s\n", e.ID, name, e.Data)
}
