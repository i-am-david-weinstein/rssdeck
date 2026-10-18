package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Error codes are the stable, branchable half of an error response. The message
// beside them is for humans and may be reworded at any time.
const (
	codeUnauthorized        = "unauthorized"
	codeMasterTokenRequired = "master_token_required"
	codeNotFound            = "not_found"
	codeMethodNotAllowed    = "method_not_allowed"
	codeInvalidRequest      = "invalid_request"
	codeRequestTooLarge     = "request_too_large"
	codeInternalError       = "internal_error"

	codeFeedNotFound   = "feed_not_found"
	codeFeedExists     = "feed_exists"
	codeInvalidURL     = "invalid_url"
	codeNotAFeed       = "not_a_feed"
	codeUnreachable    = "feed_unreachable"
	codePollInProgress = "poll_in_progress"
	codePollFailed     = "poll_failed"
	codeTokenNotFound  = "token_not_found"
	codeInvalidStatus  = "invalid_status"
)

// errorBody is the error envelope every failing request answers with.
type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// writeJSON sends v as the response body. A body that fails to encode has
// already had its status written, so there is nothing to do but log it; the
// caller passes a logging function rather than importing the server here.
func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return nil
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// decodeJSON reads a request body into v with the limits every endpoint shares:
// 64KB, and no unknown fields.
//
// The size cap matters because our bodies are a few hundred bytes, and without
// it one oversized POST can exhaust the container's memory. Unknown fields are
// rejected so a misspelled option fails loudly instead of being ignored.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badBody(err)
	}
	// A second JSON value in one body means the client sent something other
	// than the object this endpoint documents.
	if dec.More() {
		return &apiError{status: http.StatusBadRequest, code: codeInvalidRequest,
			message: "request body must contain exactly one JSON object"}
	}
	return nil
}

// maxRequestBody is the cap from the design: our bodies are tiny, and the limit
// is what keeps one oversized POST from being a memory problem.
const maxRequestBody = 64 << 10

// apiError is a failure with the status and code it should be answered with.
// Handlers return one and the wrapper renders it, so no handler writes an error
// response itself.
type apiError struct {
	status  int
	code    string
	message string
	// cause is logged but never sent, for the cases where the client gets
	// "something went wrong" and the operator needs the detail.
	cause error
}

func (e *apiError) Error() string {
	if e.cause != nil {
		return e.message + ": " + e.cause.Error()
	}
	return e.message
}

func (e *apiError) Unwrap() error { return e.cause }

// errorf builds an apiError with a formatted message.
func errorf(status int, code, format string, args ...any) *apiError {
	return &apiError{status: status, code: code, message: fmt.Sprintf(format, args...)}
}

// internalError is the answer to a failure that is the daemon's fault. The
// cause is logged rather than returned: a SQL error is not something a client
// can act on.
func internalError(err error) *apiError {
	return &apiError{
		status:  http.StatusInternalServerError,
		code:    codeInternalError,
		message: "internal error",
		cause:   err,
	}
}

// badBody turns a JSON decoding failure into the right status. The oversized
// case is a 413 rather than a 400, because the client's object may be perfectly
// well formed and only too big.
func badBody(err error) *apiError {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return errorf(http.StatusRequestEntityTooLarge, codeRequestTooLarge,
			"request body is larger than %d bytes", maxRequestBody)
	}
	return &apiError{
		status:  http.StatusBadRequest,
		code:    codeInvalidRequest,
		message: "request body is not valid JSON for this endpoint",
		cause:   err,
	}
}

// wrapError reports a failure the client caused, using the underlying error as
// the message. Package prefixes are trimmed: "sync:" tells a client which Go
// package was involved, which is not information it can use.
func wrapError(status int, code string, err error) *apiError {
	return &apiError{status: status, code: code, message: trimPackagePrefix(err.Error()), cause: err}
}

func trimPackagePrefix(message string) string {
	for _, prefix := range []string{"sync: ", "store: ", "readeck: ", "feed: "} {
		message = strings.TrimPrefix(message, prefix)
	}
	return message
}
