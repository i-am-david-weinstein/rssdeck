package main

import (
	"context"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
)

// isolateEnv removes every RSSDECK_ variable for the duration of a test, so the
// machine's own configuration cannot change what a test resolves.
//
// t.Setenv registers the restore; os.Unsetenv then does what t.Setenv cannot,
// which is make a variable absent rather than empty. The difference matters:
// RSSDECK_GLOBAL_LABEL="" is a real setting.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "RSSDECK_") {
			t.Setenv(name, "")
			if err := os.Unsetenv(name); err != nil {
				t.Fatalf("unset %s: %v", name, err)
			}
		}
	}
}

// freeAddr returns a loopback address nothing is listening on, for a test that
// needs to know the address before the daemon binds it.
func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return addr
}

func TestSplitChoosesTheSubcommand(t *testing.T) {
	tests := map[string]struct {
		args        []string
		wantCommand string
		wantRest    []string
	}{
		"nothing":            {nil, "serve", nil},
		"serve":              {[]string{"serve"}, "serve", []string{}},
		"serve with flags":   {[]string{"serve", "-addr", ":9000"}, "serve", []string{"-addr", ":9000"}},
		"bare flags serve":   {[]string{"-log-level=debug"}, "serve", []string{"-log-level=debug"}},
		"healthcheck":        {[]string{"healthcheck"}, "healthcheck", []string{}},
		"version":            {[]string{"version"}, "version", []string{}},
		"help word":          {[]string{"help"}, "help", nil},
		"help short flag":    {[]string{"-h"}, "help", nil},
		"help long flag":     {[]string{"--help"}, "help", nil},
		"unknown subcommand": {[]string{"poll"}, "poll", []string{}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			command, rest := split(tc.args)
			if command != tc.wantCommand {
				t.Errorf("command = %q, want %q", command, tc.wantCommand)
			}
			if !slices.Equal(rest, tc.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tc.wantRest)
			}
		})
	}
}

func TestRunVersion(t *testing.T) {
	isolateEnv(t)
	var out strings.Builder
	if err := run(context.Background(), []string{"version"}, &out, io.Discard); err != nil {
		t.Fatalf("run version: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "rssdeck "+version {
		t.Errorf("output = %q, want %q", got, "rssdeck "+version)
	}
}

// Asking for help is not a failure, so it prints to stdout and exits zero.
func TestRunHelp(t *testing.T) {
	isolateEnv(t)
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		var out strings.Builder
		if err := run(context.Background(), args, &out, io.Discard); err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
		for _, want := range []string{"Usage:", "healthcheck", "-poll-interval"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("help for %v does not mention %q:\n%s", args, want, out.String())
			}
		}
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	isolateEnv(t)
	err := run(context.Background(), []string{"poll"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("run accepted an unknown subcommand")
	}
	if !strings.Contains(err.Error(), "serve") {
		t.Errorf("error does not name the real subcommands: %v", err)
	}
}
