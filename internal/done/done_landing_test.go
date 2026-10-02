package done

import (
	"errors"
	"strings"
	"testing"
)

func TestDoneExitCodesAreDistinctAndClearOfCobra(t *testing.T) {
	t.Parallel()
	seen := map[int]bool{}
	for _, c := range []int{doneExitPushFailed, doneExitPushUnverified, doneExitReadyFailed, doneExitCloseFailed, doneExitRebaseConflict, doneExitGateFailed, doneExitGateUnavailable} {
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
