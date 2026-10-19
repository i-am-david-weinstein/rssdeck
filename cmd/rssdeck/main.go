// Command rssdeck polls RSS/Atom feeds and sends new items to a Readeck
// instance exactly once.
//
// Three subcommands, dispatched by hand: serve (the default), healthcheck and
// version. Cobra would be the largest dependency in a project whose pitch is
// few dependencies, for three subcommands and one flag set.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// version is the build's version, set at link time:
//
//	go build -ldflags "-X main.version=v0.1.0"
//
// It reaches feed servers in the User-Agent, so they can see who is polling
// them, and Readeck in the same header.
var version = "dev"

func main() {
	// SIGTERM is what a container runtime sends, and SIGINT is Ctrl-C in a
	// terminal. Both mean the same thing here, and the second one kills the
	// process outright: an operator who asks twice is not asking politely.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)

	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	// Called rather than deferred: the failure path below ends in os.Exit,
	// which does not run defers, so a deferred stop would never release the
	// signal handler.
	stop()
	switch {
	case err == nil:
	case errors.Is(err, flag.ErrHelp):
		// The usage message has already been printed, and asking for it is not
		// a failure.
	default:
		fmt.Fprintln(os.Stderr, "rssdeck:", err)
		os.Exit(1)
	}
}

// run dispatches a subcommand. It takes its streams and arguments so the tests
// can drive it the way a shell does.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	command, rest := split(args)
	switch command {
	case "serve":
		return serve(ctx, rest, stdout, stderr)
	case "healthcheck":
		return healthcheck(ctx, rest, stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, "rssdeck", version)
		return nil
	case "help":
		// Help goes to stdout, because it was asked for rather than suffered,
		// and asking is not an error.
		if _, err := loadConfig([]string{"-h"}, stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
			return err
		}
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q; try serve, healthcheck, version or help", command)
	}
}

// split separates the subcommand from its flags. Serving is the default, so
// `rssdeck` and `rssdeck -log-level=debug` both serve: that is what the
// container's entrypoint runs, and making it type a subcommand would be
// ceremony.
func split(args []string) (command string, rest []string) {
	if len(args) == 0 {
		return "serve", nil
	}
	switch first := args[0]; {
	case first == "-h", first == "--help", first == "help":
		return "help", nil
	case len(first) > 0 && first[0] == '-':
		return "serve", args
	default:
		return first, args[1:]
	}
}
