package polecat

import (
	"errors"
	"strings"
	"testing"
)

func TestSandboxSeatGitStateRunningSandboxIsUnknown(t *testing.T) {
	t.Parallel()
	st := SandboxSeatGitState("polecat/x/gt-1", true, nil, "ma-papaya")
	if st.Source != GitStateSourceUnknown {
		t.Fatalf("Source = %q, want unknown: the work lives in the running sandbox", st.Source)
	}
	if !strings.Contains(st.FailedReason, "ma-papaya") {
		t.Errorf("FailedReason = %q, want it to name the sandbox", st.FailedReason)
	}
}

func TestSandboxSeatGitStateGoneSandboxIsCleanAndLive(t *testing.T) {
	t.Parallel()
	st := SandboxSeatGitState("polecat/x/gt-1", false, nil, "ma-papaya")
	if st.Source != GitStateSourceLive {
		t.Errorf("Source = %q, want live: a gone sandbox has nothing left to measure", st.Source)
	}
	if st.Branch != "polecat/x/gt-1" {
		t.Errorf("Branch = %q, want the host branch label", st.Branch)
	}
	if st.FailedReason != "" {
		t.Errorf("FailedReason = %q, want none on a live answer", st.FailedReason)
	}
	if st.Dirty || st.StashCount != 0 || st.UnpushedCommits != 0 {
		t.Errorf("host worktree facts leaked into a sandbox answer: Dirty=%v StashCount=%d UnpushedCommits=%d",
			st.Dirty, st.StashCount, st.UnpushedCommits)
	}
}

// A gateway that errors (down, timed out) is not an answer: a live sandbox must
// not read as gone, so the state stays unknown whatever exists says.
func TestSandboxSeatGitStateGatewayErrorIsUnknown(t *testing.T) {
	t.Parallel()
	for _, exists := range []bool{false, true} {
		st := SandboxSeatGitState("polecat/x/gt-1", exists, errors.New("gateway unreachable"), "ma-papaya")
		if st.Source != GitStateSourceUnknown {
			t.Errorf("exists=%v with a gateway error: Source = %v, want unknown", exists, st.Source)
		}
		if !strings.Contains(st.FailedReason, "gateway unreachable") {
			t.Errorf("exists=%v: FailedReason = %q, want the gateway error in it", exists, st.FailedReason)
		}
	}
}
