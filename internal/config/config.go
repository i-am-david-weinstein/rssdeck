// Package config resolves rssdeck's settings from flags and the environment.
//
// Precedence is flag, then environment variable, then default. There is no
// config file: there are a dozen settings and the feeds themselves live in the
// database, so a file format would be surface with nothing to hold.
//
// Resolution and validation are separate calls. The healthcheck subcommand
// needs the listen address and nothing else, and it would be absurd for a
// liveness probe to fail because the Readeck token is missing.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults for every setting that has one. The four required values are absent
// here on purpose: there is no sensible default for a token or for someone
// else's Readeck instance.
const (
	DefaultAddr            = "0.0.0.0:8080"
	DefaultDBPath          = "/data/rssdeck.db"
	DefaultPollInterval    = 15 * time.Minute
	DefaultBackfill        = 1
	DefaultGlobalLabel     = "rssdeck"
	DefaultMaxFailures     = 5
	DefaultHTTPTimeout     = 30 * time.Second
	DefaultMaxBodySize     = 10 << 20
	DefaultPollConcurrency = 4
	DefaultLogLevel        = "info"
	DefaultLogFormat       = "text"
)

// envPrefix is on every variable rssdeck reads, so an operator can see which
// settings belong to it with one `env | grep`.
const envPrefix = "RSSDECK_"

// Config is the resolved settings. Nothing here changes after startup.
type Config struct {
	// Addr is what the API listens on. Exposing it is the operator's job, via
	// port mapping or a reverse proxy; TLS is not terminated in-process.
	Addr string

	// DBPath is the SQLite file. Its parent directory is created at open time.
	DBPath string

	// APIToken is the master token for the API. The daemon refuses to serve
	// without it.
	APIToken string

	ReadeckURL   string
	ReadeckToken string

	// PollInterval is the global poll interval, used by every feed with no
	// interval of its own.
	PollInterval time.Duration

	// Backfill is how many of a feed's newest items an add sends when the
	// request does not say. Zero seeds the whole document and sends nothing.
	Backfill int

	// GlobalLabel is added to every bookmark. Empty disables it.
	GlobalLabel string

	// MaxFailures is how many failed sends an item takes before it is given up
	// on as failed_permanent.
	MaxFailures int

	// HTTPTimeout bounds each feed fetch and each Readeck request.
	HTTPTimeout time.Duration

	// MaxBodySize caps a feed response. Exceeding it fails the poll rather than
	// filling memory.
	MaxBodySize int64

	// PollConcurrency bounds how many feeds are polled at once.
	PollConcurrency int

	LogLevel  slog.Level
	LogFormat string
}

