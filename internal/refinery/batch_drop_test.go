package refinery

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
)

func TestEditorialDropCount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		labels []string
		head   string
		want   int
	}{
		{name: "no labels", labels: nil, head: "abc123", want: 0},
		{name: "unrelated labels", labels: []string{"gt:merge-request"}, head: "abc123", want: 0},
		{name: "one mark for this head", labels: []string{"editorial-drop:abc123:2"}, head: "abc123", want: 2},
		{name: "mark for a different head", labels: []string{"editorial-drop:def456:5"}, head: "abc123", want: 0},
		{
			name:   "mark among others",
			labels: []string{"gt:merge-request", "editorial-drop:abc123:1", "severity:high"},
			head:   "abc123",
			want:   1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := editorialDropCount(tc.labels, tc.head); got != tc.want {
				t.Errorf("editorialDropCount(%v, %q) = %d, want %d", tc.labels, tc.head, got, tc.want)
			}
		})
	}
}

func TestEditorialDropEscalated(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		labels []string
		head   string
		want   bool
	}{
		{name: "no labels", labels: nil, head: "abc123", want: false},
		{name: "escalated for this head", labels: []string{"editorial-drop-escalated:abc123"}, head: "abc123", want: true},
		{name: "escalated for a different head", labels: []string{"editorial-drop-escalated:def456"}, head: "abc123", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := editorialDropEscalated(tc.labels, tc.head); got != tc.want {
				t.Errorf("editorialDropEscalated(%v, %q) = %v, want %v", tc.labels, tc.head, got, tc.want)
			}
		})
	}
}

// TestRecordEditorialDrop_IncrementsAcrossCycles pins the gt-crvw0 fix: a
// deterministic infra failure repeated on an unchanged branch head must be
// countable across batch cycles, not silently retried forever.
func TestRecordEditorialDrop_IncrementsAcrossCycles(t *testing.T) {
	t.Parallel()
	store := newCulpritLabelStore(map[string][]string{"gt-mr-a": {"gt:merge-request"}})
	e := newCulpritTestEngineer(t, store)

	mr := &MRInfo{ID: "gt-mr-a", SourceIssue: "gt-work-a", CommitSHA: "abc123", Labels: []string{"gt:merge-request"}}

	// Cycle 1: first drop at this head.
	e.recordEditorialDrop(mr, editorial.Tooling, "rehearsal failed: exit 1")
	labels := store.labelsOf("gt-mr-a")
	if !containsLabel(labels, "editorial-drop:abc123:1") {
		t.Fatalf("after 1st drop, labels = %v, want editorial-drop:abc123:1", labels)
	}
	if editorialDropEscalated(labels, "abc123") {
		t.Fatalf("after 1st drop, already escalated: %v", labels)
	}

	// Cycle 2: same head, next cycle reads the bead's current labels back
	// before reviewing again.
	mr.Labels = labels
	e.recordEditorialDrop(mr, editorial.Tooling, "rehearsal failed: exit 1")
	labels = store.labelsOf("gt-mr-a")
	if !containsLabel(labels, "editorial-drop:abc123:2") {
		t.Fatalf("after 2nd drop, labels = %v, want editorial-drop:abc123:2", labels)
	}
	if containsLabel(labels, "editorial-drop:abc123:1") {
		t.Fatalf("after 2nd drop, labels = %v, want the stale count-1 mark cleared", labels)
	}
	if editorialDropEscalated(labels, "abc123") {
		t.Fatalf("after 2nd drop (below threshold), already escalated: %v", labels)
	}

	// Cycle 3: crosses editorialDropEscalationThreshold (3) — escalate once.
	mr.Labels = labels
	e.recordEditorialDrop(mr, editorial.Tooling, "rehearsal failed: exit 1")
	labels = store.labelsOf("gt-mr-a")
	if !containsLabel(labels, "editorial-drop:abc123:3") {
		t.Fatalf("after 3rd drop, labels = %v, want editorial-drop:abc123:3", labels)
	}
	if !editorialDropEscalated(labels, "abc123") {
		t.Fatalf("after 3rd drop (at threshold), labels = %v, want it marked escalated", labels)
	}

	// Cycle 4: escalation must fire only once per head — a repeat drop keeps
	// counting but must not re-add or duplicate the escalation mark.
	mr.Labels = labels
	e.recordEditorialDrop(mr, editorial.Tooling, "rehearsal failed: exit 1")
	labels = store.labelsOf("gt-mr-a")
	if !containsLabel(labels, "editorial-drop:abc123:4") {
		t.Fatalf("after 4th drop, labels = %v, want editorial-drop:abc123:4", labels)
	}
	count := 0
	for _, l := range labels {
		if l == "editorial-drop-escalated:abc123" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("editorial-drop-escalated:abc123 appears %d times in %v, want exactly 1", count, labels)
	}
}

// TestRecordEditorialDrop_HeadMoveResetsCount covers a rework: once the
// branch head changes, the old count no longer describes the current
// revision and must not carry over, mirroring batch-culprit's head-keyed
// mark (gt-gz8l).
func TestRecordEditorialDrop_HeadMoveResetsCount(t *testing.T) {
	t.Parallel()
	store := newCulpritLabelStore(map[string][]string{
		"gt-mr-a": {"gt:merge-request", "editorial-drop:old000:3", "editorial-drop-escalated:old000"},
	})
	e := newCulpritTestEngineer(t, store)

	mr := &MRInfo{
		ID:          "gt-mr-a",
		SourceIssue: "gt-work-a",
		CommitSHA:   "new111",
		Labels:      []string{"gt:merge-request", "editorial-drop:old000:3", "editorial-drop-escalated:old000"},
	}
	e.recordEditorialDrop(mr, editorial.Tooling, "rehearsal failed: exit 1")

	labels := store.labelsOf("gt-mr-a")
	if !containsLabel(labels, "editorial-drop:new111:1") {
		t.Fatalf("labels = %v, want the count reset to 1 for the new head", labels)
	}
	if containsLabel(labels, "editorial-drop:old000:3") {
		t.Fatalf("labels = %v, want the stale old-head count cleared", labels)
	}
	if editorialDropEscalated(labels, "new111") {
		t.Fatalf("labels = %v, want the new head not already escalated", labels)
	}
}

