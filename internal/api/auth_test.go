package api

import (
	"net/http"
	"testing"
	"time"
)

func TestRejectedCredentials(t *testing.T) {
	tests := map[string]struct {
		header string
	}{
		"no header":        {""},
		"wrong scheme":     {"Basic " + masterToken},
		"no scheme":        {masterToken},
		"empty token":      {"Bearer "},
		"unknown token":    {"Bearer not-a-real-token"},
		"master truncated": {"Bearer " + masterToken[:len(masterToken)-1]},
		"master extended":  {"Bearer " + masterToken + "x"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			req := requestFor(http.MethodGet, "/api/feeds")
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := e.serve(req)

			e.expectError(rec, http.StatusUnauthorized, codeUnauthorized)
			if got := rec.Header().Get("WWW-Authenticate"); got == "" {
				t.Error("401 carried no WWW-Authenticate header, so a client is not told the scheme")
			}
		})
	}
}

func TestMasterTokenIsAccepted(t *testing.T) {
	e := newEnv(t)
	e.expectStatus(e.do(http.MethodGet, "/api/feeds", masterToken, nil), http.StatusOK)
}

func TestMintedTokenDrivesFeedsAndRecordsItsUse(t *testing.T) {
	e := newEnv(t)
	plaintext, token := e.mintToken("agent", nil)
	if token.LastUsedAt != nil {
		t.Fatalf("a token that has not been used has last_used_at = %v, want null", token.LastUsedAt)
	}

	e.now = start.Add(2 * time.Minute)
	e.expectStatus(e.do(http.MethodGet, "/api/feeds", plaintext, nil), http.StatusOK)

	rec := e.do(http.MethodGet, "/api/tokens", masterToken, nil)
	e.expectStatus(rec, http.StatusOK)
	var listed struct {
		Tokens []tokenJSON `json:"tokens"`
	}
	e.decode(rec, &listed)
	if len(listed.Tokens) != 1 {
		t.Fatalf("listed %d tokens, want 1", len(listed.Tokens))
	}
	used := listed.Tokens[0].LastUsedAt
	if used == nil || !used.Equal(e.now) {
		t.Fatalf("last_used_at = %v, want %v", used, e.now)
	}
}

func TestExpiredTokenIsRejected(t *testing.T) {
	e := newEnv(t)
	expires := start.Add(time.Hour)
	plaintext, _ := e.mintToken("short lived", &expires)

	e.expectStatus(e.do(http.MethodGet, "/api/feeds", plaintext, nil), http.StatusOK)

	// At its expiry, not merely after it: a token good "until" a time is not
	// good at that time.
	e.now = expires
	e.expectError(e.do(http.MethodGet, "/api/feeds", plaintext, nil),
		http.StatusUnauthorized, codeUnauthorized)
}

func TestRevokedTokenIsRejected(t *testing.T) {
	e := newEnv(t)
	plaintext, token := e.mintToken("to be revoked", nil)

	e.expectStatus(e.do(http.MethodDelete, "/api/tokens/"+itoa(token.ID), masterToken, nil),
		http.StatusNoContent)
	e.expectError(e.do(http.MethodGet, "/api/feeds", plaintext, nil),
		http.StatusUnauthorized, codeUnauthorized)
}

func TestTokenManagementIsMasterOnly(t *testing.T) {
	e := newEnv(t)
	plaintext, token := e.mintToken("agent", nil)

	requests := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/tokens", nil},
		{http.MethodPost, "/api/tokens", map[string]any{"name": "minted by an agent"}},
		{http.MethodDelete, "/api/tokens/" + itoa(token.ID), nil},
	}
	for _, req := range requests {
		t.Run(req.method+" "+req.path, func(t *testing.T) {
			rec := e.do(req.method, req.path, plaintext, req.body)
			e.expectError(rec, http.StatusForbidden, codeMasterTokenRequired)
		})
	}

	// The agent token could neither mint nor revoke, so the one token it was
	// issued is still the only one.
	rec := e.do(http.MethodGet, "/api/tokens", masterToken, nil)
	var listed struct {
		Tokens []tokenJSON `json:"tokens"`
	}
	e.decode(rec, &listed)
	if len(listed.Tokens) != 1 {
		t.Fatalf("listed %d tokens, want the 1 that was minted by the master token", len(listed.Tokens))
	}
}

func TestSpecIsUnauthenticated(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodGet, "/api/openapi.yaml", "", nil)
	e.expectStatus(rec, http.StatusOK)

	if got := rec.Header().Get("Content-Type"); got != "application/yaml; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if body := rec.Body.String(); body != string(Spec()) {
		t.Error("served spec is not the embedded one")
	}
}
