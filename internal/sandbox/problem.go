package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// APIError is the engine's own refusal or failure: the problem+json answer
// every error is. Status and Kind are the contract; Detail is the engine's
// sentence for a human. It never holds the token.
type APIError struct {
	Status int    // the HTTP status
	Kind   string // the type's suffix: not-found, conflict, unauthorized, bad-request, at-capacity, internal
	Title  string
	Detail string
}

func (e *APIError) Error() string {
	reason := e.Title
	if reason == "" {
		reason = http.StatusText(e.Status)
	}
	if e.Detail != "" {
		return fmt.Sprintf("the sandbox engine answered %d %s: %s", e.Status, reason, e.Detail)
	}
	return fmt.Sprintf("the sandbox engine answered %d %s", e.Status, reason)
}

// errRequestNotSent marks a request that did not leave: an id that is not
// one, a path that escapes, a command that is empty. Nothing reached the
// engine, so nothing happened.
var errRequestNotSent = errors.New("the request was not sent")

func notSent(err error) error {
	return fmt.Errorf("%w: %w", errRequestNotSent, err)
}

// NotSent reports whether err is a request the client refused to send.
func NotSent(err error) bool { return errors.Is(err, errRequestNotSent) }

// IsNotFound reports whether the engine said there is no such run.
func IsNotFound(err error) bool { return isStatus(err, http.StatusNotFound) }

// IsUnauthorized reports whether the engine refused the token.
func IsUnauthorized(err error) bool { return isStatus(err, http.StatusUnauthorized) }

// IsConflict reports whether the run was in the wrong state for the
// request: stopped, busy, or already in the state asked for.
func IsConflict(err error) bool { return isStatus(err, http.StatusConflict) }

func isStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

// problemFrom reads a problem+json answer. A body that is not one (another
// service on the port, a proxy) is an unknown answer with the first of its
// text as the detail.
func problemFrom(status int, raw []byte, readErr error) error {
	if readErr != nil {
		return readErr
	}
	problem := &APIError{Status: status, Kind: "unknown"}
	var body struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(bytes.TrimSpace(raw), &body) == nil && body.Type != "" {
		problem.Kind = body.Type[strings.LastIndex(body.Type, "/")+1:]
		problem.Title = body.Title
		problem.Detail = body.Detail
		if body.Status != 0 {
			problem.Status = body.Status
		}
	} else if text := strings.Join(strings.Fields(string(raw)), " "); text != "" {
		if len(text) > 200 {
			text = text[:200] + "..."
		}
		problem.Detail = text
	}
	return problem
}

func notFound(id string) error {
	return &APIError{Status: http.StatusNotFound, Kind: "not-found", Title: "Not Found", Detail: fmt.Sprintf("no run %s", id)}
}

// OutcomeKnown reports whether a failed create or fork certainly left no
// run behind. The contract's retry rules: a create is never sent twice, and
// after a lost answer the runs are listed before trying again. So the
// outcome is known only when the engine itself refused (a 4xx problem other
// than 408, or 503 at capacity). No answer, a cut-off wait, a reply that is
// not a problem, a 408 or a 500 all leave it unknown.
func OutcomeKnown(err error) bool {
	if err == nil || NotSent(err) {
		return true
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if apiErr.Kind == "unknown" {
		return false
	}
	refused := apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != http.StatusRequestTimeout
	return refused || apiErr.Status == http.StatusServiceUnavailable
}
