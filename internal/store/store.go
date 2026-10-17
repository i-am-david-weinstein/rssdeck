// Package store owns every read and write of rssdeck's SQLite database.
//
// The daemon runs one Store with a single connection (SetMaxOpenConns(1)), so
// writes never contend and SQLITE_BUSY cannot happen for this workload. Timestamps
// are stored as RFC3339 UTC text, which SQLite sorts and compares correctly and a
// human can read from the sqlite3 CLI.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Errors callers are expected to branch on.
var (
	ErrNotFound     = errors.New("not found")
	ErrDuplicateURL = errors.New("feed url already exists")
	ErrTokenExpired = errors.New("token expired")
)

// Store is a handle on the database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open prepares the database at path and brings it up to the current schema.
// The parent directory is created if missing. Pass ":memory:" for a throwaway
// database, which stays alive as long as the Store does.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("store: create %s: %w", dir, err)
			}
		}
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// One connection. An in-memory database also lives and dies with its
	// connection, so a pool would hand out empty databases.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: connect %s: %w", path, err)
	}
	if err := s.verifyPragmas(ctx, path); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// dsn builds the connection string. busy_timeout and foreign_keys are
// per-connection settings, so they belong in the DSN rather than in a statement
// run once after opening: the driver reapplies them if it ever reconnects.
func dsn(path string) string {
	pragmas := []string{
		"_pragma=journal_mode(WAL)",
		"_pragma=busy_timeout(5000)",
		"_pragma=synchronous(NORMAL)",
		"_pragma=foreign_keys(on)",
	}
	return "file:" + path + "?" + strings.Join(pragmas, "&")
}

// verifyPragmas fails the boot if the driver silently ignored a pragma, rather
// than letting a missing foreign_keys turn into orphaned items much later.
//
// An in-memory database cannot use WAL, so journal_mode is checked only for
// files.
func (s *Store) verifyPragmas(ctx context.Context, path string) error {
	want := map[string]string{
		"busy_timeout": "5000",
		"foreign_keys": "1",
		"synchronous":  "1", // NORMAL
	}
	if path != ":memory:" {
		want["journal_mode"] = "wal"
	}
	for pragma, expect := range want {
		var got string
		if err := s.db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
			return fmt.Errorf("store: read pragma %s: %w", pragma, err)
		}
		if !strings.EqualFold(got, expect) {
			return fmt.Errorf("store: pragma %s is %q, want %q", pragma, got, expect)
		}
	}
	return nil
}

// Close releases the connection. In-memory databases are discarded here.
func (s *Store) Close() error {
	return s.db.Close()
}

// nullTime maps a nullable RFC3339 text column to and from a time.Time. The
// driver hands back whatever SQLite stored, so parsing lives here instead of in
// every scan.
type nullTime struct {
	Time  time.Time
	Valid bool
}

func timePtr(t *time.Time) nullTime {
	if t == nil {
		return nullTime{}
	}
	return nullTime{Time: *t, Valid: true}
}

func (n *nullTime) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*n = nullTime{}
		return nil
	case time.Time:
		*n = nullTime{Time: v.UTC(), Valid: true}
		return nil
	case string:
		return n.parse(v)
	case []byte:
		return n.parse(string(v))
	default:
		return fmt.Errorf("store: cannot scan %T as timestamp", src)
	}
}

func (n *nullTime) parse(s string) error {
	if s == "" {
		*n = nullTime{}
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fmt.Errorf("store: parse timestamp %q: %w", s, err)
	}
	*n = nullTime{Time: t.UTC(), Valid: true}
	return nil
}

func (n nullTime) Value() (driver.Value, error) {
	if !n.Valid {
		return nil, nil
	}
	return formatTime(n.Time), nil
}

// ptr returns nil for a NULL column, so it can be assigned to an optional
// struct field directly.
func (n nullTime) ptr() *time.Time {
	if !n.Valid {
		return nil
	}
	t := n.Time
	return &t
}

// formatTime renders the one timestamp format the database uses.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// nullString writes "" as SQL NULL. Nothing in rssdeck distinguishes an empty
// title from a missing one, so collapsing the two keeps the Go structs free of
// pointers.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// text reads a nullable text column as "".
func text(ns sql.NullString) string {
	if !ns.Valid {
		return ""
	}
	return ns.String
}
