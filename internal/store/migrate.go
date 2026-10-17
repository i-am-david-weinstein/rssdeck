package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migration is one numbered SQL file. The number is the schema version the file
// produces, tracked in PRAGMA user_version.
type migration struct {
	version int
	name    string
	sql     string
}

// migrate brings the database up to the schema version this build expects,
// tracked in PRAGMA user_version.
func (s *Store) migrate(ctx context.Context) error {
	migrations, err := loadMigrations(migrationsFS, "migrations")
	if err != nil {
		return err
	}

	var current int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}

	// A database newer than this binary is a downgrade. Refuse it: the schema
	// may have dropped a column that a query in this build still names.
	if newest := migrations[len(migrations)-1].version; current > newest {
		return fmt.Errorf("store: database schema version %d is newer than this build supports (%d)", current, newest)
	}

	return s.applyMigrations(ctx, migrations, current)
}

// applyMigrations runs every migration above current in one transaction, so a
// failure part-way through leaves the schema untouched.
func (s *Store) applyMigrations(ctx context.Context, migrations []migration, current int) error {
	pending := make([]migration, 0, len(migrations))
	for _, m := range migrations {
		if m.version > current {
			pending = append(pending, m)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration: %w", err)
	}
	defer tx.Rollback()

	for _, m := range pending {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("store: apply migration %s: %w", m.name, err)
		}
	}

	// PRAGMA does not take bound parameters. The version comes from a filename
	// already parsed as an integer, so formatting it in is safe.
	target := pending[len(pending)-1].version
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", target)); err != nil {
		return fmt.Errorf("store: set user_version to %d: %w", target, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration: %w", err)
	}
	return nil
}

// loadMigrations reads and orders the numbered SQL files in dir. A filename that
// does not start with a number, or a duplicate or non-positive version, is a
// build mistake and fails loudly at startup.
func loadMigrations(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}

	var migrations []migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, err := versionFromName(e.Name())
		if err != nil {
			return nil, err
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("store: migrations %s and %s share version %d", other, e.Name(), version)
		}
		seen[version] = e.Name()

		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", e.Name(), err)
		}
		migrations = append(migrations, migration{version: version, name: e.Name(), sql: string(body)})
	}
	if len(migrations) == 0 {
		return nil, fmt.Errorf("store: no migrations found in %s", dir)
	}

	// Sort numerically. Lexical order would put 010 before 9.
	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})
	return migrations, nil
}

func versionFromName(name string) (int, error) {
	prefix, _, found := strings.Cut(name, "_")
	if !found {
		return 0, fmt.Errorf("store: migration %s is not named <version>_<description>.sql", name)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, fmt.Errorf("store: migration %s has a non-numeric version: %w", name, err)
	}
	if version <= 0 {
		return 0, fmt.Errorf("store: migration %s has version %d, which must be positive", name, version)
	}
	return version, nil
}
