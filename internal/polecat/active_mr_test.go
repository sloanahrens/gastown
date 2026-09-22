package polecat

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

type fakeActiveMRReader struct {
	issues map[string]*beads.Issue
	errs   map[string]error
}

func (f fakeActiveMRReader) Show(issueID string) (*beads.Issue, error) {
	if err := f.errs[issueID]; err != nil {
		return nil, err
	}
	issue, ok := f.issues[issueID]
	if !ok {
		return nil, beads.ErrNotFound
	}
	return issue, nil
}

func TestAssessActiveMR(t *testing.T) {
	reader := fakeActiveMRReader{issues: map[string]*beads.Issue{
		"mr-open":        &beads.Issue{ID: "mr-open", Status: "open"},
		"mr-closed":      &beads.Issue{ID: "mr-closed", Status: "closed"},
		"mr-with-source": &beads.Issue{ID: "mr-with-source", Status: "closed", Description: "source_issue: gt-closed\n"},
		"gt-closed":      &beads.Issue{ID: "gt-closed", Status: "closed"},
		"gt-open":        &beads.Issue{ID: "gt-open", Status: "open"},
	}}

	tests := []struct {
		name       string
		reader     IssueReader
		input      ActiveMRInput
		wantPend   bool
		wantSource string
	}{
		{name: "empty active MR is not pending", reader: reader, input: ActiveMRInput{}, wantPend: false},
		{name: "open MR is pending", reader: reader, input: ActiveMRInput{ActiveMR: "mr-open", SourceIssueHint: "gt-closed"}, wantPend: true},
		{name: "closed MR with terminal source is stale", reader: reader, input: ActiveMRInput{ActiveMR: "mr-closed", SourceIssueHint: "gt-closed"}, wantPend: false, wantSource: "gt-closed"},
		{name: "closed MR with unknown source is pending", reader: reader, input: ActiveMRInput{ActiveMR: "mr-closed"}, wantPend: true},
		{name: "closed MR with open source is pending", reader: reader, input: ActiveMRInput{ActiveMR: "mr-closed", SourceIssueHint: "gt-open"}, wantPend: true, wantSource: "gt-open"},
		{name: "missing MR with terminal source is stale", reader: reader, input: ActiveMRInput{ActiveMR: "mr-missing", SourceIssueHint: "gt-closed"}, wantPend: false, wantSource: "gt-closed"},
		{name: "missing MR with missing source is pending", reader: reader, input: ActiveMRInput{ActiveMR: "mr-missing", SourceIssueHint: "gt-missing"}, wantPend: true, wantSource: "gt-missing"},
		{name: "terminal MR source wins from description", reader: reader, input: ActiveMRInput{ActiveMR: "mr-with-source"}, wantPend: false, wantSource: "gt-closed"},
		{name: "nil reader fails closed", reader: nil, input: ActiveMRInput{ActiveMR: "mr-closed", SourceIssueHint: "gt-closed"}, wantPend: true},
		{name: "git unsafe fails closed when required", reader: reader, input: ActiveMRInput{ActiveMR: "mr-closed", SourceIssueHint: "gt-closed", RequireGitSafe: true}, wantPend: true, wantSource: "gt-closed"},
		{name: "git safe permits stale when required", reader: reader, input: ActiveMRInput{ActiveMR: "mr-closed", SourceIssueHint: "gt-closed", RequireGitSafe: true, GitSafe: true}, wantPend: false, wantSource: "gt-closed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AssessActiveMR(tt.reader, tt.input)
			if got.Pending != tt.wantPend {
				t.Fatalf("Pending = %v, want %v (reason %q)", got.Pending, tt.wantPend, got.Reason)
			}
			if tt.wantSource != "" && got.SourceIssue != tt.wantSource {
				t.Fatalf("SourceIssue = %q, want %q", got.SourceIssue, tt.wantSource)
			}
		})
	}
}

