package api

import (
	"embed"
	"net/http"
)

// specFS holds the hand-written spec. Serving it from the binary means an agent
// can always fetch the spec of the instance it is actually talking to, rather
// than whatever version of the docs it happens to have.
//
//go:embed openapi.yaml
var specFS embed.FS

// Spec is the OpenAPI document this build serves. The coverage test parses it.
func Spec() []byte {
	spec, err := specFS.ReadFile("openapi.yaml")
	if err != nil {
		// The file is embedded at build time, so a binary that compiled has it.
		panic("api: embedded openapi.yaml is missing: " + err.Error())
	}
	return spec
}

// openAPI answers GET /api/openapi.yaml. It is unauthenticated: a client that
// has no token yet still needs to know how to use one.
func (s *Server) openAPI(w http.ResponseWriter, _ *http.Request) error {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, err := w.Write(Spec())
	return err
}
