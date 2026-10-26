package main

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// TestJudgeSkipsWhatItCannotJudge: a run that times out or exits non-zero is skipped with its reason,
// and its stdout is not judged, since it is error text (gt-keakj).
func TestJudgeSkipsWhatItCannotJudge(t *testing.T) {
	t.Parallel()
	notice := []byte("note: rebuilding\n{}\n")
	if got := judge("json", 0, true, notice); got.skip != "timed out" || got.err != nil {
		t.Errorf("a timed-out run = %+v, want skipped as timed out", got)
	}
	if got := judge("json", 2, false, notice); got.skip != "exited 2" || got.err != nil {
		t.Errorf("a non-zero run = %+v, want skipped as exited 2 with its stdout not judged", got)
	}
}

// TestJudgeFailsAStdoutProblemOnACleanRun: a run that exits zero is judged: a notice on --json stdout
// fails it, JSON-only and empty pass (gt-keakj).
func TestJudgeFailsAStdoutProblemOnACleanRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mode   string
		stdout string
		fails  bool
	}{
		{"json only", "json", `{"ok":true}`, false},
		{"json with a notice", "json", "note: x\n{}\n", true},
		{"quiet empty", "quiet", "", false},
		{"quiet with output", "quiet", "done\n", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := judge(tc.mode, 0, false, []byte(tc.stdout))
			if (got.err != nil) != tc.fails || got.skip != "" {
				t.Errorf("judge(%s, %q) = %+v, want a failure: %v", tc.mode, tc.stdout, got, tc.fails)
			}
		})
	}
}
