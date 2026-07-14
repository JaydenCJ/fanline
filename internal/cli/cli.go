// Package cli implements the fanline command-line interface. Run takes
// argv, two writers, and an environment lookup, and returns an exit code —
// so the whole surface is testable in-process without building a binary.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/JaydenCJ/fanline/internal/version"
)

// Exit codes, documented in the README.
const (
	ExitOK      = 0
	ExitRuntime = 1
	ExitUsage   = 2
)

const usageText = `fanline %s — SSE pub/sub hub with signed channel tokens and replay

Usage:
  fanline serve    [flags]              run the hub
  fanline token    new|inspect [flags]  mint or inspect signed channel tokens
  fanline publish  [flags]              publish one event to a running hub
  fanline tail     [flags]              stream a channel to stdout
  fanline version                       print the version

Run "fanline <command> -h" for command flags.
`

// Run dispatches argv and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if len(args) == 0 {
		fmt.Fprintf(stderr, usageText, version.Version)
		return ExitUsage
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:], stdout, stderr, getenv)
	case "token":
		return runToken(args[1:], stdout, stderr, getenv)
	case "publish":
		return runPublish(args[1:], stdout, stderr, getenv)
	case "tail":
		return runTail(args[1:], stdout, stderr, getenv)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "fanline %s\n", version.Version)
		return ExitOK
	case "help", "--help", "-h":
		fmt.Fprintf(stdout, usageText, version.Version)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "fanline: unknown command %q\n\n", args[0])
		fmt.Fprintf(stderr, usageText, version.Version)
		return ExitUsage
	}
}

// newFlagSet returns a FlagSet that reports errors to stderr without
// exiting the process.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseFlags runs fs.Parse and folds the outcome into an exit code.
// done is true when parsing already ended the command: -h/--help (usage
// was printed by the FlagSet) exits 0, anything else invalid exits 2.
func parseFlags(fs *flag.FlagSet, args []string) (code int, done bool) {
	switch err := fs.Parse(args); {
	case err == nil:
		return 0, false
	case errors.Is(err, flag.ErrHelp):
		return ExitOK, true
	default:
		return ExitUsage, true
	}
}

// envDefault returns the environment value for key, or fallback.
func envDefault(getenv func(string) string, key, fallback string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
}