// TestAssessActiveMRWithLandedEvidence covers both branches of the
// dangling-pointer gate (gt-wprt): a gone-or-closed MR frees its polecat only
// when the work it carried is independently proven to be on origin/main, and a
// live MR is decided without ever consulting that evidence.
func TestAssessActiveMRWithLandedEvidence(t *testing.T) {
	reader := fakeActiveMRReader{issues: map[string]*beads.Issue{
		"mr-open":     {ID: "mr-open", Status: "open"},
		"mr-rejected": {ID: "mr-rejected", Status: "closed"},
		"gt-closed":   {ID: "gt-closed", Status: "closed"},
		"gt-open":     {ID: "gt-open", Status: "open"},
	}}

	landed := func() (LandedEvidenceProbe, *int) {
		calls := 0
		return func() LandedEvidence {
			calls++
			return LandedEvidence{Verified: true, Ref: "origin/main"}
		}, &calls
	}

	tests := []struct {
		name         string
		input        ActiveMRInput
		landed       bool
		nilProbe     bool
		wantPending  bool
		wantProbeRun int
		wantReason   string
	}{
		{
			name:         "dangling MR with landed work is free",
			input:        ActiveMRInput{ActiveMR: "mr-gone", SourceIssueHint: "gt-open"},
			landed:       true,
			wantPending:  false,
			wantProbeRun: 1,
		},
		{
			name:         "dangling MR without landed work still blocks",
			input:        ActiveMRInput{ActiveMR: "mr-gone", SourceIssueHint: "gt-open"},
			wantPending:  true,
			wantProbeRun: 1,
			wantReason:   "source_status=open",
		},
		{
			name:         "dangling MR with no source hint is freed by landed work",
			input:        ActiveMRInput{ActiveMR: "mr-gone"},
			landed:       true,
			wantPending:  false,
			wantProbeRun: 1,
		},
		{
			name:         "live MR blocks without measuring landed work",
			input:        ActiveMRInput{ActiveMR: "mr-open", SourceIssueHint: "gt-closed"},
			landed:       true,
			wantPending:  true,
			wantProbeRun: 0,
		},
		{
			name:         "terminal source clears the MR before the probe is needed",
			input:        ActiveMRInput{ActiveMR: "mr-gone", SourceIssueHint: "gt-closed"},
			landed:       true,
			wantPending:  false,
			wantProbeRun: 0,
		},
		{
			name:         "rejected MR with landed work is free",
			input:        ActiveMRInput{ActiveMR: "mr-rejected", SourceIssueHint: "gt-open"},
			landed:       true,
			wantPending:  false,
			wantProbeRun: 1,
		},
		{
			name:         "rejected MR without landed work still blocks",
			input:        ActiveMRInput{ActiveMR: "mr-rejected", SourceIssueHint: "gt-open"},
			wantPending:  true,
			wantProbeRun: 1,
			wantReason:   "source_status=open",
		},
		{
			name:         "landed work stands in for git safety",
			input:        ActiveMRInput{ActiveMR: "mr-gone", SourceIssueHint: "gt-open", RequireGitSafe: true},
			landed:       true,
			wantPending:  false,
			wantProbeRun: 1,
		},
		{
			// A terminal source issue is not enough on its own when the caller
			// demands direct git safety and the work cannot be shown to have
			// landed: that is the pre-existing fail-closed gate, unchanged.
			name:         "git unsafe without landed work blocks",
			input:        ActiveMRInput{ActiveMR: "mr-gone", SourceIssueHint: "gt-closed", RequireGitSafe: true},
			wantPending:  true,
			wantProbeRun: 1,
			wantReason:   "git_state=unsafe",
		},
		{
			name:         "missing probe fails closed",
			input:        ActiveMRInput{ActiveMR: "mr-gone", SourceIssueHint: "gt-open"},
			nilProbe:     true,
			wantPending:  true,
			wantProbeRun: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe, calls := landed()
			if !tt.landed {
				probe = func() LandedEvidence { *calls++; return LandedEvidence{} }
			}
			if tt.nilProbe {
				probe = nil
			}

			got := AssessActiveMRWithLandedEvidence(reader, tt.input, probe)
			if got.Pending != tt.wantPending {
				t.Fatalf("Pending = %v, want %v (reason %q)", got.Pending, tt.wantPending, got.Reason)
			}
			if *calls != tt.wantProbeRun {
				t.Fatalf("landed probe ran %d times, want %d", *calls, tt.wantProbeRun)
			}
			if tt.wantReason != "" && !strings.Contains(got.Reason, tt.wantReason) {
				t.Fatalf("Reason = %q, want contains %q", got.Reason, tt.wantReason)
			}
			if !tt.wantPending && got.Reason != "" {
				t.Fatalf("Reason = %q, want empty on a freed MR", got.Reason)
			}
			wantLanded := tt.landed && tt.wantProbeRun == 1
			if got.WorkLandedOnMain != wantLanded {
				t.Fatalf("WorkLandedOnMain = %v, want %v", got.WorkLandedOnMain, wantLanded)
			}
			if wantLanded && got.WorkLandedRef != "origin/main" {
				t.Fatalf("WorkLandedRef = %q, want origin/main", got.WorkLandedRef)
			}
		})
	}
}

