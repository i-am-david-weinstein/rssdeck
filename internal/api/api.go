// Package api is rssdeck's only management surface: an always-listening,
// always-authenticated HTTP API that scripts and agents drive.
//
// Every route is declared in one table, which the server registers and the
// OpenAPI coverage test walks. A route that exists but is missing from the
// served spec fails the build, which is the drift hand-writing a spec would
// otherwise invite.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/i-am-david-weinstein/rssdeck/internal/store"
	"github.com/i-am-david-weinstein/rssdeck/internal/sync"
)

// Syncer is the part of *sync.Syncer the API drives. It is an interface so the
// handler tests can run without a feed server or a Readeck instance.
type Syncer interface {
	// AddFeed fetches and parses the feed before storing it, so a URL that is
	// not a feed fails the request rather than a later poll.
	AddFeed(ctx context.Context, req sync.AddRequest) (*store.Feed, error)

	// Poll polls one feed now.
	Poll(ctx context.Context, feedID int64) (*sync.PollResult, error)

	// Reschedule puts a feed on the poll calendar, or takes it off when it has
	// been disabled, which is what lets a PATCH take effect without a restart.
	Reschedule(ctx context.Context, feedID int64) error

	// Unschedule takes a deleted feed off the calendar.
	Unschedule(feedID int64)

	// NextPoll reports when a feed is next due, and false when it is not on the
	// calendar at all, which is how a disabled feed reads.
	NextPoll(feedID int64) (time.Time, bool)
}

// Options configures a Server. Store, Syncer and MasterToken are required.
type Options struct {
	Store  *store.Store
	Syncer Syncer
	Logger *slog.Logger

	// MasterToken authenticates as the master principal, the only one allowed
	// to manage tokens. The daemon refuses to start without it, so there is no
	// default and no loopback exception.
	MasterToken string

	// now is the clock, replaced by tests. Production uses time.Now.
	now func() time.Time
}

// Server routes and authenticates API requests. It is an http.Handler, holds no
// per-request state, and is safe for concurrent use.
type Server struct {
	store  *store.Store
	syncer Syncer
	log    *slog.Logger
	now    func() time.Time

	// master is the SHA-256 of the master token. Hashing both sides before
	// comparing them keeps it constant time whatever the lengths involved.
	master [32]byte

	mux    *http.ServeMux
	routes []Route
}

// New validates options, registers every route and returns the server.
func New(o Options) (*Server, error) {
	switch {
	case o.Store == nil:
		return nil, errors.New("api: nil store")
	case o.Syncer == nil:
		return nil, errors.New("api: nil syncer")
	case o.MasterToken == "":
		return nil, errors.New("api: empty master token")
	}

	s := &Server{
		store:  o.Store,
		syncer: o.Syncer,
		log:    o.Logger,
		now:    o.now,
		master: tokenHash(o.MasterToken),
		mux:    http.NewServeMux(),
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.register()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Route is one registered method and path, with the credential it demands.
type Route struct {
	Method  string
	Pattern string
	Auth    Auth
}

// Auth is how much credential a route asks for.
type Auth int

const (
	// AuthPublic is unauthenticated: liveness and the spec, which a client may
	// need before it has a token.
	AuthPublic Auth = iota
	// AuthToken accepts the master token or any live minted token.
	AuthToken
	// AuthMaster accepts only the master token. Token management is
	// master-only, so a leaked agent token cannot mint itself persistence or
	// revoke anyone else's.
	AuthMaster
)

// Routes is the registered route table, in registration order. The OpenAPI
// coverage test walks it.
func (s *Server) Routes() []Route {
	return slices.Clone(s.routes)
}

// handler is an API handler. Returning an error rather than writing one is what
// keeps the error envelope, the status mapping and the logging in one place.
type handler func(http.ResponseWriter, *http.Request) error

// methodHandler is one method of one path.
type methodHandler struct {
	method string
	h      handler
}

// endpoint is one path, its methods, and the credential they all demand.
type endpoint struct {
	pattern  string
	auth     Auth
	handlers []methodHandler
}

// register builds the route table and the mux from it.
//
// Each path is also registered without a method, which is what answers an
// unsupported method with the JSON envelope and an Allow header instead of
// net/http's plain text. A pattern carrying a method is more specific than the
// same pattern without one, so the real handlers still win.
func (s *Server) register() {
	get := func(h handler) methodHandler { return methodHandler{http.MethodGet, h} }
	post := func(h handler) methodHandler { return methodHandler{http.MethodPost, h} }
	patch := func(h handler) methodHandler { return methodHandler{http.MethodPatch, h} }
	del := func(h handler) methodHandler { return methodHandler{http.MethodDelete, h} }

	endpoints := []endpoint{
		{"/healthz", AuthPublic, []methodHandler{get(s.health)}},
		{"/api/openapi.yaml", AuthPublic, []methodHandler{get(s.openAPI)}},
		{"/api/feeds", AuthToken, []methodHandler{get(s.listFeeds), post(s.createFeed)}},
		{"/api/feeds/{id}", AuthToken, []methodHandler{get(s.getFeed), patch(s.patchFeed), del(s.deleteFeed)}},
		{"/api/feeds/{id}/poll", AuthToken, []methodHandler{post(s.pollFeed)}},
		{"/api/items", AuthToken, []methodHandler{get(s.listItems)}},
		{"/api/tokens", AuthMaster, []methodHandler{get(s.listTokens), post(s.createToken)}},
		{"/api/tokens/{id}", AuthMaster, []methodHandler{del(s.deleteToken)}},
	}

	for _, e := range endpoints {
		allow := make([]string, 0, len(e.handlers)+1)
		for _, mh := range e.handlers {
			s.routes = append(s.routes, Route{Method: mh.method, Pattern: e.pattern, Auth: e.auth})
			s.mux.Handle(mh.method+" "+e.pattern, s.wrap(e.auth, mh.h))
			allow = append(allow, mh.method)
			if mh.method == http.MethodGet {
				// net/http serves HEAD from a GET route, so the Allow header
				// should admit it.
				allow = append(allow, http.MethodHead)
			}
		}
		s.mux.Handle(e.pattern, s.wrap(AuthPublic, methodNotAllowed(allow)))
	}

	// Anything else is a path the API does not serve. Answering it in the same
	// envelope means a client never has to parse net/http's plain text.
	s.mux.Handle("/", s.wrap(AuthPublic, notFound))
}

// wrap applies authentication, renders whatever the handler returned, and logs
// the request.
func (s *Server) wrap(auth Auth, h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := s.now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}

		principal, err := s.authenticate(r, auth)
		if err == nil {
			err = h(rec, r.WithContext(withPrincipal(r.Context(), principal)))
		}
		if err != nil {
			s.renderError(rec, r, err)
		}

		s.log.Debug("api request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"principal", principal.String(), "duration", s.now().Sub(started))
	})
}

