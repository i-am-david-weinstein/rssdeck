package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/i-am-david-weinstein/rssdeck/internal/store"
)

// principalKind is which credential a request arrived with.
type principalKind int

const (
	// The zero value is a request to a public route, or one that has not been
	// authenticated yet. It has no name because nothing branches on it: every
	// route that reads a principal has already required a credential.
	_ principalKind = iota
	// kindMaster is the token from the configuration.
	kindMaster
	// kindMinted is a token created through the API.
	kindMinted
)

// principal is who made the request. It exists for the master-only check and
// for the log line; nothing else branches on it.
type principal struct {
	kind    principalKind
	tokenID int64
	name    string
	prefix  string
}

func (p principal) String() string {
	switch p.kind {
	case kindMaster:
		return "master"
	case kindMinted:
		return fmt.Sprintf("token %d %q (%s)", p.tokenID, p.name, p.prefix)
	default:
		return "anonymous"
	}
}

// principalContextKey is unexported so nothing outside this package can put a
// principal into a context.
type principalContextKey struct{}

func withPrincipal(ctx context.Context, p principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

// principalFor returns who made the request, anonymous when nothing
// authenticated it.
func principalFor(ctx context.Context) principal {
	p, _ := ctx.Value(principalContextKey{}).(principal)
	return p
}

// tokenHash is the one hash used for tokens: SHA-256 of the plaintext. Minted
// tokens are stored as this and never in plaintext, and the master token is
// compared as this so the comparison cannot leak its length.
func tokenHash(token string) [32]byte {
	return sha256.Sum256([]byte(token))
}

// authenticate resolves the credential a request presented against what the
// route demands.
//
// Every rejection is the same 401 with the same code, whether the token is
// missing, unknown, revoked or expired. Which one it was goes to the log: a
// client learning that a token exists but has expired is information it has no
// use for and an attacker does.
func (s *Server) authenticate(r *http.Request, auth Auth) (principal, error) {
	if auth == AuthPublic {
		return principal{}, nil
	}

	presented, err := bearerToken(r)
	if err != nil {
		return principal{}, err
	}

	p, err := s.resolve(r.Context(), presented)
	if err != nil {
		return principal{}, err
	}
	if auth == AuthMaster && p.kind != kindMaster {
		return p, errorf(http.StatusForbidden, codeMasterTokenRequired,
			"this endpoint requires the master token")
	}
	return p, nil
}

// resolve identifies a presented token. The master token is checked first and
// without touching the database, so a request that authenticates as master
// never shows up as a token lookup.
func (s *Server) resolve(ctx context.Context, presented string) (principal, error) {
	hash := tokenHash(presented)
	if subtle.ConstantTimeCompare(hash[:], s.master[:]) == 1 {
		return principal{kind: kindMaster}, nil
	}

	token, err := s.store.UseToken(ctx, hash[:], s.now())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.log.Warn("rejected an unknown api token")
		case errors.Is(err, store.ErrTokenExpired):
			s.log.Warn("rejected an expired api token", "error", err)
		default:
			return principal{}, internalError(err)
		}
		return principal{}, unauthorized("the bearer token is not valid")
	}
	return principal{
		kind:    kindMinted,
		tokenID: token.ID,
		name:    token.Name,
		prefix:  token.Prefix,
	}, nil
}

// bearerToken reads the Authorization header. Only the Bearer scheme is
// accepted, and only with a non-empty token.
func bearerToken(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", unauthorized("missing Authorization header")
	}
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", unauthorized("Authorization header must be: Bearer <token>")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", unauthorized("empty bearer token")
	}
	return token, nil
}

func unauthorized(message string) *apiError {
	return errorf(http.StatusUnauthorized, codeUnauthorized, "%s", message)
}
