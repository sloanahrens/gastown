package done

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/rig"
)

func TestFollowupBudget(t *testing.T) {
	t.Parallel()
	const maxAdded, maxFiles = 80, 3
	followup := &beads.Issue{Labels: []string{land.FollowUpLabel}}
	tests := []struct {
		name     string
		issue    *beads.Issue
		added    int
		files    int
		wantErr  bool
		wantText []string
	}{
		{"at the limit passes", followup, 80, 3, false, nil},
		{"one line over refuses", followup, 81, 1, true, []string{
			"81 lines", "over the diff budget of a follow-up bead",
			"80 added lines", "3 files", "gt done --status ESCALATED",
			"instead of expanding the change",
		}},
		{"one file over refuses", followup, 20, 4, true, []string{
			"20 lines", "4 file(s)", "80 added lines", "3 files",
			"gt done --status ESCALATED",
		}},
		{"unlabelled bead passes", &beads.Issue{Labels: []string{"other"}}, 800, 30, false, nil},
		{"nil issue passes", nil, 800, 30, false, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := followupBudgetError(tc.issue, tc.added, tc.files, maxAdded, maxFiles)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("followupBudgetError() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("followupBudgetError() = nil, want a refusal")
			}
			for _, want := range tc.wantText {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not contain %q", err, want)
				}
			}
		})
	}
}

// TestFollowupBudgetRefusesOverBudgetSubmission drives the budget through
// checkBranchForSubmit: a labelled bead over either limit is refused, an
// unlabelled bead never reads the diff, the rig config's raised limit lets a
// larger change submit, and a failed git read refuses naming it (gt-c9cp26).
func TestFollowupBudgetRefusesOverBudgetSubmission(t *testing.T) {
	t.Parallel()
	labelled := &beads.Issue{Labels: []string{land.FollowUpLabel}}
	tests := []struct {
		name     string
		issue    *beads.Issue
		numstat  string
		maxAdded int // rig config's followup_max_added_lines; 0 leaves it unset
		errAt    string
		gitErr   error
		want     []string
	}{
		{"over_added_lines", labelled, "81\t0\ta.go\n", 0, "", nil, []string{"81 lines", "gt done --status ESCALATED"}},
		{"over_files", labelled, "20\t0\ta.go\n0\t0\tb.go\n0\t0\tc.go\n0\t0\td.go\n", 0, "", nil, []string{"4 file(s)"}},
		{"unlabelled_never_reads", &beads.Issue{Labels: []string{"other"}}, "", 0, "", nil, nil},
		{"unparsable_numstat", labelled, "not a numstat line\n", 0, "", nil, []string{"cannot check the follow-up bead's diff budget"}},
		{"config_raises_the_limit", labelled, "150\t0\ta.go\n", 200, "", nil, nil},
		{"merge_base_fails", labelled, "", 0, "merge-base", errors.New("no merge base"), []string{"merge base", "no merge base"}},
		{"numstat_fails", labelled, "", 0, "diff", errors.New("bad revision"), []string{"git diff --numstat", "bad revision"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := git.NewGitWithExec(t.TempDir(), func(c git.ExecCall) (string, string, error) {
				args := c.Args()
				switch {
				case tt.gitErr != nil && args[0] == tt.errAt:
					return "", "", tt.gitErr
				case args[0] == "merge-base":
					return "base000\n", "", nil
				case args[0] == "diff":
					return tt.numstat, "", nil
				}
				return "", "", fmt.Errorf("unexpected git call: git %s", strings.Join(args, " "))
			})
			r := &doneRun{g: g, out: new(bytes.Buffer),
				opts: doneOptions{polecatEnv: true, allowReverts: true, allowThrowawayPaths: true}}
			if tt.maxAdded > 0 {
				dir := t.TempDir()
				cfg := fmt.Sprintf(`{"type":"rig","version":1,"name":"gastown","merge_queue":{"followup_max_added_lines":%d}}`, tt.maxAdded)
				if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
					t.Fatal(err)
				}
				loaded, err := rig.LoadRigConfigIfPresent(dir)
				if err != nil || loaded == nil {
					t.Fatalf("loading the rig config: %v, %v", loaded, err)
				}
				r.rigCfg = loaded
			}
			err := checkBranchForSubmit(r, doneSubmission{sourceIssue: tt.issue}, "origin/main")
			if (err == nil) != (len(tt.want) == 0) {
				t.Fatalf("checkBranchForSubmit() = %v, want refusal=%v", err, len(tt.want) > 0)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not contain %q", err, want)
				}
			}
		})
	}
}
