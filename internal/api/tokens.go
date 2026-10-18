package api

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/i-am-david-weinstein/rssdeck/internal/store"
)

// Token minting constants.
const (
	// tokenBytes is how much entropy a minted token carries. 32 bytes is far
	// past guessing, and base64 makes it 43 characters a client can paste.
	tokenBytes = 32

	// tokenPrefixLength is how much of a token is kept in the clear so a
	// listing can identify it. Eight characters of base64 is 48 bits: enough to
	// tell two tokens apart, nowhere near enough to reconstruct one.
	tokenPrefixLength = 8

	// maxTokenNameLength keeps a listing readable.
	maxTokenNameLength = 64
)

// tokenJSON is a minted token as a listing reports it. The plaintext is not
// here: it exists only in the response that created it.
type tokenJSON struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`

	// Prefix is the token's first 8 characters, which is how an operator
	// matches a row to the token a client is using.
	Prefix string `json:"prefix"`

	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

func newTokenJSON(t *store.Token) tokenJSON {
	return tokenJSON{
		ID:         t.ID,
		Name:       t.Name,
		Prefix:     t.Prefix,
		ExpiresAt:  t.ExpiresAt,
		LastUsedAt: t.LastUsedAt,
		CreatedAt:  t.CreatedAt,
	}
}

// createdTokenJSON is the one response that carries a token's plaintext.
type createdTokenJSON struct {
	tokenJSON

	// Token is the plaintext, returned here and never again: the database
	// holds only its hash, so an operator who loses it mints a new one.
	Token string `json:"token"`
}

// listTokens answers GET /api/tokens. The master token is configuration rather
// than a row, so it never appears.
func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) error {
	tokens, err := s.store.ListTokens(r.Context())
	if err != nil {
		return internalError(err)
	}
	out := make([]tokenJSON, 0, len(tokens))
	for i := range tokens {
		out = append(out, newTokenJSON(&tokens[i]))
	}
	return writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// createTokenRequest is the body of POST /api/tokens.
type createTokenRequest struct {
	// Name is what this token is for, so a listing is something an operator
	// can act on.
	Name string `json:"name"`

	// ExpiresAt is an RFC3339 timestamp. Null never expires.
	ExpiresAt *time.Time `json:"expires_at"`
}

// createToken answers POST /api/tokens, returning the plaintext exactly once.
func (s *Server) createToken(w http.ResponseWriter, r *http.Request) error {
	var req createTokenRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	name := strings.TrimSpace(req.Name)
	switch {
	case name == "":
		return errorf(http.StatusBadRequest, codeInvalidRequest, "name is required")
	case len(name) > maxTokenNameLength:
		return errorf(http.StatusBadRequest, codeInvalidRequest,
			"name cannot be longer than %d characters", maxTokenNameLength)
	}
	now := s.now()
	// A token that is already expired would authenticate nothing, so it is
	// almost certainly a timezone or units mistake worth reporting now.
	if req.ExpiresAt != nil && !req.ExpiresAt.After(now) {
		return errorf(http.StatusBadRequest, codeInvalidRequest,
			"expires_at must be in the future, got %s", req.ExpiresAt.UTC().Format(time.RFC3339))
	}

	plaintext, err := mintToken()
	if err != nil {
		return internalError(err)
	}
	hash := tokenHash(plaintext)
	token, err := s.store.CreateToken(r.Context(), store.NewToken{
		Name:      name,
		Prefix:    plaintext[:tokenPrefixLength],
		Hash:      hash[:],
		ExpiresAt: req.ExpiresAt,
		CreatedAt: now,
	})
	if err != nil {
		return internalError(err)
	}

	s.log.Info("api token minted",
		"token_id", token.ID, "name", token.Name, "prefix", token.Prefix,
		"expires_at", token.ExpiresAt)
	return writeJSON(w, http.StatusCreated, createdTokenJSON{
		tokenJSON: newTokenJSON(token),
		Token:     plaintext,
	})
}

// deleteToken answers DELETE /api/tokens/{id}, revoking the token immediately.
func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "token")
	if err != nil {
		return err
	}
	if err := s.store.DeleteToken(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errorf(http.StatusNotFound, codeTokenNotFound, "no token with id %d", id)
		}
		return internalError(err)
	}
	s.log.Info("api token revoked", "token_id", id)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// mintToken returns a new token's plaintext. The encoding is base64url without
// padding, so the whole token is one word a client can put in a header, a shell
// variable or a URL without escaping.
func mintToken() (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