// TestRecordEditorialDrop_NoCommitSHA_NoOp covers an MR with no recorded
// submitted head: there is nothing to key the mark to, so recordEditorialDrop
// must not write anything (mirrors recordBatchCulprits' same guard).
func TestRecordEditorialDrop_NoCommitSHA_NoOp(t *testing.T) {
	t.Parallel()
	store := newCulpritLabelStore(map[string][]string{"gt-mr-a": {"gt:merge-request"}})
	e := newCulpritTestEngineer(t, store)

	mr := &MRInfo{ID: "gt-mr-a", SourceIssue: "gt-work-a", CommitSHA: "", Labels: []string{"gt:merge-request"}}
	e.recordEditorialDrop(mr, editorial.Tooling, "rehearsal failed: exit 1")

	labels := store.labelsOf("gt-mr-a")
	if len(labels) != 1 || labels[0] != "gt:merge-request" {
		t.Fatalf("labels = %v, want unchanged (no head to key the mark to)", labels)
	}
}

// TestReviewBatchCandidates_RehearsalFailure_SurfacesStderrAndEscalates is the
// gt-crvw0 end-to-end case: a candidate whose branch was never pushed to
// origin fails rehearsal.Branch deterministically (the same class of
// per-branch, not global, failure the reported bug hit), while a healthy
// sibling candidate in the same batch still stacks normally. Before this fix
// the drop line discarded r.Stderr and nothing tracked the repeat, so
// gt-wisp-06j7 sat dropped for 7-8 consecutive batches with no visibility and
// no path to ever land.
func TestReviewBatchCandidates_RehearsalFailure_SurfacesStderrAndEscalates(t *testing.T) {
	fakeBDForBatch(t)
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-good", "good.txt", "hello good\n")
	run(t, workDir, "git", "push", "origin", "feature-good")
	writeEditorialManifest(t, workDir)

	// feature-bad is never pushed to origin, so rehearsal.Branch(mr.Branch)
	// fails to resolve it — deterministic, per-branch, and reproduced on
	// every cycle, exactly like the reported bug.
	goodIssue := batchMRIssue("mr-good", "feature-good", "main", "polecats/test")
	badIssue := batchMRIssue("mr-bad", "feature-bad", "main", "polecats/test")
	store := newBatchReviewStore(goodIssue, badIssue)

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true, ReviewParallelism: 3}
	e.beads = beads.NewWithStore(workDir, store)
	e.editorialExec = func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
		data, _ := json.Marshal(map[string]interface{}{"score": 0.8, "verdict": "approve"})
		if err := os.WriteFile(verdictPathFromArgs(args), data, 0644); err != nil {
			return "", 0, err
		}
		return "", 0, nil
	}

	runCycle := func() ([]*MRInfo, string) {
		good := makeMR("mr-good", "feature-good", "main")
		good.Worker = "polecats/test"
		bad := makeMR("mr-bad", "feature-bad", "main")
		bad.Worker = "polecats/test"
		// A non-empty SourceIssue keeps this from being treated as a
		// testAllowSyntheticMRs synthetic MR (see isSyntheticMergeMechanicsMR)
		// — every real MR has one, and recordEditorialDrop must run for it.
		bad.SourceIssue = "gt-work-bad"
		bad.CommitSHA = "deadbeef"
		bad.Labels = store.labelsOf("mr-bad")

		out := &strings.Builder{}
		e.output = out
		approved, _, _ := e.reviewBatchCandidates(context.Background(), []*MRInfo{good, bad}, "main")
		return approved, out.String()
	}

	approved, output := runCycle()
	if len(approved) != 1 || approved[0].ID != "mr-good" {
		t.Fatalf("expected only mr-good approved, got %v (output:\n%s)", mrIDs(approved), output)
	}
	if !strings.Contains(output, "mr-bad") || !strings.Contains(output, "dropped from batch") {
		t.Fatalf("expected a drop line for mr-bad, got:\n%s", output)
	}
	if strings.Contains(output, "no error detail captured") {
		t.Fatalf("expected the actual rehearsal error surfaced, got a placeholder:\n%s", output)
	}
	if !containsLabel(store.labelsOf("mr-bad"), "editorial-drop:deadbeef:1") {
		t.Fatalf("mr-bad labels = %v, want editorial-drop:deadbeef:1 after the 1st drop", store.labelsOf("mr-bad"))
	}

	// Two more cycles at the same (never reworked) head cross the escalation
	// threshold.
	runCycle()
	_, output = runCycle()

	labels := store.labelsOf("mr-bad")
	if !containsLabel(labels, "editorial-drop:deadbeef:3") {
		t.Fatalf("mr-bad labels = %v, want editorial-drop:deadbeef:3 after the 3rd drop", labels)
	}
	if !containsLabel(labels, "editorial-drop-escalated:deadbeef") {
		t.Fatalf("mr-bad labels = %v, want it marked escalated after 3 consecutive drops (output:\n%s)", labels, output)
	}
}
