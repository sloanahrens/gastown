package cmd

import (
	"errors"
	"strings"
	"testing"
)

func TestDoneExitCodesAreDistinctAndClearOfCobra(t *testing.T) {
	t.Parallel()
	seen := map[int]bool{}
	for _, c := range []int{doneExitPushFailed, doneExitPushUnverified, doneExitReadyFailed, doneExitCloseFailed, doneExitRebaseConflict, doneExitGateFailed} {
		if c <= 2 || seen[c] {
			t.Errorf("exit code %d collides with 0/1/2 or another outcome", c)
		}
		seen[c] = true
	}
}

func TestDoneExitCarriesCodeMessageAndCause(t *testing.T) {
	t.Parallel()
	cause := errors.New("origin is at abc, not def")
	err := doneExit(doneExitPushUnverified, "branch b is not on origin", cause)
	var coded *ExitCodeError
	if !errors.As(err, &coded) || coded.Code != doneExitPushUnverified {
		t.Fatalf("doneExit = %T %v", err, err)
	}
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "branch b is not on origin") || !strings.Contains(err.Error(), "work not submitted") {
		t.Errorf("doneExit message/cause = %v", err)
	}
	if err := doneExit(doneExitGateFailed, "red", nil); !strings.Contains(err.Error(), "red") {
		t.Errorf("nil cause lost the message: %v", err)
	}
}

func TestExecuteExitCodeForCodedError(t *testing.T) {
	t.Parallel()
	if got := exitCodeForError(&ExitCodeError{Code: 12, Err: errors.New("x")}); got != 12 {
		t.Errorf("exitCodeForError(coded 12) = %d", got)
	}
	if got := exitCodeForError(NewSilentExit(3)); got != 3 {
		t.Errorf("exitCodeForError(silent 3) = %d", got)
	}
	if got := exitCodeForError(errors.New("plain")); got != 1 {
		t.Errorf("exitCodeForError(plain) = %d", got)
	}
}
