package cmd

import (
	"errors"
	"strings"
	"testing"
)

func TestDoneLandingNoFailuresIsNil(t *testing.T) {
	t.Parallel()
	var l doneLanding
	if err := l.err(); err != nil {
		t.Fatalf("err() = %v, want nil", err)
	}
}

// TestDoneLandingPrecedence: every unlanded outcome exits non-zero with its
// own code; with several, the one furthest from landing wins, and the
// message lists them all.
func TestDoneLandingPrecedence(t *testing.T) {
	t.Parallel()
	cause := errors.New("origin is at abc, not def")
	for _, tc := range []struct {
		name  string
		fails []int
		want  int
	}{
		{"push", []int{doneExitPushFailed}, doneExitPushFailed},
		{"unverified", []int{doneExitPushUnverified}, doneExitPushUnverified},
		{"mr", []int{doneExitMRFailed}, doneExitMRFailed},
		{"close", []int{doneExitCloseFailed}, doneExitCloseFailed},
		{"close then push", []int{doneExitCloseFailed, doneExitPushFailed}, doneExitPushFailed},
		{"mr then unverified", []int{doneExitMRFailed, doneExitPushUnverified}, doneExitPushUnverified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var l doneLanding
			for i, code := range tc.fails {
				l.fail(code, "failure "+string(rune('A'+i)), cause)
			}
			err := l.err()
			var coded *ExitCodeError
			if !errors.As(err, &coded) || coded.Code != tc.want {
				t.Fatalf("err() = %v, want code %d", err, tc.want)
			}
			for i := range tc.fails {
				if want := "failure " + string(rune('A'+i)); !strings.Contains(err.Error(), want) {
					t.Errorf("message %q lacks %q", err, want)
				}
			}
			if !errors.Is(err, cause) {
				t.Error("cause not reachable through errors.Is")
			}
		})
	}
}

func TestDoneExitCodesAreDistinctAndClearOfCobra(t *testing.T) {
	t.Parallel()
	seen := map[int]bool{}
	for _, c := range []int{doneExitPushFailed, doneExitPushUnverified, doneExitMRFailed, doneExitCloseFailed} {
		if c <= 2 || seen[c] {
			t.Errorf("exit code %d collides with 0/1/2 or another outcome", c)
		}
		seen[c] = true
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

// TestDoneSkipVerifyIsATestSkipAlias: --skip-verify used to skip both the
// test gate and push verification; it now only skips tests, and push
// verification has no flag at all (G2-02).
func TestDoneSkipVerifyIsATestSkipAlias(t *testing.T) {
	resetDoneFlagsForTest(t)
	if err := doneCmd.Flags().Set("skip-verify", "true"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = doneCmd.Flags().Set("skip-verify", "false") })
	if !doneSkipTests {
		t.Fatal("--skip-verify did not set skip-tests")
	}
	f := doneCmd.Flags().Lookup("skip-verify")
	if f == nil || f.Deprecated == "" {
		t.Error("--skip-verify must be a deprecated alias")
	}
	if doneCmd.Flags().Lookup("skip-tests") == nil {
		t.Error("--skip-tests missing")
	}
}
