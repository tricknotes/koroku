package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Sentinel errors for errors.Is. Errors returned by Client wrap one of them
// whenever the cause is known.
var (
	// ErrNotFound means GitHub does not know the resource, or the app cannot see it.
	ErrNotFound = errors.New("github: not found")
	// ErrUnauthorized means GitHub rejected the credentials or the permission (401 / 403).
	ErrUnauthorized = errors.New("github: unauthorized")
	// ErrRateLimited means GitHub throttled the request (429, or 403 with rate limit headers).
	ErrRateLimited = errors.New("github: rate limited")
	// ErrInvalidArgument means an argument was rejected before any request was sent.
	ErrInvalidArgument = errors.New("github: invalid argument")
	// ErrTooLarge means a file is bigger than MaxFileSize.
	ErrTooLarge = errors.New("github: file too large")
	// ErrNotConfigured means the call needs App credentials and the client
	// has none (NewAnonymous).
	ErrNotConfigured = errors.New("github: no app credentials")
)

// APIError is a non-2xx response from GitHub.
type APIError struct {
	// Status is the HTTP status code.
	Status int
	// Message is GitHub's explanation, scrubbed of credentials and truncated.
	Message string
	// RateLimited is set when the response carried rate limit headers.
	RateLimited bool

	// notFound marks statuses other than 404 that still mean "no such thing".
	notFound bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub responded %d: %s", e.Status, e.Message)
}

// Is maps the status onto the package's sentinel errors.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Status == http.StatusNotFound || e.notFound
	case ErrRateLimited:
		return e.RateLimited
	case ErrUnauthorized:
		return !e.RateLimited && (e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden)
	}
	return false
}

// newAPIError builds an APIError from a response. Only the "message" field of
// a JSON body is kept; anything else (for example an HTML error page) is
// replaced by the status text.
func newAPIError(resp *http.Response, body []byte, secrets []string) *APIError {
	var payload struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &payload)
	msg := sanitize(payload.Message, secrets)
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	limited := resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden &&
			(resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != ""))
	return &APIError{Status: resp.StatusCode, Message: msg, RateLimited: limited}
}

// graphQLError is one entry of the "errors" list of a GraphQL response.
// GitHub answers these with HTTP 200.
type graphQLError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// apiError expresses the error as the response the REST API gives for the
// same cause, so that callers tell the causes apart the same way.
func (e graphQLError) apiError(secrets []string) *APIError {
	out := &APIError{Status: http.StatusUnprocessableEntity, Message: sanitize(e.Message, secrets)}
	switch e.Type {
	case "NOT_FOUND":
		out.Status = http.StatusNotFound
	case "FORBIDDEN", "INSUFFICIENT_SCOPES":
		out.Status = http.StatusForbidden
	case "RATE_LIMITED":
		out.Status, out.RateLimited = http.StatusForbidden, true
	}
	if out.Message == "" {
		out.Message = http.StatusText(out.Status)
	}
	return out
}

// OAuthError is an error GitHub reports for the code exchange. GitHub answers
// these with HTTP 200 and an "error" field.
type OAuthError struct {
	// Code is GitHub's machine-readable error, for example "bad_verification_code".
	Code string
	// Description is GitHub's explanation, scrubbed of credentials and truncated.
	Description string
}

func (e *OAuthError) Error() string {
	if e.Description == "" {
		return "GitHub OAuth error " + e.Code
	}
	return "GitHub OAuth error " + e.Code + ": " + e.Description
}

// Is reports every OAuth error as ErrUnauthorized: the login did not succeed.
func (e *OAuthError) Is(target error) bool { return target == ErrUnauthorized }

// sanitize makes a message from GitHub safe to store and print: credentials
// are replaced, control characters are flattened and the length is capped.
func sanitize(msg string, secrets []string) string {
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, redacted)
		}
	}
	msg = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, msg)
	msg = strings.TrimSpace(msg)
	if len(msg) > maxErrorMessage {
		cut := maxErrorMessage
		// Do not split a multi-byte character.
		for cut > 0 && msg[cut]&0xC0 == 0x80 {
			cut--
		}
		msg = msg[:cut] + "..."
	}
	return msg
}
