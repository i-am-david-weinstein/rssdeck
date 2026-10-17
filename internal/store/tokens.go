package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Token is a minted API token. The plaintext is never stored: only its SHA-256
// hash, plus a short prefix so listings can identify it.
type Token struct {
	ID         int64
	Name       string
	Prefix     string
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	CreatedAt  time.Time
}

// NewToken is the input to CreateToken. Hashing and prefix extraction happen in
// the API layer, which is the only place the plaintext exists.
type NewToken struct {
	Name      string
	Prefix    string
	Hash      []byte
	ExpiresAt *time.Time
	CreatedAt time.Time
}

const tokenColumns = `id, name, prefix, expires_at, last_used_at, created_at`

// CreateToken stores a minted token.
func (s *Store) CreateToken(ctx context.Context, t NewToken) (*Token, error) {
	if len(t.Hash) == 0 {
		return nil, errors.New("store: create token: empty hash")
	}
	const q = `INSERT INTO tokens (name, prefix, hash, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)
		RETURNING ` + tokenColumns

	row := s.db.QueryRowContext(ctx, q,
		t.Name,
		t.Prefix,
		t.Hash,
		timePtr(t.ExpiresAt),
		formatTime(t.CreatedAt),
	)
	token, err := scanToken(row)
	if err != nil {
		return nil, fmt.Errorf("store: create token %q: %w", t.Name, err)
	}
	return token, nil
}

// ListTokens returns every minted token, oldest first. The master token is
// configuration, not a row, so it never appears here.
func (s *Store) ListTokens(ctx context.Context) ([]Token, error) {
	const q = `SELECT ` + tokenColumns + ` FROM tokens ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: list tokens: %w", err)
	}
	defer rows.Close()

	tokens := []Token{}
	for rows.Next() {
		token, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list tokens: %w", err)
		}
		tokens = append(tokens, *token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list tokens: %w", err)
	}
	return tokens, nil
}

// DeleteToken revokes a token.
func (s *Store) DeleteToken(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete token %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete token %d: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: token %d", ErrNotFound, id)
	}
	return nil
}

// UseToken resolves the hash of a presented token for an authenticated request
// and records the use.
//
// An unknown hash is ErrNotFound and an expired one is ErrTokenExpired, both of
// which the caller must answer with the same 401: the distinction is for logs,
// not for clients.
func (s *Store) UseToken(ctx context.Context, hash []byte, now time.Time) (*Token, error) {
	const q = `SELECT ` + tokenColumns + ` FROM tokens WHERE hash = ?`
	token, err := scanToken(s.db.QueryRowContext(ctx, q, hash))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: token", ErrNotFound)
		}
		return nil, fmt.Errorf("store: look up token: %w", err)
	}
	if token.ExpiresAt != nil && !now.Before(*token.ExpiresAt) {
		return nil, fmt.Errorf("%w: token %d (%s)", ErrTokenExpired, token.ID, token.Prefix)
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET last_used_at = ? WHERE id = ?`, formatTime(now), token.ID); err != nil {
		return nil, fmt.Errorf("store: touch token %d: %w", token.ID, err)
	}
	last := now.UTC()
	token.LastUsedAt = &last
	return token, nil
}

func scanToken(row rowScanner) (*Token, error) {
	var (
		t         Token
		expires   nullTime
		lastUsed  nullTime
		createdAt nullTime
	)
	if err := row.Scan(&t.ID, &t.Name, &t.Prefix, &expires, &lastUsed, &createdAt); err != nil {
		return nil, err
	}
	t.ExpiresAt = expires.ptr()
	t.LastUsedAt = lastUsed.ptr()
	t.CreatedAt = createdAt.Time
	return &t, nil
}
