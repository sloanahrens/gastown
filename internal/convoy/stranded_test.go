package convoy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsReadyIssue_BlockingAndStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   TrackedIssue
		want bool
	}{
		{
			name: "closed issue never ready",
			in: TrackedIssue{
				Status:  "closed",
				Blocked: false,
			},
			want: false,
		},
		{
			name: "unknown issue never ready",
			in: TrackedIssue{
				Status:  TrackedStatusUnknown,
				Blocked: false,
			},
			want: false,
		},
		{
			name: "blank status never ready",
			in: TrackedIssue{
				Status:  " ",
				Blocked: false,
			},
			want: false,
		},
		{
			// bd show's blocked flag drops cross-rig blockers, so readiness
			// does not read it: the stranded scan asks BlockReason
			// instead (gt-j02xy, TestFindStrandedConvoys_CrossRigBlockerNotReady).
			name: "bd show blocked flag not consulted",
			in: TrackedIssue{
				Status:  "open",
				Blocked: true,
			},
			want: true,
		},
		{
			name: "open unassigned issue ready",
			in: TrackedIssue{
				Status:  "open",
				Blocked: false,
			},
			want: true,
		},
		{
			name: "non-open unassigned issue treated ready for recovery",
			in: TrackedIssue{
				Status:  "in_progress",
				Blocked: false,
			},
			want: true,
		},
		{
			// The orphaned-molecule recovery case for the hook status too.
			name: "hooked unassigned issue treated ready for recovery",
			in:   TrackedIssue{Status: "hooked"},
			want: true,
		},
		{
			// Submitted for landing: the landing worker owns it (gt-v4ssj.2).
			name: "ready-to-land issue never stranded",
			in:   TrackedIssue{Status: "hooked", Labels: []string{"gt:ready-to-land"}},
			want: false,
		},
		// Readiness is an allowlist (gt-t08jn): every status the tracker says
		// is not ready work stays off the feeders, assigned or not.
		{
			name: "blocked-status unassigned issue not ready",
			in:   TrackedIssue{Status: "blocked"},
			want: false,
		},
		{
			name: "deferred unassigned issue not ready",
			in:   TrackedIssue{Status: "deferred"},
			want: false,
		},
		{
			name: "pinned unassigned issue not ready",
			in:   TrackedIssue{Status: "pinned"},
			want: false,
		},
		{
			name: "tombstone issue not ready",
			in:   TrackedIssue{Status: "tombstone"},
			want: false,
		},
		{
			name: "custom status unassigned issue not ready",
			in:   TrackedIssue{Status: "review"},
			want: false,
		},
		{
			// Not ready before any session lookup: a frozen status is not
			// made ready by its holder being gone.
			name: "deferred assigned issue not ready",
			in:   TrackedIssue{Status: "deferred", Assignee: "gastown/polecats/gone"},
			want: false,
		},
		{
			name: "scheduled open issue not ready",
			in:   TrackedIssue{ID: "gt-sched", Status: "open"},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isReadyIssue(nil, tc.in, map[string]bool{"gt-sched": true})
			if got != tc.want {
				t.Fatalf("isReadyIssue() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyFreshIssueDetails_SetsBlockedFlag(t *testing.T) {
	t.Parallel()
	dep := trackedDependency{
		ID:     "gt-123",
		Status: "open",
	}
	details := &IssueDetails{
		ID:             "gt-123",
		Status:         "open",
		BlockedByCount: 1,
	}

	applyFreshIssueDetails(&dep, details)

	if !dep.Blocked {
		t.Fatalf("applyFreshIssueDetails() should set Blocked=true when details are blocked")
	}
}

func TestApplyFreshIssueDetails_BlankStatusBecomesUnknown(t *testing.T) {
	t.Parallel()
	dep := trackedDependency{ID: "gt-123"}
	details := &IssueDetails{ID: "gt-123", Status: "  "}

	applyFreshIssueDetails(&dep, details)

	if dep.Status != TrackedStatusUnknown {
		t.Fatalf("dep.Status = %q, want %q", dep.Status, TrackedStatusUnknown)
	}
}

func TestIssueDetailsIsBlocked(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   IssueDetails
		want bool
	}{
		{
			name: "blocked_by_count marks blocked",
			in: IssueDetails{
				BlockedByCount: 2,
			},
			want: true,
		},
		{
			name: "blocked_by list marks blocked",
			in: IssueDetails{
				BlockedBy: []string{"gt-1"},
			},
			want: true,
		},
		{
			name: "open blocks dependency marks blocked",
			in: IssueDetails{
				Dependencies: []issueDependency{
					{DependencyType: "blocks", Status: "open"},
				},
			},
			want: true,
		},
		{
			name: "closed blocks dependency does not mark blocked",
			in: IssueDetails{
				Dependencies: []issueDependency{
					{DependencyType: "blocks", Status: "closed"},
				},
			},
			want: false,
		},
		{
			name: "non-blocking dependency does not mark blocked",
			in: IssueDetails{
				Dependencies: []issueDependency{
					{DependencyType: "parent-child", Status: "open"},
				},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.IsBlocked()
			if got != tc.want {
				t.Fatalf("IsBlocked() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsSlingableBead(t *testing.T) {
	t.Parallel()
	// Set up a fake town root with routes.jsonl
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "bd-", "path": "beads/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		beadID string
		want   bool
	}{
		{"rig bead is slingable", "gt-wisp-abc", true},
		{"another rig bead is slingable", "bd-wisp-xyz", true},
		{"town-level bead not slingable", "hq-wisp-abc", false},
		{"town-level convoy not slingable", "hq-cv-kl6ns", false},
		{"unknown prefix not slingable", "zz-wisp-abc", false},
		{"no prefix assumes slingable", "nohyphen", true},
		{"empty ID assumes slingable", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isSlingableBead(townRoot, tc.beadID)
			if got != tc.want {
				t.Fatalf("isSlingableBead(%q) = %v, want %v", tc.beadID, got, tc.want)
			}
		})
	}
}
