package jev

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrTimeout marks a call that did not finish within the client's
	// timeout; the caller maps it to "no answer".
	ErrTimeout = errors.New("jev: request timed out")

	// ErrRateLimited marks an HTTP 429. The client never retries it; the
	// caller decides whether and when to ask again.
	ErrRateLimited = errors.New("jev: rate limited")

	// ErrUnauthorized marks an HTTP 401: the key is missing or invalid.
	ErrUnauthorized = errors.New("jev: unauthorized")

	// ErrBadResponse marks a 2xx response that does not hold a usable answer
	// for every question asked.
	ErrBadResponse = errors.New("jev: unusable response")
)

// RateLimitError is the error behind an HTTP 429. It matches ErrRateLimited
// under errors.Is.
type RateLimitError struct {
	// RetryAfter is the delay the server asked for through Retry-After or
	// retry-after-ms; zero when it named none or an unusable value.
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%s (retry after %s)", ErrRateLimited, e.RetryAfter)
	}
	return ErrRateLimited.Error()
}

func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// StatusError is an unsuccessful HTTP response other than 401 and 429.
type StatusError struct {
	Status int
	// Message is the server's error text, cut to a short length.
	Message string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("jev: http %d", e.Status)
	}
	return fmt.Sprintf("jev: http %d: %s", e.Status, e.Message)
}
