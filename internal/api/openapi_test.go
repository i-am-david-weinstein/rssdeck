package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// specOperation is one operation found in the served spec.
type specOperation struct {
	method string
	path   string
	public bool // declares security: [], so it is documented as unauthenticated
}

// parseSpecOperations reads the operations out of the spec's paths section.
//
// This is a deliberately small reader rather than a YAML parser: the daemon's
// pitch is few dependencies, and a YAML library pulled in for one test is a
// dependency in the module graph. The spec is hand-written here with a fixed
// two-space layout, so indentation is enough to find paths, methods and a
// security override.
func parseSpecOperations(t *testing.T, spec string) []specOperation {
	t.Helper()
	methods := []string{"get", "post", "put", "patch", "delete", "head", "options"}

	var (
		found     []specOperation
		inPaths   bool
		path      string
		operation = -1
	)
	for _, line := range strings.Split(spec, "\n") {
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		// A key in the first column ends the paths section.
		if !strings.HasPrefix(line, " ") {
			inPaths = line == "paths:"
			path, operation = "", -1
			continue
		}
		if !inPaths {
			continue
		}

		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		key := strings.TrimSuffix(trimmed, ":")

		switch {
		case indent == 2 && strings.HasPrefix(trimmed, "/") && strings.HasSuffix(trimmed, ":"):
			path, operation = key, -1
		case indent == 4 && path != "" && slices.Contains(methods, key):
			found = append(found, specOperation{method: strings.ToUpper(key), path: path})
			operation = len(found) - 1
		case indent == 6 && operation >= 0 && trimmed == "security: []":
			found[operation].public = true
		}
	}
	if len(found) == 0 {
		t.Fatal("found no operations in the spec, so this reader is broken rather than the spec being empty")
	}
	return found
}

// The spec is hand-written, so the drift it invites is a route that exists and
// is not documented, or a documented route that no longer exists. This is the
// check the design asks for, and it is why the route table is one list.
func TestSpecCoversEveryRouteAndNoneMore(t *testing.T) {
	e := newEnv(t)
	documented := parseSpecOperations(t, string(Spec()))

	inSpec := func(method, pattern string) bool {
		return slices.ContainsFunc(documented, func(o specOperation) bool {
			return o.method == method && o.path == pattern
		})
	}
	for _, route := range e.server.Routes() {
		if !inSpec(route.Method, route.Pattern) {
			t.Errorf("%s %s is served but missing from openapi.yaml", route.Method, route.Pattern)
		}
	}

	registered := e.server.Routes()
	for _, op := range documented {
		if !slices.ContainsFunc(registered, func(r Route) bool {
			return r.Method == op.method && r.Pattern == op.path
		}) {
			t.Errorf("%s %s is in openapi.yaml but not served", op.method, op.path)
		}
	}
}

// A route the spec documents as unauthenticated while the server demands a
// token, or the reverse, is worse than an undocumented route: a client would
// trust it.
func TestSpecAgreesOnWhichRoutesArePublic(t *testing.T) {
	e := newEnv(t)
	documented := parseSpecOperations(t, string(Spec()))

	for _, route := range e.server.Routes() {
		index := slices.IndexFunc(documented, func(o specOperation) bool {
			return o.method == route.Method && o.path == route.Pattern
		})
		if index < 0 {
			continue // reported by the coverage test
		}
		wantPublic := route.Auth == AuthPublic
		if documented[index].public != wantPublic {
			t.Errorf("%s %s: spec says public = %v, server says %v",
				route.Method, route.Pattern, documented[index].public, wantPublic)
		}
	}
}

// Every path in the table is really registered, which is what makes the
// coverage test above meaningful: a table that lists a route the mux never saw
// would pass it while the API 404s.
func TestEveryRouteInTheTableIsServed(t *testing.T) {
	e := newEnv(t)
	for _, route := range e.server.Routes() {
		path := strings.ReplaceAll(route.Pattern, "{id}", "1")
		rec := e.do(route.Method, path, masterToken, "{}")
		if rec.Code == http.StatusNotFound {
			var body errorBody
			e.decode(rec, &body)
			if body.Code == codeNotFound {
				t.Errorf("%s %s answered as an unknown endpoint", route.Method, path)
			}
		}
		if rec.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s answered method not allowed", route.Method, path)
		}
	}
}

func TestSpecIsServedWithTheRoutesItDocuments(t *testing.T) {
	spec := string(Spec())

	for _, want := range []string{"openapi: 3.1.0", "bearerAuth", "/api/feeds/{id}/poll"} {
		if !strings.Contains(spec, want) {
			t.Errorf("spec does not mention %q", want)
		}
	}
}
