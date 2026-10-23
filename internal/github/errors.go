package github

import (
	"errors"
	"fmt"
	"net/http"
)

// Error kinds, used as the `kind` label of gh_exporter_poll_errors_total.
const (
	KindForbidden = "403"
	KindNotFound  = "404"
	KindServer    = "5xx"
	KindParse     = "parse"
	KindNetwork   = "network"
	KindAuth      = "auth"
)

// Kinds lists every error kind, so counters can be pre-initialised to zero.
var Kinds = []string{KindForbidden, KindNotFound, KindServer, KindParse, KindNetwork, KindAuth}

// Error is a classified API failure.
type Error struct {
	Kind   string
	Status int // HTTP status, 0 if no response
	Op     string
	Err    error
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s: HTTP %d: %v", e.Op, e.Status, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Op, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf classifies err. Unclassified errors count as network failures.
func KindOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindNetwork
}

// IsNotFound reports whether err is an HTTP 404/410 from the API.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == KindNotFound
}

// kindForStatus maps an unexpected HTTP status to an error kind. The metric's
// label set is fixed, so 429 and any other 4xx refusal fold into "403", and a
// 401 that survived the token-refresh retry is "auth".
func kindForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return KindAuth
	case status == http.StatusNotFound || status == http.StatusGone:
		return KindNotFound
	case status >= 500:
		return KindServer
	default:
		return KindForbidden
	}
}