// TestAssessActiveMRWithLandedEvidenceLookupErrorDoesNotProbe pins the fail-
// closed boundary: a lookup that *failed* is not a stale MR, so an unreadable
// queue never reaches the landed probe and never frees a slot on evidence it
// did not have. A closed MR whose *source* cannot be read is a different case:
// the MR itself is verifiably closed, so landed work is allowed to settle it.
func TestAssessActiveMRWithLandedEvidenceLookupErrorDoesNotProbe(t *testing.T) {
	reader := fakeActiveMRReader{
		issues: map[string]*beads.Issue{"gt-closed": {ID: "gt-closed", Status: "closed"}},
		errs:   map[string]error{"mr-error": errors.New("bd exploded")},
	}
	calls := 0
	probe := func() LandedEvidence { calls++; return LandedEvidence{Verified: true, Ref: "origin/main"} }

	got := AssessActiveMRWithLandedEvidence(reader, ActiveMRInput{ActiveMR: "mr-error", SourceIssueHint: "gt-closed"}, probe)
	if !got.Pending {
		t.Fatalf("Pending = false, want true for an unreadable MR")
	}
	if calls != 0 {
		t.Fatalf("landed probe ran %d times, want 0 for an unreadable MR", calls)
	}

	reader.issues["mr-closed"] = &beads.Issue{ID: "mr-closed", Status: "closed"}
	got = AssessActiveMRWithLandedEvidence(reader, ActiveMRInput{ActiveMR: "mr-closed", SourceIssueHint: "gt-error"}, probe)
	if got.Pending {
		t.Fatalf("Pending = true, want false: the MR is verifiably closed and the work landed")
	}
	if calls != 1 {
		t.Fatalf("landed probe ran %d times, want 1", calls)
	}
}

func TestAssessActiveMRLookupErrorsFailClosed(t *testing.T) {
	reader := fakeActiveMRReader{
		issues: map[string]*beads.Issue{"gt-closed": &beads.Issue{ID: "gt-closed", Status: "closed"}},
		errs:   map[string]error{"mr-error": errors.New("bd exploded"), "gt-error": errors.New("bd exploded")},
	}

	if got := AssessActiveMR(reader, ActiveMRInput{ActiveMR: "mr-error", SourceIssueHint: "gt-closed"}); !got.Pending {
		t.Fatalf("MR lookup error Pending = false, want true")
	}
	reader.issues["mr-closed"] = &beads.Issue{ID: "mr-closed", Status: "closed"}
	if got := AssessActiveMR(reader, ActiveMRInput{ActiveMR: "mr-closed", SourceIssueHint: "gt-error"}); !got.Pending {
		t.Fatalf("source lookup error Pending = false, want true")
	}
}

func TestActiveMRRemovalBlockerUsesActiveMRPolicy(t *testing.T) {
	reader := fakeActiveMRReader{issues: map[string]*beads.Issue{
		"mr-open":     {ID: "mr-open", Status: "open"},
		"mr-progress": {ID: "mr-progress", Status: "in_progress"},
		"mr-closed":   {ID: "mr-closed", Status: "closed"},
		"gt-closed":   {ID: "gt-closed", Status: "closed"},
		"gt-open":     {ID: "gt-open", Status: "open"},
	}}

	tests := []struct {
		name       string
		fields     *beads.AgentFields
		wantBlock  bool
		wantReason string
	}{
		{name: "empty fields are safe"},
		{name: "open MR blocks", fields: &beads.AgentFields{ActiveMR: "mr-open", LastSourceIssue: "gt-closed"}, wantBlock: true, wantReason: "status=open"},
		{name: "in-progress MR blocks", fields: &beads.AgentFields{ActiveMR: "mr-progress", LastSourceIssue: "gt-closed"}, wantBlock: true, wantReason: "status=in_progress"},
		{name: "closed MR with terminal source is safe", fields: &beads.AgentFields{ActiveMR: "mr-closed", LastSourceIssue: "gt-closed"}},
		{name: "closed MR with open source blocks", fields: &beads.AgentFields{ActiveMR: "mr-closed", LastSourceIssue: "gt-open"}, wantBlock: true, wantReason: "source_status=open"},
		{name: "closed MR uses hook as source hint", fields: &beads.AgentFields{ActiveMR: "mr-closed", HookBead: "gt-closed"}},
		{name: "missing MR without terminal source blocks", fields: &beads.AgentFields{ActiveMR: "mr-missing"}, wantBlock: true, wantReason: "source_issue=<missing>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := activeMRRemovalBlocker(reader, tt.fields)
			if tt.wantBlock && got == "" {
				t.Fatal("activeMRRemovalBlocker() = empty, want blocker")
			}
			if !tt.wantBlock && got != "" {
				t.Fatalf("activeMRRemovalBlocker() = %q, want empty", got)
			}
			if tt.wantReason != "" && !strings.Contains(got, tt.wantReason) {
				t.Fatalf("blocker = %q, want contains %q", got, tt.wantReason)
			}
		})
	}
}

func TestActiveMRRemovalBlockerLookupErrorsFailClosed(t *testing.T) {
	reader := fakeActiveMRReader{
		issues: map[string]*beads.Issue{"gt-closed": {ID: "gt-closed", Status: "closed"}},
		errs:   map[string]error{"mr-error": errors.New("bd exploded")},
	}

	got := activeMRRemovalBlocker(reader, &beads.AgentFields{ActiveMR: "mr-error", LastSourceIssue: "gt-closed"})
	if got == "" || !strings.Contains(got, "lookup_error") {
		t.Fatalf("blocker = %q, want lookup_error fail-closed", got)
	}
}
