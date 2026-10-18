package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// healthcheckArgs points the healthcheck at a test server, as flags rather than
// environment variables so the test does not depend on resolution order.
func healthcheckArgs(server *httptest.Server) []string {
	return []string{"-addr", strings.TrimPrefix(server.URL, "http://")}
}

func TestHealthcheckSucceedsWhenTheInstanceAnswers(t *testing.T) {
	isolateEnv(t)
	var requested string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	var out strings.Builder
	if err := healthcheck(context.Background(), healthcheckArgs(server), &out, io.Discard); err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	if requested != "/healthz" {
		t.Errorf("healthcheck requested %q, want /healthz", requested)
	}
	if strings.TrimSpace(out.String()) != "ok" {
		t.Errorf("output = %q, want ok", out.String())
	}
}

// The probe reports on liveness and nothing else, so it must not care that the
// Readeck token or the API token is missing: those are serve's business.
func TestHealthcheckDoesNotValidateTheRestOfTheConfiguration(t *testing.T) {
	isolateEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	if err := healthcheck(context.Background(), healthcheckArgs(server), io.Discard, io.Discard); err != nil {
		t.Fatalf("healthcheck failed with no tokens configured: %v", err)
	}
}

func TestHealthcheckFailsOnAnUnhealthyInstance(t *testing.T) {
	isolateEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	err := healthcheck(context.Background(), healthcheckArgs(server), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("healthcheck passed against a 503")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error does not carry the status: %v", err)
	}
}

func TestHealthcheckFailsWhenNothingIsListening(t *testing.T) {
	isolateEnv(t)
	err := healthcheck(context.Background(), []string{"-addr", freeAddr(t)}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("healthcheck passed with nothing listening")
	}
}

func TestHealthURL(t *testing.T) {
	tests := map[string]struct {
		addr    string
		want    string
		wantErr bool
	}{
		// A daemon on the wildcard address is reached from inside its own
		// container as loopback, which is where Docker runs the healthcheck.
		"wildcard ipv4": {"0.0.0.0:8080", "http://127.0.0.1:8080/healthz", false},
		"wildcard ipv6": {"[::]:8080", "http://127.0.0.1:8080/healthz", false},
		"port only":     {":8080", "http://127.0.0.1:8080/healthz", false},
		"explicit host": {"127.0.0.1:9000", "http://127.0.0.1:9000/healthz", false},
		"named host":    {"rssdeck:8080", "http://rssdeck:8080/healthz", false},
		"ipv6 host":     {"[::1]:8080", "http://[::1]:8080/healthz", false},
		"no port":       {"0.0.0.0", "", true},
		"empty":         {"", "", true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := healthURL(tc.addr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("healthURL(%q) = %q, want an error", tc.addr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("healthURL(%q): %v", tc.addr, err)
			}
			if got != tc.want {
				t.Errorf("healthURL(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}
