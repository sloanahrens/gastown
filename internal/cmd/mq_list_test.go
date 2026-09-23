package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestMqListDisplayStatus_DuplicateBranchWinsOverEveryOtherState is the
// gt-k1qf fix for the single-MR patrol path: `gt mq list` (queue-scan's
// source of truth) must mark a duplicate-branch MR as "duplicate", not
// "ready" or "blocked", or process-branch will still gate it.
func TestMqListDisplayStatus_DuplicateBranchWinsOverEveryOtherState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		issue           *beads.Issue
		alreadyLanded   bool
		duplicateBranch bool
		want            string
	}{
		{
			name:  "ordinary ready MR",
			issue: &beads.Issue{Status: "open"},
			want:  "ready",
		},
		{
			name:  "blocked MR",
			issue: &beads.Issue{Status: "open", BlockedByCount: 1},
			want:  "blocked",
		},
		{
			name:          "already landed MR",
			issue:         &beads.Issue{Status: "open"},
			alreadyLanded: true,
			want:          "landed",
		},
		{
			name:            "duplicate branch beats blocked",
			issue:           &beads.Issue{Status: "open", BlockedByCount: 1},
			duplicateBranch: true,
			want:            "duplicate",
		},
		{
			name:            "duplicate branch beats landed",
			issue:           &beads.Issue{Status: "open"},
			alreadyLanded:   true,
			duplicateBranch: true,
			want:            "duplicate",
		},
		{
			name:  "closed MR keeps its raw status",
			issue: &beads.Issue{Status: "closed"},
			want:  "closed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mqListDisplayStatus(tt.issue, tt.alreadyLanded, tt.duplicateBranch); got != tt.want {
				t.Fatalf("mqListDisplayStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildMQListColumns_IncludesTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		verify        bool
		wantColumnSeq []string
	}{
		{
			name:   "without verify",
			verify: false,
			wantColumnSeq: []string{
				"ID", "SCORE", "PRI", "CONVOY", "BRANCH", "TARGET", "STATUS", "AGE",
			},
		},
		{
			name:   "with verify",
			verify: true,
			wantColumnSeq: []string{
				"ID", "SCORE", "PRI", "CONVOY", "BRANCH", "TARGET", "STATUS", "GIT", "AGE",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cols := buildMQListColumns(tt.verify)
			if len(cols) != len(tt.wantColumnSeq) {
				t.Fatalf("len(columns) = %d, want %d", len(cols), len(tt.wantColumnSeq))
			}
			for i, want := range tt.wantColumnSeq {
				if cols[i].Name != want {
					t.Fatalf("column[%d] = %q, want %q", i, cols[i].Name, want)
				}
			}
		})
	}
}
