package done

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// refusedSubmit returns the size refusal of a polecat branch past twice its
// limit, the push answering pushErr (gt-fkhjrv).
func refusedSubmit(t *testing.T, pushErr error) (*submitHarness, error) {
	t.Helper()
	h := pastLimitHarness(t, backendOpenShell, true, false)
	h.repo.pushErrs = []error{pushErr, pushErr}
	sub := doneSubmission{sourceIssue: &beads.Issue{Description: "Change: at most 70 added lines in 2 files."}}
	err := checkBranchForSubmit(h.r, sub, "origin/main")
	if err == nil {
		t.Fatal("checkBranchForSubmit accepted a branch past twice its limit")
	}
	return h, err
}

// agentWrites records each agent-state write as "exit cleanup", answering with
// writeErr (gt-fkhjrv).
func agentWrites(r *doneRun, writeErr error) *[]string {
	var got []string
	r.agentBeadID = "gt-polecat-agent"
	r.env = func(string) string { return "" }
	r.logEvent = func(string, string, map[string]interface{}) error { return nil }
	r.recordAgentState = func(_, _, exit, _ string, p beads.AgentFieldUpdates) (bool, error) {
		got = append(got, exit+" "+*p.CleanupStatus)
		return true, writeErr
	}
	return &got
}

// TestSizeRefusalExitEscalated: a refusal records ESCALATED with its cleanup
// status and returns the refusal, pushed or not; any other error records none
// and goes to failRun (gt-fkhjrv).
func TestSizeRefusalExitEscalated(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, want string
		pushErr    error
	}{
		{"pushed", "ESCALATED clean", nil},
		{"push_fails", "ESCALATED has_unpushed", errors.New("connection reset")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, refusal := refusedSubmit(t, tt.pushErr)
			writes := agentWrites(h.r, nil)
			err := endRunOnSubmitFailure(h.r, h.r.cleanupStatus, refusal, func(error) error { return nil })
			if err != refusal || len(*writes) != 1 || (*writes)[0] != tt.want {
				t.Errorf("returned %v, writes %q; want the refusal and one write %q", err, *writes, tt.want)
			}
		})
	}
	t.Run("other_error_records_none", func(t *testing.T) {
		t.Parallel()
		h := pastLimitHarness(t, backendOpenShell, true, false)
		writes := agentWrites(h.r, nil)
		submitErr, failErr := errors.New("rebase conflict"), errors.New("failRun")
		var failedWith error
		err := endRunOnSubmitFailure(h.r, h.r.cleanupStatus, submitErr, func(e error) error { failedWith = e; return failErr })
		if err != failErr || failedWith != submitErr || len(*writes) != 0 {
			t.Errorf("returned %v, failRun got %v, writes %q; want failRun's %v with %v and no write", err, failedWith, *writes, failErr, submitErr)
		}
	})
}

// TestSizeRefusalExitReportFails: a failed agent-state write returns the refusal
// and that failure together (gt-fkhjrv).
func TestSizeRefusalExitReportFails(t *testing.T) {
	t.Parallel()
	h, refusal := refusedSubmit(t, nil)
	agentWrites(h.r, errors.New("dolt unavailable"))
	err := endRunOnSubmitFailure(h.r, h.r.cleanupStatus, refusal, func(error) error { return nil })
	var sized sizeRefusalError
	if err == nil || !errors.As(err, &sized) || !strings.Contains(err.Error(), "dolt unavailable") {
		t.Errorf("returned %v, want the size refusal and the write failure", err)
	}
}
