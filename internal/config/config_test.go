package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// env builds a lookup over a fixed map, so a test's environment is only what it
// says and never the machine's.
func env(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := vars[name]
		return value, ok
	}
}

// load resolves a config and fails the test if it could not.
func load(t *testing.T, args []string, vars map[string]string) *Config {
	t.Helper()
	cfg, err := Load(args, env(vars), io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestDefaults(t *testing.T) {
	cfg := load(t, nil, nil)

	switch {
	case cfg.Addr != DefaultAddr:
		t.Errorf("addr = %q, want %q", cfg.Addr, DefaultAddr)
	case cfg.DBPath != DefaultDBPath:
		t.Errorf("db path = %q, want %q", cfg.DBPath, DefaultDBPath)
	case cfg.PollInterval != DefaultPollInterval:
		t.Errorf("poll interval = %s, want %s", cfg.PollInterval, DefaultPollInterval)
	case cfg.Backfill != DefaultBackfill:
		t.Errorf("backfill = %d, want %d", cfg.Backfill, DefaultBackfill)
	case cfg.GlobalLabel != DefaultGlobalLabel:
		t.Errorf("global label = %q, want %q", cfg.GlobalLabel, DefaultGlobalLabel)
	case cfg.MaxFailures != DefaultMaxFailures:
		t.Errorf("max failures = %d, want %d", cfg.MaxFailures, DefaultMaxFailures)
	case cfg.HTTPTimeout != DefaultHTTPTimeout:
		t.Errorf("http timeout = %s, want %s", cfg.HTTPTimeout, DefaultHTTPTimeout)
	case cfg.MaxBodySize != DefaultMaxBodySize:
		t.Errorf("max body size = %d, want %d", cfg.MaxBodySize, DefaultMaxBodySize)
	case cfg.PollConcurrency != DefaultPollConcurrency:
		t.Errorf("poll concurrency = %d, want %d", cfg.PollConcurrency, DefaultPollConcurrency)
	case cfg.LogLevel != slog.LevelInfo:
		t.Errorf("log level = %s, want info", cfg.LogLevel)
	case cfg.LogFormat != DefaultLogFormat:
		t.Errorf("log format = %q, want %q", cfg.LogFormat, DefaultLogFormat)
	}

	// The four required values have no defaults, which is what makes the daemon
	// refuse to start without them.
	if cfg.APIToken != "" || cfg.ReadeckURL != "" || cfg.ReadeckToken != "" {
		t.Errorf("a required value was defaulted: %+v", cfg)
	}
}

func TestEnvironmentOverridesDefaults(t *testing.T) {
	cfg := load(t, nil, map[string]string{
		"RSSDECK_ADDR":             "127.0.0.1:9000",
		"RSSDECK_DB_PATH":          "/var/lib/rssdeck.db",
		"RSSDECK_API_TOKEN":        "from-env",
		"RSSDECK_READECK_URL":      "https://readeck.example",
		"RSSDECK_READECK_TOKEN":    "readeck-from-env",
		"RSSDECK_POLL_INTERVAL":    "5m",
		"RSSDECK_DEFAULT_BACKFILL": "3",
		"RSSDECK_GLOBAL_LABEL":     "feeds",
		"RSSDECK_MAX_FAILURES":     "9",
		"RSSDECK_HTTP_TIMEOUT":     "45s",
		"RSSDECK_MAX_BODY_SIZE":    "2MB",
		"RSSDECK_POLL_CONCURRENCY": "8",
		"RSSDECK_LOG_LEVEL":        "debug",
		"RSSDECK_LOG_FORMAT":       "json",
	})

	switch {
	case cfg.Addr != "127.0.0.1:9000":
		t.Errorf("addr = %q", cfg.Addr)
	case cfg.DBPath != "/var/lib/rssdeck.db":
		t.Errorf("db path = %q", cfg.DBPath)
	case cfg.APIToken != "from-env":
		t.Errorf("api token = %q", cfg.APIToken)
	case cfg.ReadeckURL != "https://readeck.example":
		t.Errorf("readeck url = %q", cfg.ReadeckURL)
	case cfg.ReadeckToken != "readeck-from-env":
		t.Errorf("readeck token = %q", cfg.ReadeckToken)
	case cfg.PollInterval != 5*time.Minute:
		t.Errorf("poll interval = %s", cfg.PollInterval)
	case cfg.Backfill != 3:
		t.Errorf("backfill = %d", cfg.Backfill)
	case cfg.GlobalLabel != "feeds":
		t.Errorf("global label = %q", cfg.GlobalLabel)
	case cfg.MaxFailures != 9:
		t.Errorf("max failures = %d", cfg.MaxFailures)
	case cfg.HTTPTimeout != 45*time.Second:
		t.Errorf("http timeout = %s", cfg.HTTPTimeout)
	case cfg.MaxBodySize != 2<<20:
		t.Errorf("max body size = %d", cfg.MaxBodySize)
	case cfg.PollConcurrency != 8:
		t.Errorf("poll concurrency = %d", cfg.PollConcurrency)
	case cfg.LogLevel != slog.LevelDebug:
		t.Errorf("log level = %s", cfg.LogLevel)
	case cfg.LogFormat != "json":
		t.Errorf("log format = %q", cfg.LogFormat)
	}
}

func TestFlagsOverrideEnvironment(t *testing.T) {
	vars := map[string]string{
		"RSSDECK_ADDR":          "127.0.0.1:9000",
		"RSSDECK_POLL_INTERVAL": "5m",
		"RSSDECK_MAX_BODY_SIZE": "2MB",
		"RSSDECK_LOG_LEVEL":     "debug",
		"RSSDECK_GLOBAL_LABEL":  "from-env",
	}
	cfg := load(t, []string{
		"-addr", "0.0.0.0:7000",
		"-poll-interval", "90s",
		"-max-body-size", "512K",
		"-log-level", "warn",
		"-global-label", "from-flag",
	}, vars)

	switch {
	case cfg.Addr != "0.0.0.0:7000":
		t.Errorf("addr = %q", cfg.Addr)
	case cfg.PollInterval != 90*time.Second:
		t.Errorf("poll interval = %s", cfg.PollInterval)
	case cfg.MaxBodySize != 512<<10:
		t.Errorf("max body size = %d", cfg.MaxBodySize)
	case cfg.LogLevel != slog.LevelWarn:
		t.Errorf("log level = %s", cfg.LogLevel)
	case cfg.GlobalLabel != "from-flag":
		t.Errorf("global label = %q", cfg.GlobalLabel)
	}
}

// An empty global label is the documented way to disable it, so a variable set
// to the empty string must not read as unset.
func TestGlobalLabelCanBeDisabled(t *testing.T) {
	if cfg := load(t, nil, map[string]string{"RSSDECK_GLOBAL_LABEL": ""}); cfg.GlobalLabel != "" {
		t.Errorf("global label = %q from an empty environment variable, want it disabled", cfg.GlobalLabel)
	}
	if cfg := load(t, []string{"-global-label", ""}, nil); cfg.GlobalLabel != "" {
		t.Errorf("global label = %q from an empty flag, want it disabled", cfg.GlobalLabel)
	}
}

func TestTokenFilesWinOverInlineTokens(t *testing.T) {
	dir := t.TempDir()
	apiFile := filepath.Join(dir, "api-token")
	readeckFile := filepath.Join(dir, "readeck-token")
	// Trailing newline on purpose: that is what echo, a heredoc and most secret
	// mounts produce, and a token with a newline on it fails authentication in
	// a way that is miserable to diagnose.
	if err := os.WriteFile(apiFile, []byte("api-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readeckFile, []byte("  readeck-from-file\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := load(t, nil, map[string]string{
		"RSSDECK_API_TOKEN":          "api-inline",
		"RSSDECK_API_TOKEN_FILE":     apiFile,
		"RSSDECK_READECK_TOKEN":      "readeck-inline",
		"RSSDECK_READECK_TOKEN_FILE": readeckFile,
	})
	if cfg.APIToken != "api-from-file" {
		t.Errorf("api token = %q, want the file's contents trimmed", cfg.APIToken)
	}
	if cfg.ReadeckToken != "readeck-from-file" {
		t.Errorf("readeck token = %q, want the file's contents trimmed", cfg.ReadeckToken)
	}

	// A flag naming a different file beats the environment's file.
	other := filepath.Join(dir, "other-token")
	if err := os.WriteFile(other, []byte("from-flag-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = load(t, []string{"-api-token-file", other}, map[string]string{
		"RSSDECK_API_TOKEN_FILE": apiFile,
	})
	if cfg.APIToken != "from-flag-file" {
		t.Errorf("api token = %q, want the file the flag named", cfg.APIToken)
	}
}

func TestUnreadableAndEmptyTokenFilesFail(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n  \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := map[string]string{
		"missing file": filepath.Join(dir, "nope"),
		"empty file":   empty,
	}
	for name, path := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(nil, env(map[string]string{"RSSDECK_API_TOKEN_FILE": path}), io.Discard)
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
		})
	}
}

// A typo in one variable should not need a restart to find the typo in the
// next one, and a flag overriding a malformed variable does not make it fine:
// the operator meant something by it.
func TestEveryMalformedValueIsReportedAtOnce(t *testing.T) {
	_, err := Load(nil, env(map[string]string{
		"RSSDECK_POLL_INTERVAL": "fifteen minutes",
		"RSSDECK_MAX_FAILURES":  "lots",
		"RSSDECK_MAX_BODY_SIZE": "10 bananas",
		"RSSDECK_LOG_LEVEL":     "chatty",
		"RSSDECK_LOG_FORMAT":    "yaml",
	}), io.Discard)
	if err == nil {
		t.Fatal("Load succeeded with five malformed values")
	}

	message := err.Error()
	for _, want := range []string{"POLL_INTERVAL", "MAX_FAILURES", "MAX_BODY_SIZE", "chatty", "yaml"} {
		if !strings.Contains(message, want) {
			t.Errorf("error does not mention %s:\n%s", want, message)
		}
	}
}

func TestUnexpectedArgumentIsRejected(t *testing.T) {
	if _, err := Load([]string{"serve"}, env(nil), io.Discard); err == nil {
		t.Fatal("Load accepted a stray argument")
	}
}

func TestHelpPrintsUsageAndReportsItself(t *testing.T) {
	var out strings.Builder
	_, err := Load([]string{"-h"}, env(nil), &out)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
	for _, want := range []string{"Usage:", "healthcheck", "RSSDECK_POLL_INTERVAL", "-poll-interval"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage does not mention %q:\n%s", want, out.String())
		}
	}
}