// renderError answers with the error envelope. A failure that is the daemon's
// fault is logged with its cause and reported without one.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		apiErr = internalError(err)
	}
	if apiErr.status == http.StatusUnauthorized {
		// Naming the scheme is what tells a client how to authenticate, and
		// RFC 9110 requires the header on a 401.
		w.Header().Set("WWW-Authenticate", `Bearer realm="rssdeck"`)
	}
	if apiErr.status >= http.StatusInternalServerError {
		s.log.Error("api request failed",
			"method", r.Method, "path", r.URL.Path, "status", apiErr.status,
			"code", apiErr.code, "error", apiErr.Error())
	}
	if werr := writeJSON(w, apiErr.status, errorBody{Error: apiErr.message, Code: apiErr.code}); werr != nil {
		s.log.Debug("could not write an error response", "path", r.URL.Path, "error", werr)
	}
}

// health is liveness only: it answers for as long as the process is serving. It
// deliberately does not touch the database or Readeck, because a dependency
// being briefly unavailable is not a reason for an orchestrator to kill a
// daemon whose whole job is to retry.
func (s *Server) health(w http.ResponseWriter, _ *http.Request) error {
	return writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// notFound answers a path the API does not serve.
func notFound(w http.ResponseWriter, r *http.Request) error {
	return errorf(http.StatusNotFound, codeNotFound, "no such endpoint: %s", r.URL.Path)
}

// methodNotAllowed answers a known path with an unsupported method, naming the
// methods it does support.
func methodNotAllowed(allow []string) handler {
	allowed := slices.Clone(allow)
	slices.Sort(allowed)
	header := strings.Join(allowed, ", ")
	return func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Allow", header)
		return errorf(http.StatusMethodNotAllowed, codeMethodNotAllowed,
			"%s is not allowed on %s; allowed: %s", r.Method, r.URL.Path, header)
	}
}

// recorder remembers the status code for the log line.
type recorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *recorder) WriteHeader(status int) {
	if r.written {
		return
	}
	r.written = true
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.written = true
	return r.ResponseWriter.Write(b)
}

// pathID reads the {id} path segment. A segment that is not a number names no
// row, so it is the client's mistake rather than a missing resource.
func pathID(r *http.Request, what string) (int64, error) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, errorf(http.StatusBadRequest, codeInvalidRequest,
			"%s id must be a positive integer, got %q", what, raw)
	}
	return id, nil
}
