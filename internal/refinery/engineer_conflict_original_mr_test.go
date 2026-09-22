package refinery

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// conflictOriginalMRDescription is the metadata block
// conflictTaskDescription writes, whose "- Key: value" lines
// conflictTaskMetadata parses back out.
const conflictOriginalMRDescription = "Resolve merge conflicts for branch polecat/amethyst/gt-znj8+mu3yr9vx\n" +
	"\n## Metadata\n" +
	"- Original MR: gt-wisp-0jy\n" +
	"- Branch: polecat/amethyst/gt-znj8+mu3yr9vx\n" +
	"- Conflict with: main@e4faf78f\n" +
	"- Original issue: gt-znj8\n" +
	"- Retry count: 1\n"

// Task title as createConflictResolutionTaskForMR builds it — the constant plus
// whatever the source issue's title was.
func conflictTaskTitle(original string) string {
	return ConflictTaskTitlePrefix + original
}

// TestConflictTaskOriginalMR covers the reverse lookup gt-rv8h needs: given the
// task bead a conflict-resolution polecat has just finished, name the MR it was
// created for.
func TestConflictTaskOriginalMR(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		issue *beads.Issue
		want  string
	}{
		{
			name: "conflict task names its MR",
			issue: &beads.Issue{
				ID:          "gt-l5dy",
				Title:       conflictTaskTitle("main_branch_test runner ignores merge_queue.setup_command"),
				Description: conflictOriginalMRDescription,
			},
			want: "gt-wisp-0jy",
		},
		{
			name:  "nil issue",
			issue: nil,
			want:  "",
		},
		{
			// The metadata line alone is not the signature: the parser matches
			// any colon-bearing line, and polecat-molecule attachment metadata
			// carries prose with colons.
			name: "metadata without the title prefix is not a conflict task",
			issue: &beads.Issue{
				ID:          "gt-other",
				Title:       "Investigate merge queue stalls",
				Description: conflictOriginalMRDescription,
			},
			want: "",
		},
		{
			// The consolidation-conflict tasks mol-refinery-patrol creates share
			// the words but block an epic, not an MR — waking a refinery for one
			// would name an epic id as an MR.
			name: "consolidation conflict task is not an MR conflict task",
			issue: &beads.Issue{
				ID:          "gt-cons",
				Title:       "Resolve consolidation conflicts: epic title (gt-epic-1)",
				Description: conflictOriginalMRDescription,
			},
			want: "",
		},
		{
			name: "title prefix without the metadata",
			issue: &beads.Issue{
				ID:          "gt-nometa",
				Title:       conflictTaskTitle("something"),
				Description: "Resolve merge conflicts for branch b\n\n## Metadata\n- Branch: b\n",
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ConflictTaskOriginalMR(tt.issue); got != tt.want {
				t.Errorf("ConflictTaskOriginalMR = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestConflictTaskOriginalMR_MatchesDescriptionWriter pins the two halves
// together: the value read back must be the MR id conflictTaskDescription was
// given. Without this, a change to either the description writer or the metadata
// parser could silently break the lookup while both sides still "look" right.
func TestConflictTaskOriginalMR_MatchesDescriptionWriter(t *testing.T) {
	t.Parallel()

	mr := &MRInfo{ID: "gt-wisp-0jy", SourceIssue: "gt-znj8", Priority: 2}
	task := &beads.Issue{
		ID:          "gt-l5dy",
		Title:       conflictTaskTitle("main_branch_test runner ignores merge_queue.setup_command"),
		Description: conflictTaskDescription(mr, "polecat/amethyst/gt-znj8+mu3yr9vx", "main", "e4faf78f", 1),
	}

	if got := ConflictTaskOriginalMR(task); got != mr.ID {
		t.Errorf("ConflictTaskOriginalMR = %q, want %q", got, mr.ID)
	}
	// The relation is symmetric with the forward check the refinery uses to
	// verify a task belongs to an MR.
	if !isConflictTaskForMR(task, mr.ID, mr.SourceIssue) {
		t.Errorf("task written for %s does not verify as a conflict task for it", mr.ID)
	}
	// And the title gate really is the prefix the writer uses.
	if !strings.HasPrefix(task.Title, ConflictTaskTitlePrefix) {
		t.Errorf("task title %q does not carry ConflictTaskTitlePrefix %q",
			task.Title, ConflictTaskTitlePrefix)
	}
}