func TestValidateReportsEveryMissingValueAtOnce(t *testing.T) {
	cfg := load(t, nil, nil)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate passed with no token and no Readeck instance")
	}
	for _, want := range []string{"API_TOKEN", "READECK_URL", "READECK_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%s", want, err)
		}
	}
}

func TestValidateAcceptsACompleteConfig(t *testing.T) {
	cfg := load(t, nil, map[string]string{
		"RSSDECK_API_TOKEN":     "token",
		"RSSDECK_READECK_URL":   "https://readeck.example",
		"RSSDECK_READECK_TOKEN": "readeck-token",
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejectsNonsense(t *testing.T) {
	complete := map[string]string{
		"RSSDECK_API_TOKEN":     "token",
		"RSSDECK_READECK_URL":   "https://readeck.example",
		"RSSDECK_READECK_TOKEN": "readeck-token",
	}
	tests := map[string][]string{
		"readeck url with no scheme": {"-readeck-url", "readeck.example"},
		"readeck url with no host":   {"-readeck-url", "https://"},
		"readeck url not http":       {"-readeck-url", "ftp://readeck.example"},
		"zero poll interval":         {"-poll-interval", "0s"},
		"negative backfill":          {"-default-backfill", "-1"},
		"zero max failures":          {"-max-failures", "0"},
		"zero http timeout":          {"-http-timeout", "0s"},
		"zero concurrency":           {"-poll-concurrency", "0"},
		"empty addr":                 {"-addr", ""},
		"empty db path":              {"-db-path", ""},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			if err := load(t, args, complete).Validate(); err == nil {
				t.Fatal("Validate passed")
			}
		})
	}
}

// A fast interval is the operator's business. It earns a word at startup, not a
// refusal to start.
func TestImpoliteIntervalIsAWarningNotAnError(t *testing.T) {
	complete := map[string]string{
		"RSSDECK_API_TOKEN":     "token",
		"RSSDECK_READECK_URL":   "https://readeck.example",
		"RSSDECK_READECK_TOKEN": "readeck-token",
	}
	cfg := load(t, []string{"-poll-interval", "10s"}, complete)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected a fast interval: %v", err)
	}
	if !cfg.Impolite() {
		t.Error("a 10s interval is not reported as impolite")
	}
	if load(t, nil, complete).Impolite() {
		t.Error("the default interval is reported as impolite")
	}
}

