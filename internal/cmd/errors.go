package cmd

import (
	"errors"
	"fmt"

	"github.com/steveyegge/gastown/internal/done"
)

// SilentExitError signals that the command should exit with a specific code
// without printing an error message. This is used for scripting purposes
// where exit codes convey status (e.g., "no mail" = exit 1).
type SilentExitError struct {
	Code int
}

func (e *SilentExitError) Error() string {
	return fmt.Sprintf("exit %d", e.Code)
}

// NewSilentExit creates a SilentExitError with the given exit code.
func NewSilentExit(code int) *SilentExitError {
	return &SilentExitError{Code: code}
}

// IsSilentExit checks if an error is a SilentExitError and returns its code.
// Uses errors.As to properly handle wrapped errors.
// Returns 0 and false if err is nil or not a SilentExitError.
func IsSilentExit(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var se *SilentExitError
	if errors.As(err, &se) {
		return se.Code, true
	}
	return 0, false
}

// ExitCodeError is a command failure that exits with Code. Unlike
// SilentExitError it carries a message, which cobra prints like any error.
type ExitCodeError struct {
	Code int
	Err  error
}

func (e *ExitCodeError) Error() string { return e.Err.Error() }

func (e *ExitCodeError) Unwrap() error { return e.Err }

// exitCodeForError is the process exit status for a command's error:
// SilentExitError, ExitCodeError and done.ExitCodeError carry their own,
// anything else is 1. gt done's submission failures are raised in
// internal/done, which cannot import this package, so its coded error is
// mapped here.
func exitCodeForError(err error) int {
	if code, ok := IsSilentExit(err); ok {
		return code
	}
	var coded *ExitCodeError
	if errors.As(err, &coded) {
		return coded.Code
	}
	var doneCoded *done.ExitCodeError
	if errors.As(err, &doneCoded) {
		return doneCoded.Code
	}
	return 1
}
