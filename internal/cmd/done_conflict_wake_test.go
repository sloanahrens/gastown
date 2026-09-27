package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// conflictWakeStore is a minimal in-memory bead store for the blocked->ready
// transition. The MR's blocker list is the live state the decision reads, so
// closing the conflict task is expressed by flipping the dependency's status —
// the same move beads makes when `bd close` runs on the task (isResolvedDependency
// treats a closed dep as resolved).
type conflictWakeStore struct {
	beads map[string]*beads.Issue
}

func (s *conflictWakeStore) show(id string) (*beads.Issue, error) {
	issue, ok := s.beads[id]
	if !ok {
		return nil, beads.ErrNotFound
	}
	return issue, nil
}

// closeConflictTask is the blocked->ready transition: the conflict task goes
// terminal and the MR's dependency on it resolves in the same step.
func (s *conflictWakeStore) closeConflictTask(taskID string) {
	task := s.beads[taskID]
	task.Status = string(beads.StatusClosed)
	for _, mr := range s.beads {
		for i := range mr.Dependencies {
			if mr.Dependencies[i].ID == taskID {
				mr.Dependencies[i].Status = string(beads.StatusClosed)
			}
		}
	}
}

// conflictTaskDescription mirrors the metadata block
// refinery.createConflictResolutionTaskForMR writes, including the trailing
// colon-bearing prose the metadata parser also walks.
func conflictTaskDescription(mrID string) string {
	return "Resolve merge conflicts for branch polecat/amethyst/gt-znj8+mu3yr9vx\n" +
		"\n## Metadata\n" +
		"- Original MR: " + mrID + "\n" +
		"- Branch: polecat/amethyst/gt-znj8+mu3yr9vx\n" +
		"- Conflict with: main@e4faf78f\n" +
		"- Original issue: gt-znj8\n" +
		"- Retry count: 1\n" +
		"\n## Instructions\n" +
		"1. Check out the branch\n" +
		"6. Close this task: bd close <this-task-id>\n"
}

// newConflictWakeStore builds the gt-rv8h shape: an open MR that is blocked on an
// open conflict-resolution task. This is the state before the transition, in
// which the MR is invisible to the refinery's ready list.
func newConflictWakeStore() *conflictWakeStore {
	const (
		mrID   = "gt-wisp-0jy"
		taskID = "gt-l5dy"
	)
	return &conflictWakeStore{beads: map[string]*beads.Issue{
		taskID: {
			ID:          taskID,
			Title:       "Resolve merge conflicts: main_branch_test runner ignores merge_queue.setup_command",
			Status:      string(beads.StatusOpen),
			Description: conflictTaskDescription(mrID),
		},
		mrID: {
			ID:        mrID,
			Title:     "MR: main_branch_test runner ignores merge_queue.setup_command",
			Status:    string(beads.StatusOpen),
			BlockedBy: []string{taskID},
			Dependencies: []beads.IssueDep{{
				ID:             taskID,
				Status:         string(beads.StatusOpen),
				DependencyType: "blocks",
			}},
		},
	}}
}

// TestReadyConflictResolvedMR_ForcedBlockedToReadyTransition walks the exact
// transition gt-rv8h reported: while the conflict task is open the MR is blocked
// and must not be announced, and the moment the task closes the same call must
// name the MR. The pre-transition half is what makes this a transition test
// rather than a "kind of bead" test — a detector that always returned the MR
// would fail it.
func TestReadyConflictResolvedMR_ForcedBlockedToReadyTransition(t *testing.T) {
	t.Parallel()
	const (
		mrID   = "gt-wisp-0jy"
		taskID = "gt-l5dy"
	)

	store := newConflictWakeStore()

	if got := readyConflictResolvedMR(store.show, taskID); got != "" {
		t.Fatalf("MR still blocked by open task %s: readyConflictResolvedMR = %q, want no wake", taskID, got)
	}

	store.closeConflictTask(taskID)

	if got := readyConflictResolvedMR(store.show, taskID); got != mrID {
		t.Fatalf("after %s closed: readyConflictResolvedMR = %q, want %q", taskID, got, mrID)
	}
}