func TestLogAttrsRedactTokens(t *testing.T) {
	cfg := load(t, nil, map[string]string{
		"RSSDECK_API_TOKEN":     "super-secret-api-token",
		"RSSDECK_READECK_TOKEN": "super-secret-readeck-token",
	})
	summary := fmt.Sprint(cfg.LogAttrs()...)

	for _, secret := range []string{"super-secret-api-token", "super-secret-readeck-token"} {
		if strings.Contains(summary, secret) {
			t.Fatalf("the startup summary leaked a token:\n%s", summary)
		}
	}
	if !strings.Contains(summary, "set") {
		t.Errorf("the summary does not say whether a token is set:\n%s", summary)
	}

	// An unset token reads as unset rather than as an empty value, which is
	// what an operator debugging a 401 needs to see.
	if unset := fmt.Sprint(load(t, nil, nil).LogAttrs()...); !strings.Contains(unset, "unset") {
		t.Errorf("an unset token is not reported as unset:\n%s", unset)
	}

	// Every setting is in the summary, so one line answers "what is this
	// daemon actually running with".
	for _, want := range []string{"addr", "db_path", "readeck_url", "poll_interval", "log_format"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary omits %s:\n%s", want, summary)
		}
	}
}

func TestLoggerHonoursFormatAndLevel(t *testing.T) {
	tests := map[string]struct {
		format string
		want   string
	}{
		"text": {"text", "level=INFO"},
		"json": {"json", `"level":"INFO"`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := load(t, []string{"-log-format", tc.format}, nil)
			var out strings.Builder
			cfg.Logger(&out).Info("hello")
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("%s output = %q, want it to contain %q", name, out.String(), tc.want)
			}
		})
	}

	// A level above debug drops debug lines, which is what keeps twenty feeds
	// to a couple of thousand log lines a day.
	cfg := load(t, []string{"-log-level", "warn"}, nil)
	var out strings.Builder
	log := cfg.Logger(&out)
	log.Info("dropped")
	log.Warn("kept")
	if strings.Contains(out.String(), "dropped") || !strings.Contains(out.String(), "kept") {
		t.Errorf("log level was not applied: %q", out.String())
	}
}

func TestParseSize(t *testing.T) {
	tests := map[string]struct {
		in      string
		want    int64
		wantErr bool
	}{
		"bare bytes":   {"10485760", 10 << 20, false},
		"bytes suffix": {"512B", 512, false},
		"kilobytes":    {"512K", 512 << 10, false},
		"kb":           {"512kb", 512 << 10, false},
		"kib":          {"512KiB", 512 << 10, false},
		"megabytes":    {"10MB", 10 << 20, false},
		"gigabytes":    {"1GB", 1 << 30, false},
		"fractional":   {"1.5MB", 1572864, false},
		"spaced":       {" 10 MB ", 10 << 20, false},
		"empty":        {"", 0, true},
		"unknown unit": {"10 bananas", 0, true},
		"not a number": {"plenty", 0, true},
		"negative":     {"-1MB", 0, true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseSize(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSize(%q) = %d, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSize(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
