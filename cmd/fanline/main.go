// Command fanline is a zero-dependency SSE pub/sub hub with signed channel
// tokens and last-N message replay. See the README for the full manual.
package main

import (
	"os"

	"github.com/JaydenCJ/fanline/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}