// Load resolves settings for a subcommand from its arguments and the
// environment. args is the arguments after the subcommand name, as in
// os.Args[2:].
//
// A value that is present but unparseable is an error even when a flag
// overrides it: a typo in RSSDECK_POLL_INTERVAL that the daemon quietly ignores
// is worse than a boot that says so. Every such problem is reported at once,
// so an operator fixing three of them needs one restart rather than three.
//
// output is where flag errors and the usage message go.
func Load(args []string, lookupEnv func(string) (string, bool), output io.Writer) (*Config, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	env := &resolver{lookup: lookupEnv}

	// Flag defaults are the environment's answers, which is what makes
	// precedence work: a flag that is not passed keeps the value the
	// environment resolved, and one that is passed overrides it.
	cfg := &Config{
		Addr:            env.text("ADDR", DefaultAddr),
		DBPath:          env.text("DB_PATH", DefaultDBPath),
		APIToken:        env.text("API_TOKEN", ""),
		ReadeckURL:      env.text("READECK_URL", ""),
		ReadeckToken:    env.text("READECK_TOKEN", ""),
		PollInterval:    env.duration("POLL_INTERVAL", DefaultPollInterval),
		Backfill:        env.integer("DEFAULT_BACKFILL", DefaultBackfill),
		GlobalLabel:     env.text("GLOBAL_LABEL", DefaultGlobalLabel),
		MaxFailures:     env.integer("MAX_FAILURES", DefaultMaxFailures),
		HTTPTimeout:     env.duration("HTTP_TIMEOUT", DefaultHTTPTimeout),
		MaxBodySize:     env.size("MAX_BODY_SIZE", DefaultMaxBodySize),
		PollConcurrency: env.integer("POLL_CONCURRENCY", DefaultPollConcurrency),
		LogFormat:       env.text("LOG_FORMAT", DefaultLogFormat),
	}
	logLevel := env.text("LOG_LEVEL", DefaultLogLevel)
	apiTokenFile := env.text("API_TOKEN_FILE", "")
	readeckTokenFile := env.text("READECK_TOKEN_FILE", "")

	fs := flag.NewFlagSet("rssdeck", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() { usage(fs, output) }

	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "address the API listens on")
	fs.StringVar(&cfg.DBPath, "db-path", cfg.DBPath, "path to the SQLite database; its directory is created if missing")
	fs.StringVar(&cfg.APIToken, "api-token", cfg.APIToken,
		"master token for the API (prefer the environment or -api-token-file: a flag is visible in ps)")
	fs.StringVar(&apiTokenFile, "api-token-file", apiTokenFile, "file holding the master API token; wins over -api-token")
	fs.StringVar(&cfg.ReadeckURL, "readeck-url", cfg.ReadeckURL, "Readeck instance root, for example https://readeck.example")
	fs.StringVar(&cfg.ReadeckToken, "readeck-token", cfg.ReadeckToken,
		"Readeck API token (prefer the environment or -readeck-token-file: a flag is visible in ps)")
	fs.StringVar(&readeckTokenFile, "readeck-token-file", readeckTokenFile, "file holding the Readeck token; wins over -readeck-token")
	fs.DurationVar(&cfg.PollInterval, "poll-interval", cfg.PollInterval, "global poll interval, overridable per feed")
	fs.IntVar(&cfg.Backfill, "default-backfill", cfg.Backfill, "items sent when a feed is added, unless the request says otherwise")
	fs.StringVar(&cfg.GlobalLabel, "global-label", cfg.GlobalLabel, "label added to every bookmark; empty disables it")
	fs.IntVar(&cfg.MaxFailures, "max-failures", cfg.MaxFailures, "failed sends an item takes before it is given up on")
	fs.DurationVar(&cfg.HTTPTimeout, "http-timeout", cfg.HTTPTimeout, "timeout for feed fetches and Readeck requests")
	sizeFlag := &sizeValue{target: &cfg.MaxBodySize}
	fs.Var(sizeFlag, "max-body-size", "cap on a feed response, in bytes or with a suffix such as 10MB")
	fs.IntVar(&cfg.PollConcurrency, "poll-concurrency", cfg.PollConcurrency, "feeds polled at once")
	fs.StringVar(&logLevel, "log-level", logLevel, "debug, info, warn or error")
	fs.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "text or json")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if extra := fs.Args(); len(extra) > 0 {
		return nil, fmt.Errorf("config: unexpected argument %q", extra[0])
	}

	// A token file is read after parsing, because the flag may name a different
	// file than the environment did. A file wins over an inline token either
	// way: it is the form a container secret takes, and the one an operator
	// reaches for when they care.
	if apiTokenFile != "" {
		cfg.APIToken = env.file("API_TOKEN_FILE", apiTokenFile)
	}
	if readeckTokenFile != "" {
		cfg.ReadeckToken = env.file("READECK_TOKEN_FILE", readeckTokenFile)
	}

	level, err := parseLevel(logLevel)
	if err != nil {
		env.errs = append(env.errs, err)
	}
	cfg.LogLevel = level
	if cfg.LogFormat != "text" && cfg.LogFormat != "json" {
		env.errs = append(env.errs, fmt.Errorf("config: log format %q is not text or json", cfg.LogFormat))
	}

	if err := errors.Join(env.errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// usage prints the flags with a reminder of where else settings come from. Every
// flag has an environment variable, and listing both forms twice would double
// the help text for no gain.
func usage(fs *flag.FlagSet, output io.Writer) {
	fmt.Fprint(output, `rssdeck polls RSS/Atom feeds and sends new items to Readeck exactly once.

Usage:
  rssdeck [serve] [flags]     poll feeds and serve the API (the default)
  rssdeck healthcheck [flags] exit 0 if the local instance answers /healthz
  rssdeck version             print the version

Every flag has an environment variable: -poll-interval is RSSDECK_POLL_INTERVAL.
Precedence is flag, then environment, then default. Tokens are better supplied
as RSSDECK_API_TOKEN / RSSDECK_READECK_TOKEN or their _FILE forms, since a flag
is visible to anyone who can run ps.

Flags:
`)
	fs.PrintDefaults()
}

// Validate reports every missing or nonsensical setting at once, so an operator
// with three mistakes needs one more boot rather than three.
//
// Only serve calls this. The required four have no defaults, and refusing to
// start without them is deliberate: a daemon serving an unauthenticated API, or
// one that cannot reach Readeck, has nothing useful to do.
func (c *Config) Validate() error {
	var errs []error
	if c.Addr == "" {
		errs = append(errs, errors.New("config: no listen address; set RSSDECK_ADDR or -addr"))
	}
	if c.DBPath == "" {
		errs = append(errs, errors.New("config: no database path; set RSSDECK_DB_PATH or -db-path"))
	}
	if c.APIToken == "" {
		// No loopback exception: that assumption breaks the moment someone
		// runs the container with host networking.
		errs = append(errs, errors.New("config: no API token; set RSSDECK_API_TOKEN or RSSDECK_API_TOKEN_FILE"))
	}
	if c.ReadeckToken == "" {
		errs = append(errs, errors.New("config: no Readeck token; set RSSDECK_READECK_TOKEN or RSSDECK_READECK_TOKEN_FILE"))
	}
	switch {
	case c.ReadeckURL == "":
		errs = append(errs, errors.New("config: no Readeck URL; set RSSDECK_READECK_URL or -readeck-url"))
	default:
		if err := validateReadeckURL(c.ReadeckURL); err != nil {
			errs = append(errs, err)
		}
	}
	if c.PollInterval <= 0 {
		errs = append(errs, fmt.Errorf("config: poll interval %s must be positive", c.PollInterval))
	}
	if c.Backfill < 0 {
		errs = append(errs, fmt.Errorf("config: backfill %d cannot be negative", c.Backfill))
	}
	if c.MaxFailures < 1 {
		errs = append(errs, fmt.Errorf("config: max failures %d must be at least 1", c.MaxFailures))
	}
	if c.HTTPTimeout <= 0 {
		errs = append(errs, fmt.Errorf("config: http timeout %s must be positive", c.HTTPTimeout))
	}
	if c.MaxBodySize < 1 {
		errs = append(errs, fmt.Errorf("config: max body size %d must be positive", c.MaxBodySize))
	}
	if c.PollConcurrency < 1 {
		errs = append(errs, fmt.Errorf("config: poll concurrency %d must be at least 1", c.PollConcurrency))
	}
	return errors.Join(errs...)
}

// validateReadeckURL fails a URL the Readeck client would reject anyway, so the
// complaint arrives with the other configuration problems rather than on its
// own a moment later.
func validateReadeckURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("config: readeck url %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("config: readeck url %q must be http or https", raw)
	}
	if parsed.Host == "" {
		return fmt.Errorf("config: readeck url %q has no host", raw)
	}
	return nil
}