// TestReadyConflictResolvedMR_Gates covers the completions that must NOT wake the
// refinery, so the detector cannot become a nudge storm on ordinary work.
func TestReadyConflictResolvedMR_Gates(t *testing.T) {
	t.Parallel()
	const taskID = "gt-l5dy"

	tests := []struct {
		name   string
		mutate func(*conflictWakeStore)
		want   string
	}{
		{
			name:   "no task id",
			mutate: func(*conflictWakeStore) {},
			want:   "",
		},
		{
			name: "task still open leaves the MR blocked",
			mutate: func(s *conflictWakeStore) {
				s.beads[taskID].Status = string(beads.StatusOpen)
			},
			want: "",
		},
		{
			name: "task closed but a second blocker still open",
			mutate: func(s *conflictWakeStore) {
				s.beads["gt-wisp-0jy"].Dependencies = append(
					s.beads["gt-wisp-0jy"].Dependencies,
					beads.IssueDep{ID: "gt-other", Status: string(beads.StatusOpen), DependencyType: "blocks"})
			},
			want: "",
		},
		{
			name: "MR already merged",
			mutate: func(s *conflictWakeStore) {
				s.beads["gt-wisp-0jy"].Status = string(beads.StatusClosed)
			},
			want: "",
		},
		{
			name: "task missing its Original MR metadata",
			mutate: func(s *conflictWakeStore) {
				s.beads[taskID].Description = "Resolve merge conflicts for branch b\n\n## Metadata\n- Branch: b\n"
			},
			want: "",
		},
		{
			name: "non-conflict bead that mentions Original MR in prose",
			mutate: func(s *conflictWakeStore) {
				s.beads[taskID].Title = "Unrelated task"
			},
			want: "",
		},
		{
			name: "MR bead itself is not a conflict task",
			mutate: func(s *conflictWakeStore) {
				s.beads[taskID].Title = "Resolve consolidation conflicts: epic title"
			},
			want: "",
		},
		{
			name:   "unknown task id",
			mutate: func(*conflictWakeStore) {},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newConflictWakeStore()
			store.closeConflictTask(taskID)
			tt.mutate(store)

			id := taskID
			if tt.name == "no task id" {
				id = "   "
			}
			if tt.name == "unknown task id" {
				id = "gt-does-not-exist"
			}

			if got := readyConflictResolvedMR(store.show, id); got != tt.want {
				t.Errorf("readyConflictResolvedMR = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadyConflictResolvedMR_NilShower(t *testing.T) {
	t.Parallel()
	if got := readyConflictResolvedMR(nil, "gt-l5dy"); got != "" {
		t.Errorf("readyConflictResolvedMR(nil) = %q, want no wake", got)
	}
}

// TestWakeRefineryForReadyConflict_Nudges are the wake assertions: a completion
// that releases a blocked MR must reach the refinery, and its MR id must be in
// the message so the wake is actionable rather than "something changed".
//
// The assertion is on the nudge log rather than a tmux session because that log
// is the seam the rest of gt done's refinery wakes are tested through
// (GT_TEST_NUDGE_LOG — see sling_helpers_test.go); it is written by the same
// production path that emits the refinery channel event and the tmux nudge.
func TestWakeRefineryForReadyConflict_Nudges(t *testing.T) {
	const (
		mrID   = "gt-wisp-0jy"
		taskID = "gt-l5dy"
		rig    = "gastown"
	)

	logPath := filepath.Join(t.TempDir(), "nudge.log")
	t.Setenv("GT_TEST_NUDGE_LOG", logPath)

	store := newConflictWakeStore()
	if got := wakeRefineryForReadyConflict(store.show, rig, taskID); got != "" {
		t.Fatalf("blocked MR: wakeRefineryForReadyConflict = %q, want no wake", got)
	}
	if entries := readNudgeLog(t, logPath); len(entries) != 0 {
		t.Fatalf("blocked MR produced %d nudge(s): %v", len(entries), entries)
	}

	store.closeConflictTask(taskID)

	if got := wakeRefineryForReadyConflict(store.show, rig, taskID); got != mrID {
		t.Fatalf("wakeRefineryForReadyConflict = %q, want %q", got, mrID)
	}

	entries := readNudgeLog(t, logPath)
	if len(entries) != 1 {
		t.Fatalf("nudges after transition = %d (%v), want exactly 1", len(entries), entries)
	}
	if !strings.Contains(entries[0], "MERGE_READY "+mrID) {
		t.Errorf("nudge %q must name the released MR %s", entries[0], mrID)
	}

	// A second completion of the same task must be idempotent enough not to
	// double-nudge while the MR is still waiting: the refinery is asleep at its
	// prompt, so the wake is not consumed here and the MR is still ready.
	if got := wakeRefineryForReadyConflict(store.show, rig, taskID); got != mrID {
		t.Fatalf("repeat wake = %q, want %q (MR still ready)", got, mrID)
	}
}

// TestWakeRefineryForReadyConflict_FallsBackToHookedBead covers the completion
// whose branch does not name the conflict task. A conflict polecat that finishes
// while still on the resolved branch (polecat/<owner>/<source-issue>+<mol>) has a
// branch-derived issueID of the *source* issue, so gt done must also consider the
// agent bead's hook_bead — the bead the polecat was actually slung.
func TestWakeRefineryForReadyConflict_FallsBackToHookedBead(t *testing.T) {
	const (
		mrID      = "gt-wisp-0jy"
		taskID    = "gt-l5dy"
		sourceID  = "gt-znj8"
		rig       = "gastown"
		logSuffix = "nudge.log"
	)

	logPath := filepath.Join(t.TempDir(), logSuffix)
	t.Setenv("GT_TEST_NUDGE_LOG", logPath)

	store := newConflictWakeStore()
	store.beads[sourceID] = &beads.Issue{
		ID:     sourceID,
		Title:  "main_branch_test runner ignores merge_queue.setup_command",
		Status: string(beads.StatusClosed),
	}
	store.closeConflictTask(taskID)

	// First candidate is the branch-derived source issue, which is not a
	// conflict task; the second is the hooked bead, which is.
	if got := wakeRefineryForReadyConflict(store.show, rig, sourceID, taskID); got != mrID {
		t.Fatalf("wakeRefineryForReadyConflict(source, hooked) = %q, want %q", got, mrID)
	}

	entries := readNudgeLog(t, logPath)
	if len(entries) != 1 {
		t.Fatalf("nudges = %d (%v), want exactly 1", len(entries), entries)
	}
	if !strings.Contains(entries[0], "MERGE_READY "+mrID) {
		t.Errorf("nudge %q must name the released MR %s", entries[0], mrID)
	}
}

// TestWakeRefineryForReadyConflict_SkipsUnrelatedCompletion pins the blast
// radius: an ordinary completion (no conflict task on the hook) emits nothing.
func TestWakeRefineryForReadyConflict_SkipsUnrelatedCompletion(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "nudge.log")
	t.Setenv("GT_TEST_NUDGE_LOG", logPath)

	store := newConflictWakeStore()
	store.beads["gt-rv8h"] = &beads.Issue{
		ID:     "gt-rv8h",
		Title:  "refinery does not wake when a blocked MR returns to ready after conflict resolution",
		Status: string(beads.StatusClosed),
	}

	if got := wakeRefineryForReadyConflict(store.show, "gastown", "gt-rv8h"); got != "" {
		t.Errorf("ordinary completion woke the refinery for %q", got)
	}
	if entries := readNudgeLog(t, logPath); len(entries) != 0 {
		t.Errorf("ordinary completion produced nudges: %v", entries)
	}
}

// TestDoneWiresConflictWakeIntoNotifyWitness guards the failure mode this bead
// is actually about: gt-rv8h was not a detector that got the wrong answer, it was
// a transition nobody emitted for at all. A wake function that no completion
// path calls is exactly as silent as no function, and every other test in this
// file would still pass, so the call site itself is asserted here.
//
// The call must land after updateAgentStateAfterSubmission, not merely
// somewhere in notifyWitness: that call is what actually closes the hooked
// conflict task (via updateAgentStateOnDone), and checking readiness before
// the close always found the task still open — gt done could never wake the
// refinery for its own conflict-resolution completion (gt-ue2h).
//
// A source-level check is used because the alternative — driving runDone — needs
// a git remote, a beads database, and a tmux session. This deliberately asserts
// only that notifyWitness reaches the wake after the close, not how it is
// spelled around it.
func TestDoneWiresConflictWakeIntoNotifyWitness(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("done.go")
	if err != nil {
		t.Fatalf("reading done.go: %v", err)
	}
	doneSrc := string(src)

	// Scope the search to the notifyWitness block: gt done has several early
	// gotos to it, and a call placed anywhere else would not run for the
	// completions this fixes. The block runs to the next top-level func decl
	// (the end of runDone) rather than cutting at "Notifying Witness..." —
	// the wake call must land after the state-update close, which happens
	// later in the same block.
	label := strings.Index(doneSrc, "notifyWitness:")
	if label < 0 {
		t.Fatal("done.go has no notifyWitness block — the refinery wake site moved")
	}
	block := doneSrc[label:]
	if end := strings.Index(block, "\nfunc "); end >= 0 {
		block = block[:end]
	}

	closeIdx := strings.Index(block, "updateAgentStateAfterSubmission(")
	if closeIdx < 0 {
		t.Fatal("done.go's notifyWitness block no longer calls updateAgentStateAfterSubmission — the hooked-conflict-task close site moved")
	}
	wakeIdx := strings.Index(block, "wakeRefineryForReadyConflict(")
	if wakeIdx < 0 {
		t.Fatalf("gt done's notifyWitness block does not call wakeRefineryForReadyConflict:\n%s\n"+
			"A conflict-resolution completion creates no MR of its own, so it can only reach the "+
			"refinery through this call (gt-rv8h).", block)
	}
	if wakeIdx < closeIdx {
		t.Errorf("wakeRefineryForReadyConflict is called before updateAgentStateAfterSubmission — "+
			"the readiness check would see the hooked conflict task as still open (gt-ue2h):\n%s", block)
	}
}

func readNudgeLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading nudge log: %v", err)
	}
	var entries []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			entries = append(entries, line)
		}
	}
	return entries
}
