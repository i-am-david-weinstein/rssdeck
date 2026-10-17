package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"
)

func hashOf(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

func TestCreateAndListTokens(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	tokens, err := s.ListTokens(ctx)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if tokens == nil {
		t.Error("ListTokens returned nil, want an empty slice so JSON encodes as []")
	}

	expires := base.Add(24 * time.Hour)
	created, err := s.CreateToken(ctx, NewToken{
		Name:      "deploy script",
		Prefix:    "rsd_abcd",
		Hash:      hashOf("rsd_abcdefghijklmnop"),
		ExpiresAt: &expires,
		CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if created.ID == 0 {
		t.Error("id = 0, want an assigned row id")
	}
	if created.Name != "deploy script" {
		t.Errorf("name = %q, want %q", created.Name, "deploy script")
	}
	if created.Prefix != "rsd_abcd" {
		t.Errorf("prefix = %q, want %q", created.Prefix, "rsd_abcd")
	}
	if created.ExpiresAt == nil || !created.ExpiresAt.Equal(expires) {
		t.Errorf("expires_at = %v, want %v", created.ExpiresAt, expires)
	}
	if created.LastUsedAt != nil {
		t.Errorf("last_used_at = %v, want nil on a fresh token", created.LastUsedAt)
	}

	never, err := s.CreateToken(ctx, NewToken{
		Name: "agent", Prefix: "rsd_wxyz", Hash: hashOf("rsd_wxyz0987654321"), CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if never.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil to mean never", never.ExpiresAt)
	}

	tokens, err = s.ListTokens(ctx)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("len = %d, want 2", len(tokens))
	}
	if tokens[0].ID != created.ID || tokens[1].ID != never.ID {
		t.Error("ListTokens is not ordered oldest first")
	}
}

// The plaintext never reaches the database, so nothing in a Token can leak it.
func TestTokenStructCarriesNoSecret(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	const plaintext = "rsd_abcdefghijklmnop"
	created, err := s.CreateToken(ctx, NewToken{
		Name: "agent", Prefix: plaintext[:8], Hash: hashOf(plaintext), CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if created.Prefix == plaintext {
		t.Error("the prefix is the whole token, want only its first 8 characters")
	}

	var stored []byte
	err = s.db.QueryRowContext(ctx, `SELECT hash FROM tokens WHERE id = ?`, created.ID).Scan(&stored)
	if err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if string(stored) == plaintext {
		t.Fatal("the plaintext token is in the database, want only its hash")
	}
}

func TestCreateTokenRejectsEmptyHash(t *testing.T) {
	_, err := newStore(t).CreateToken(context.Background(), NewToken{
		Name: "agent", Prefix: "rsd_abcd", CreatedAt: base,
	})
	if err == nil {
		t.Fatal("CreateToken with no hash succeeded, want an error")
	}
}

func TestUseTokenRecordsTheUse(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	hash := hashOf("rsd_abcdefghijklmnop")
	created, err := s.CreateToken(ctx, NewToken{
		Name: "agent", Prefix: "rsd_abcd", Hash: hash, CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	at := base.Add(time.Hour)
	used, err := s.UseToken(ctx, hash, at)
	if err != nil {
		t.Fatalf("UseToken: %v", err)
	}
	if used.ID != created.ID {
		t.Errorf("id = %d, want %d", used.ID, created.ID)
	}
	if used.LastUsedAt == nil || !used.LastUsedAt.Equal(at) {
		t.Errorf("returned last_used_at = %v, want %v", used.LastUsedAt, at)
	}

	stored, err := s.ListTokens(ctx)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if stored[0].LastUsedAt == nil || !stored[0].LastUsedAt.Equal(at) {
		t.Errorf("stored last_used_at = %v, want %v", stored[0].LastUsedAt, at)
	}
}

func TestUseTokenRejectsUnknownAndExpired(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.UseToken(ctx, hashOf("never minted"), base); !errors.Is(err, ErrNotFound) {
		t.Errorf("UseToken with an unknown hash = %v, want ErrNotFound", err)
	}

	hash := hashOf("rsd_abcdefghijklmnop")
	expires := base.Add(time.Hour)
	if _, err := s.CreateToken(ctx, NewToken{
		Name: "short lived", Prefix: "rsd_abcd", Hash: hash, ExpiresAt: &expires, CreatedAt: base,
	}); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	if _, err := s.UseToken(ctx, hash, expires.Add(-time.Second)); err != nil {
		t.Errorf("UseToken just before expiry: %v, want it accepted", err)
	}
	// Expiry is exclusive: at expires_at the token is already dead.
	if _, err := s.UseToken(ctx, hash, expires); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("UseToken at the expiry instant = %v, want ErrTokenExpired", err)
	}
	if _, err := s.UseToken(ctx, hash, expires.Add(time.Hour)); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("UseToken after expiry = %v, want ErrTokenExpired", err)
	}
}

func TestDeleteTokenRevokesIt(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	hash := hashOf("rsd_abcdefghijklmnop")
	created, err := s.CreateToken(ctx, NewToken{
		Name: "agent", Prefix: "rsd_abcd", Hash: hash, CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if err := s.DeleteToken(ctx, created.ID); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	if _, err := s.UseToken(ctx, hash, base); !errors.Is(err, ErrNotFound) {
		t.Errorf("UseToken after revocation = %v, want ErrNotFound", err)
	}
	if err := s.DeleteToken(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteToken = %v, want ErrNotFound", err)
	}
}
