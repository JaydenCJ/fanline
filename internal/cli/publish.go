package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/JaydenCJ/fanline/internal/client"
)

func runPublish(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := newFlagSet("fanline publish", stderr)
	var (
		baseURL = fs.String("url", envDefault(getenv, "FANLINE_URL", "http://127.0.0.1:8787"), "hub base URL (or FANLINE_URL)")
		ch      = fs.String("channel", "", "channel to publish to")
		tok     = fs.String("token", getenv("FANLINE_TOKEN"), "publish token (or FANLINE_TOKEN); omit for --dev hubs")
		event   = fs.String("event", "", "SSE event name (default: the SSE default, \"message\")")
		data    = fs.String("data", "", "event payload; omit to read it from stdin")
	)
	if code, done := parseFlags(fs, args); done {
		return code
	}
	if *ch == "" {
		fmt.Fprintln(stderr, "fanline publish: --channel is required")
		return ExitUsage
	}
	payload := *data
	if !flagWasSet(fs, "data") {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(stderr, "fanline publish: reading stdin: %v\n", err)
			return ExitRuntime
		}
		payload = string(raw)
	}
	res, err := client.Publish(context.Background(), nil, *baseURL, *ch, *tok, *event, payload)
	if err != nil {
		fmt.Fprintf(stderr, "fanline publish: %v\n", err)
		return ExitRuntime
	}
	out, _ := json.Marshal(res)
	fmt.Fprintln(stdout, string(out))
	return ExitOK
}

// flagWasSet reports whether the user passed the named flag explicitly,
// distinguishing `--data ""` (publish an empty payload) from no flag
// (read the payload from stdin).
func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}
