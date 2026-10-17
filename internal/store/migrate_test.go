package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestEmbeddedMigrationsLoad(t *testing.T) {
	migrations, err := loadMigrations(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("no migrations embedded")
	}
	if migrations[0].version != 1 {
		t.Errorf("first migration is version %d, want 1", migrations[0].version)
	}
	for i := 1; i < len(migrations); i++ {
		if migrations[i-1].version >= migrations[i].version {
			t.Errorf("migrations out of order at %d: %d then %d",
				i, migrations[i-1].version, migrations[i].version)
		}
	}
}

func TestMigrateSetsUserVersion(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	want, err := loadMigrations(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	var got int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&got); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if got != want[len(want)-1].version {
		t.Errorf("user_version = %d, want %d", got, want[len(want)-1].version)
	}

	// Migrating again applies nothing and must not fail on existing tables.
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rssdeck.db")

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 9999"); err != nil {
		t.Fatalf("bump user_version: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err = Open(ctx, path)
	if err == nil {
		t.Fatal("Open on a newer schema succeeded, want a downgrade refusal")
	}
	if !strings.Contains(err.Error(), "newer than this build") {
		t.Errorf("error = %v, want a downgrade refusal", err)
	}
}

func TestMigrateRollsBackOnFailure(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	migrations := []migration{
		{version: 100, name: "100_ok.sql", sql: `CREATE TABLE should_not_survive (id INTEGER)`},
		{version: 101, name: "101_bad.sql", sql: `CREATE TABLE feeds (id INTEGER)`}, // feeds already exists
	}
	if err := s.applyMigrations(ctx, migrations, 99); err == nil {
		t.Fatal("applyMigrations succeeded on a broken migration, want error")
	}

	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE name = 'should_not_survive'`).Scan(&count)
	if err != nil {
		t.Fatalf("inspect sqlite_master: %v", err)
	}
	if count != 0 {
		t.Error("the first migration's table survived a failure in the second")
	}
}

func TestLoadMigrationsValidatesNames(t *testing.T) {
	tests := []struct {
		name  string
		files fstest.MapFS
		want  string
	}{
		{
			name:  "missing separator",
			files: fstest.MapFS{"m/001init.sql": &fstest.MapFile{Data: []byte("SELECT 1")}},
			want:  "not named",
		},
		{
			name:  "non-numeric version",
			files: fstest.MapFS{"m/init_first.sql": &fstest.MapFile{Data: []byte("SELECT 1")}},
			want:  "non-numeric version",
		},
		{
			name:  "zero version",
			files: fstest.MapFS{"m/000_init.sql": &fstest.MapFile{Data: []byte("SELECT 1")}},
			want:  "must be positive",
		},
		{
			name: "duplicate version",
			files: fstest.MapFS{
				"m/001_init.sql":  &fstest.MapFile{Data: []byte("SELECT 1")},
				"m/001_again.sql": &fstest.MapFile{Data: []byte("SELECT 1")},
			},
			want: "share version",
		},
		{
			name:  "empty directory",
			files: fstest.MapFS{"m/README.md": &fstest.MapFile{Data: []byte("not sql")}},
			want:  "no migrations found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadMigrations(tt.files, "m")
			if err == nil {
				t.Fatalf("loadMigrations succeeded, want an error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadMigrationsSortsNumerically(t *testing.T) {
	files := fstest.MapFS{
		"m/009_nine.sql": &fstest.MapFile{Data: []byte("SELECT 9")},
		"m/010_ten.sql":  &fstest.MapFile{Data: []byte("SELECT 10")},
		"m/001_one.sql":  &fstest.MapFile{Data: []byte("SELECT 1")},
		"m/sub":          &fstest.MapFile{Mode: 0o755 | 1<<31},
	}
	migrations, err := loadMigrations(files, "m")
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	var got []int
	for _, m := range migrations {
		got = append(got, m.version)
	}
	want := []int{1, 9, 10}
	if len(got) != len(want) {
		t.Fatalf("versions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("versions = %v, want %v", got, want)
		}
	}
}
