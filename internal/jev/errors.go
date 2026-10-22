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

// The check words a BadResponse carries. Each names one check a 2xx reply
// failed, in fixed words, so a caller's log can say why without quoting the
// reply.
const (
	CheckDecode                     = "decode"
	CheckMissingAnswer              = "missing_answer"
	CheckWrongType                  = "wrong_type"
	CheckNoulMissing                = "noul_missing"
	CheckNoulRange                  = "noul_range"
	CheckScoreMissing               = "score_missing"
	CheckScoreRange                 = "score_range"
	CheckLegendCount                = "legend_count"
	CheckLegendUnknownLabel         = "legend_unknown_label"
	CheckChoiceNotAnOption          = "choice_not_an_option"
	CheckConfidenceRange            = "confidence_range"
	CheckProbabilitiesCount         = "probabilities_count"
	CheckProbabilitiesUnknownOption = "probabilities_unknown_option"
	CheckProbabilityRange           = "probability_range"
	CheckProbabilitiesSum           = "probabilities_sum"
	CheckNotMostProbable            = "not_most_probable"
)

// BadResponse is the error behind ErrBadResponse: the check a 2xx reply
// failed. It matches ErrBadResponse under errors.Is, and its text is the
// reason the check gives.
type BadResponse struct {
	// Check is the check word that names the failed check, one of the Check
	// constants.
	Check string
	// Question is the id of the question whose answer failed. It is set only
	// when Ask asked several questions, the case where the text names it.
	Question string
	// Value is the number a range or sum check read; nil for a check that
	// reads none.
	Value *float64

	cause error
}

func (e *BadResponse) Error() string {
	if e.Question != "" {
		return fmt.Sprintf("%s: question %q: %v", ErrBadResponse, e.Question, e.cause)
	}
	return fmt.Sprintf("%s: %v", ErrBadResponse, e.cause)
}

func (e *BadResponse) Is(target error) bool { return target == ErrBadResponse }

// unusable is the BadResponse for check, with its reason formatted as text.
func unusable(check string, value *float64, format string, args ...any) *BadResponse {
	return &BadResponse{Check: check, Value: value, cause: fmt.Errorf(format, args...)}
}

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
