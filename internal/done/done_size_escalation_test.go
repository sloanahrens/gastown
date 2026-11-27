package done

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestSizeEscalationCalled: the function gets the bead id and the returned refusal once (gt-3odsvc).
func TestSizeEscalationCalled(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, backend, last string
		pushErr             error
	}{
		{"pushed", backendOpenShell, "your work is pushed at ", nil},
		{"push_fails", backendOpenShell, "your work is NOT pushed: ", errors.New("connection reset")},
		{"host_pushes_nothing", "host", overLimitLine, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := pastLimitHarness(t, tt.backend, true, false)
			h.repo.pushErrs = []error{tt.pushErr, tt.pushErr}
			var calls [][2]string
			h.r.raiseSizeRefusal = func(bead, refusal string) error {
				calls = append(calls, [2]string{bead, refusal})
				return nil
			}
			err := checkBranchForSubmit(h.r, doneSubmission{sourceIssue: &beads.Issue{Description: "Change: at most 70 added lines in 2 files."}}, "origin/main")
			if err == nil || !strings.Contains(err.Error(), "\n"+tt.last) {
				t.Fatalf("error = %v, want a refusal whose last line starts %q", err, tt.last)
			}
			if want := [2]string{"bd-source", err.Error()}; len(calls) != 1 || calls[0] != want {
				t.Errorf("escalation calls = %q, want exactly %q", calls, want)
			}
		})
	}
}

// TestSizeEscalationNotRaised: a nil or erroring function prints one line; a crew branch or an unreadable count never calls it (gt-3odsvc).
func TestSizeEscalationNotRaised(t *testing.T) {
	t.Parallel()
	const noRoute = "no escalation was raised: this run has no escalation route; raise it with gt escalate"
	for _, tt := range []struct {
		name, wantErr, wantLine      string
		polecat, countFail, nilRaise bool
		raiseErr                     error
		wantCalls                    int
	}{
		{"nil_function", "Change exceeds", noRoute, true, false, true, nil, 0},
		{"function_errors", "Change exceeds", "no escalation was raised: no overseer; raise it with gt escalate", true, false, false, errors.New("no overseer"), 1},
		{"crew_past_limit", "", "", false, false, false, nil, 0},
		{"count_unreadable", "added-line check", "", true, true, false, nil, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := pastLimitHarness(t, backendOpenShell, tt.polecat, tt.countFail)
			calls := 0
			if !tt.nilRaise {
				h.r.raiseSizeRefusal = func(string, string) error { calls++; return tt.raiseErr }
			}
			err := checkBranchForSubmit(h.r, doneSubmission{sourceIssue: &beads.Issue{Description: "Change: at most 70 added lines in 2 files."}}, "origin/main")
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("error = %v, want one containing %q", err, tt.wantErr)
			}
			out := h.r.out.(*bytes.Buffer).String()
			if calls != tt.wantCalls || tt.wantLine == "" && strings.Contains(out, "escalat") || tt.wantLine != "" && !strings.Contains(out, tt.wantLine+"\n") {
				t.Errorf("calls = %d, output %q; want %d calls and the line %q", calls, out, tt.wantCalls, tt.wantLine)
			}
		})
	}
}
