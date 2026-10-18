package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMintedTokenIsReturnedOnceAndStoredHashed(t *testing.T) {
	e := newEnv(t)
	expires := start.Add(24 * time.Hour)

	rec := e.do(http.MethodPost, "/api/tokens", masterToken,
		map[string]any{"name": "curator", "expires_at": expires.Format(time.RFC3339)})
	e.expectStatus(rec, http.StatusCreated)

	var created createdTokenJSON
	e.decode(rec, &created)
	switch {
	case created.Name != "curator":
		t.Errorf("name = %q", created.Name)
	case created.Token == "":
		t.Fatal("no plaintext in the response that created the token")
	case created.Prefix != created.Token[:tokenPrefixLength]:
		t.Errorf("prefix %q is not the first %d characters of the token", created.Prefix, tokenPrefixLength)
	case created.ExpiresAt == nil || !created.ExpiresAt.Equal(expires):
		t.Errorf("expires_at = %v, want %v", created.ExpiresAt, expires)
	case !created.CreatedAt.Equal(start):
		t.Errorf("created_at = %v, want %v", created.CreatedAt, start)
	}

	// The plaintext is in that response and nowhere else: a listing carries
	// only the prefix.
	listing := e.do(http.MethodGet, "/api/tokens", masterToken, nil)
	e.expectStatus(listing, http.StatusOK)
	if strings.Contains(listing.Body.String(), created.Token) {
		t.Error("the token listing leaked a token's plaintext")
	}

	var listed struct {
		Tokens []tokenJSON `json:"tokens"`
	}
	e.decode(listing, &listed)
	if len(listed.Tokens) != 1 || listed.Tokens[0].Prefix != created.Prefix {
		t.Fatalf("listed %+v, want the one token with prefix %q", listed.Tokens, created.Prefix)
	}

	// The token works, which is what proves the stored hash matches the
	// plaintext that was handed out.
	e.expectStatus(e.do(http.MethodGet, "/api/feeds", created.Token, nil), http.StatusOK)
}

func TestMintedTokensAreDistinct(t *testing.T) {
	e := newEnv(t)
	first, _ := e.mintToken("one", nil)
	second, _ := e.mintToken("two", nil)
	if first == second {
		t.Fatal("two minted tokens are identical")
	}
	if len(first) < 32 {
		t.Errorf("token is %d characters, want enough entropy to be unguessable", len(first))
	}
}

func TestMintTokenValidatesTheRequest(t *testing.T) {
	past := start.Add(-time.Hour)
	tests := map[string]any{
		"no name":           map[string]any{},
		"blank name":        map[string]any{"name": "   "},
		"name too long":     map[string]any{"name": strings.Repeat("n", maxTokenNameLength+1)},
		"expired already":   map[string]any{"name": "late", "expires_at": past.Format(time.RFC3339)},
		"expires now":       map[string]any{"name": "now", "expires_at": start.Format(time.RFC3339)},
		"expiry not a time": map[string]any{"name": "wrong", "expires_at": "tomorrow"},
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.expectError(e.do(http.MethodPost, "/api/tokens", masterToken, body),
				http.StatusBadRequest, codeInvalidRequest)
		})
	}
}

func TestTokenListIsEmptyRatherThanNull(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodGet, "/api/tokens", masterToken, nil)
	e.expectStatus(rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, `"tokens": []`) {
		t.Errorf("empty listing rendered as %s", body)
	}
}

func TestDeleteTokenNotFound(t *testing.T) {
	e := newEnv(t)
	e.expectError(e.do(http.MethodDelete, "/api/tokens/99", masterToken, nil),
		http.StatusNotFound, codeTokenNotFound)
}

// The master token is configuration rather than a row, so there is nothing to
// revoke and nothing to list.
func TestMasterTokenIsNotARow(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodGet, "/api/tokens", masterToken, nil)
	e.expectStatus(rec, http.StatusOK)

	var listed struct {
		Tokens []tokenJSON `json:"tokens"`
	}
	e.decode(rec, &listed)
	if len(listed.Tokens) != 0 {
		t.Fatalf("listed %d tokens before any were minted", len(listed.Tokens))
	}
}