// Logger builds the logger the daemon runs with. Text by default, because a
// human is usually reading docker logs.
func (c *Config) Logger(w io.Writer) *slog.Logger {
	options := &slog.HandlerOptions{Level: c.LogLevel}
	if c.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, options))
	}
	return slog.New(slog.NewTextHandler(w, options))
}

// LogAttrs is the startup summary: every setting, with the tokens reduced to
// whether they are set.
//
// Tokens are never written to the database in plaintext, never logged and
// redacted here. Reporting whether one is present is the part an operator
// debugging a 401 actually needs.
func (c *Config) LogAttrs() []any {
	return []any{
		"addr", c.Addr,
		"db_path", c.DBPath,
		"api_token", redacted(c.APIToken),
		"readeck_url", c.ReadeckURL,
		"readeck_token", redacted(c.ReadeckToken),
		"poll_interval", c.PollInterval,
		"default_backfill", c.Backfill,
		"global_label", c.GlobalLabel,
		"max_failures", c.MaxFailures,
		"http_timeout", c.HTTPTimeout,
		"max_body_size", c.MaxBodySize,
		"poll_concurrency", c.PollConcurrency,
		"log_level", c.LogLevel.String(),
		"log_format", c.LogFormat,
	}
}

// redacted describes a secret without disclosing it, not even its length.
func redacted(secret string) string {
	if secret == "" {
		return "unset"
	}
	return "set"
}

