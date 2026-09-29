package cmd

import (
	"errors"
	"strings"
)

// gt done exit statuses for work that did not land (G5-01). Session
// retirement, witness notification and completion metadata all run before
// gt done returns, so a caller that sees one of these has lost nothing by
// the non-zero exit; it can tell dropped work from landed work.
const (
	// doneExitPushFailed: the push command failed and origin does not have
	// the commit.
	doneExitPushFailed = 10
	// doneExitPushUnverified: origin is not at the commit gt done would
	// declare, although the push command succeeded.
	doneExitPushUnverified = 11
	// doneExitMRFailed: the branch is on origin but no trusted MR bead exists.
	doneExitMRFailed = 12
	// doneExitCloseFailed: the source bead could not be closed.
	doneExitCloseFailed = 13
)

// doneFailure is one unlanded outcome of a gt done run.
type doneFailure struct {
	code  int
	msg   string
	cause error
}

// doneLanding collects every unlanded outcome of one gt done run, in place
// of an error list that was written and never read.
type doneLanding struct {
	failures []doneFailure
}

// fail records one unlanded outcome. cause may be nil.
func (l *doneLanding) fail(code int, msg string, cause error) {
	l.failures = append(l.failures, doneFailure{code: code, msg: msg, cause: cause})
}

// err is nil when everything landed. Otherwise it is an *ExitCodeError whose
// code is the outcome furthest from landing (the lowest code: a failed push
// outranks a failed close) and whose message lists every failure.
func (l *doneLanding) err() error {
	if len(l.failures) == 0 {
		return nil
	}
	code := l.failures[0].code
	msgs := make([]string, 0, len(l.failures))
	var causes []error
	for _, f := range l.failures {
		if f.code < code {
			code = f.code
		}
		msgs = append(msgs, f.msg)
		if f.cause != nil {
			causes = append(causes, f.cause)
		}
	}
	msg := "gt done: work did not land: " + strings.Join(msgs, "; ")
	return &ExitCodeError{Code: code, Err: &doneLandingError{msg: msg, causes: errors.Join(causes...)}}
}

// doneLandingError prints the failure list while the causes stay reachable
// through errors.Is and errors.As.
type doneLandingError struct {
	msg    string
	causes error
}

func (e *doneLandingError) Error() string { return e.msg }

func (e *doneLandingError) Unwrap() error { return e.causes }