// ImpoliteInterval is the interval below which serve warns rather than refuses.
// It is not a rejection: a local feed an operator owns is theirs to poll as
// often as they like, and a daemon that will not start is no help to them. A
// line in the log is enough for the case where it was a units mistake.
const ImpoliteInterval = time.Minute

// Impolite reports whether the global interval is fast enough to be worth a
// word at startup.
func (c *Config) Impolite() bool {
	return c.PollInterval > 0 && c.PollInterval < ImpoliteInterval
}

// resolver reads environment variables and collects what it could not parse,
// so one boot reports every mistake.
type resolver struct {
	lookup func(string) (string, bool)
	errs   []error
}

// text reads a string, honouring a variable that is set to the empty string:
// RSSDECK_GLOBAL_LABEL="" is how the global label is disabled, and treating it
// as unset would silently re-enable it.
func (r *resolver) text(name, def string) string {
	if value, ok := r.lookup(envPrefix + name); ok {
		return value
	}
	return def
}

func (r *resolver) duration(name string, def time.Duration) time.Duration {
	raw, ok := r.lookup(envPrefix + name)
	if !ok || raw == "" {
		return def
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("config: %s%s=%q is not a duration such as 15m or 30s", envPrefix, name, raw))
		return def
	}
	return value
}

func (r *resolver) integer(name string, def int) int {
	raw, ok := r.lookup(envPrefix + name)
	if !ok || raw == "" {
		return def
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("config: %s%s=%q is not a number", envPrefix, name, raw))
		return def
	}
	return value
}

func (r *resolver) size(name string, def int64) int64 {
	raw, ok := r.lookup(envPrefix + name)
	if !ok || raw == "" {
		return def
	}
	value, err := ParseSize(raw)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("config: %s%s: %w", envPrefix, name, err))
		return def
	}
	return value
}

// file reads a secret from a file, which is the form a Docker or Kubernetes
// secret arrives in. Trailing whitespace is dropped: a file written with echo
// or a heredoc ends in a newline, and a token with a newline on the end fails
// authentication in a way that is miserable to diagnose.
func (r *resolver) file(name, path string) string {
	contents, err := os.ReadFile(path)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("config: %s%s: %w", envPrefix, name, err))
		return ""
	}
	secret := strings.TrimSpace(string(contents))
	if secret == "" {
		r.errs = append(r.errs, fmt.Errorf("config: %s is empty", path))
	}
	return secret
}

func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("config: log level %q is not debug, info, warn or error", raw)
	}
}

// sizeValue lets -max-body-size take the same suffixed form the environment
// variable does, so the two are never written differently.
type sizeValue struct {
	target *int64
}

func (s *sizeValue) String() string {
	if s.target == nil {
		return ""
	}
	return strconv.FormatInt(*s.target, 10)
}

func (s *sizeValue) Set(raw string) error {
	value, err := ParseSize(raw)
	if err != nil {
		return err
	}
	*s.target = value
	return nil
}

// ParseSize reads a byte count, bare or with a suffix: 10MB, 512K, 1GiB,
// 10485760.
//
// Suffixes are powers of 1024, including the decimal-looking ones. An operator
// writing 10MB means the same thing the design's table means, and the 4.8%
// difference between 10MB and 10MiB matters to nobody choosing a cap on a feed
// response.
func ParseSize(raw string) (int64, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return 0, errors.New("empty size")
	}

	digits := strings.TrimRight(text, "bBiIkKmMgG ")
	suffix := strings.ToLower(strings.TrimSpace(text[len(digits):]))

	value, err := strconv.ParseFloat(strings.TrimSpace(digits), 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size such as 10MB or 10485760", raw)
	}

	var unit float64 = 1
	switch suffix {
	case "", "b":
		unit = 1
	case "k", "kb", "kib":
		unit = 1 << 10
	case "m", "mb", "mib":
		unit = 1 << 20
	case "g", "gb", "gib":
		unit = 1 << 30
	default:
		return 0, fmt.Errorf("%q has an unknown size suffix %q", raw, suffix)
	}

	bytes := value * unit
	if bytes < 0 {
		return 0, fmt.Errorf("size %q cannot be negative", raw)
	}
	return int64(bytes), nil
}
