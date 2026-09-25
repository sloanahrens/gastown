package refinery

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/rig"
)

// culpritLabelStore is a beads store that records label writes and serves the
// issues a batch's eligibility recheck reads, so a test can read back what
// recordBatchCulprits put on an MR bead and drive ProcessBatch end to end.
type culpritLabelStore struct {
	beadsdk.Storage
	mu     sync.Mutex
	labels map[string][]string
	issues map[string]*beadsdk.Issue
}

func newCulpritLabelStore(labels map[string][]string) *culpritLabelStore {
	if labels == nil {
		labels = map[string][]string{}
	}
	return &culpritLabelStore{labels: labels, issues: map[string]*beadsdk.Issue{}}
}

func (s *culpritLabelStore) GetIssue(_ context.Context, id string) (*beadsdk.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	issue, ok := s.issues[id]
	if !ok {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	return issue, nil
}

func (s *culpritLabelStore) GetDependenciesWithMetadata(_ context.Context, id string) ([]*beadsdk.IssueWithDependencyMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.issues[id]; !ok {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	return nil, nil
}

func (s *culpritLabelStore) AddLabel(_ context.Context, issueID, label, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.labels[issueID] {
		if existing == label {
			return nil
		}
	}
	s.labels[issueID] = append(s.labels[issueID], label)
	return nil
}

func (s *culpritLabelStore) RemoveLabel(_ context.Context, issueID, label, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.labels[issueID][:0]
	for _, existing := range s.labels[issueID] {
		if existing != label {
			kept = append(kept, existing)
		}
	}
	s.labels[issueID] = kept
	return nil
}

func (s *culpritLabelStore) GetLabels(_ context.Context, issueID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.labels[issueID]...), nil
}

func (s *culpritLabelStore) labelsOf(issueID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.labels[issueID]...)
}

// newCulpritTestEngineer is an Engineer with no git work and a label-recording
// beads store, for the marking tests that never touch a repository.
func newCulpritTestEngineer(t *testing.T, store *culpritLabelStore) *Engineer {
	t.Helper()
	workDir := t.TempDir()
	e := NewEngineer(&rig.Rig{Name: "test-rig", Path: workDir})
	e.testAllowSyntheticMRs = true
	e.beads = beads.NewWithStore(workDir, store)
	e.output = &strings.Builder{}
	return e
}

func TestBatchCulpritMarkedHeads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		labels []string
		want   []string
	}{
		{name: "no labels", labels: nil, want: nil},
		{name: "unrelated labels", labels: []string{"gt:merge-request", "needs-fix"}, want: nil},
		{
			name:   "one mark among others",
			labels: []string{"gt:merge-request", "batch-culprit:abc123", "severity:high"},
			want:   []string{"abc123"},
		},
		{
			name:   "several marks all reported",
			labels: []string{"batch-culprit:abc123", "batch-culprit:def456"},
			want:   []string{"abc123", "def456"},
		},
		{
			name:   "empty head is not a mark",
			labels: []string{"batch-culprit:", "batch-culprit"},
			want:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := batchCulpritMarkedHeads(tc.labels)
			if len(got) != len(tc.want) {
				t.Fatalf("batchCulpritMarkedHeads(%v) = %v, want %v", tc.labels, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("batchCulpritMarkedHeads(%v) = %v, want %v", tc.labels, got, tc.want)
				}
			}
		})
	}
}

// TestMRMarkedBatchCulprit pins the "until they change" half of gt-gz8l: the
// mark binds to the head the batch judged, so moving the branch head — a
// rework — makes the MR batch-eligible again.
func TestMRMarkedBatchCulprit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mr   *MRInfo
		want bool
	}{
		{name: "nil MR", mr: nil, want: false},
		{
			name: "no mark",
			mr:   &MRInfo{CommitSHA: "abc123", Labels: []string{"gt:merge-request"}},
			want: false,
		},
		{
			name: "mark names the current head",
			mr:   &MRInfo{CommitSHA: "abc123", Labels: []string{"batch-culprit:abc123"}},
			want: true,
		},
		{
			name: "mark names an old head, branch has moved",
			mr:   &MRInfo{CommitSHA: "def456", Labels: []string{"batch-culprit:abc123"}},
			want: false,
		},
		{
			name: "one of several marks names the current head",
			mr:   &MRInfo{CommitSHA: "def456", Labels: []string{"batch-culprit:abc123", "batch-culprit:def456"}},
			want: true,
		},
		{
			name: "no recorded head and a mark present",
			mr:   &MRInfo{CommitSHA: "", Labels: []string{"batch-culprit:abc123"}},
			want: true,
		},
		{
			name: "no recorded head and no mark",
			mr:   &MRInfo{CommitSHA: ""},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := MRMarkedBatchCulprit(tc.mr); got != tc.want {
				t.Errorf("MRMarkedBatchCulprit = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRecordBatchCulprits_MarksCurrentHead(t *testing.T) {
	t.Parallel()
	store := newCulpritLabelStore(map[string][]string{"gt-mr-a": {"gt:merge-request"}})
	e := newCulpritTestEngineer(t, store)

	e.recordBatchCulprits([]*MRInfo{{
		ID:          "gt-mr-a",
		SourceIssue: "gt-work-a",
		CommitSHA:   "abc123",
		Labels:      []string{"gt:merge-request"},
	}})

	labels := store.labelsOf("gt-mr-a")
	if !containsLabel(labels, "batch-culprit:abc123") {
		t.Errorf("labels = %v, want batch-culprit:abc123", labels)
	}
	if !containsLabel(labels, "gt:merge-request") {
		t.Errorf("labels = %v, want the existing gt:merge-request label kept", labels)
	}
}

// A mark left over from an earlier head must not survive the new one: two
// marks naming different heads would keep the MR out of every future batch
// even after a rework, which is the starvation the head-keyed mark exists to
// avoid.
func TestRecordBatchCulprits_ClearsStaleHeadMarks(t *testing.T) {
	t.Parallel()
	store := newCulpritLabelStore(map[string][]string{
		"gt-mr-a": {"gt:merge-request", "batch-culprit:old000", "batch-culprit:older00"},
	})
	e := newCulpritTestEngineer(t, store)

	mr := &MRInfo{
		ID:          "gt-mr-a",
		SourceIssue: "gt-work-a",
		CommitSHA:   "new1111",
		Labels:      []string{"gt:merge-request", "batch-culprit:old000", "batch-culprit:older00"},
	}
	e.recordBatchCulprits([]*MRInfo{mr})

	labels := store.labelsOf("gt-mr-a")
	if !containsLabel(labels, "batch-culprit:new1111") {
		t.Fatalf("labels = %v, want batch-culprit:new1111", labels)
	}
	if containsLabel(labels, "batch-culprit:old000") || containsLabel(labels, "batch-culprit:older00") {
		t.Errorf("labels = %v, want the stale head marks cleared", labels)
	}
}

// Re-marking at the same head must leave the label in place. beads applies
// removals after additions, so a removal list that named the label being added
// would delete the mark it just wrote.
func TestRecordBatchCulprits_RewritesTheSameHeadMark(t *testing.T) {
	t.Parallel()
	store := newCulpritLabelStore(map[string][]string{"gt-mr-a": {"batch-culprit:abc123"}})
	e := newCulpritTestEngineer(t, store)

	e.recordBatchCulprits([]*MRInfo{{
		ID:          "gt-mr-a",
		SourceIssue: "gt-work-a",
		CommitSHA:   "abc123",
		Labels:      []string{"batch-culprit:abc123"},
	}})

	if labels := store.labelsOf("gt-mr-a"); !containsLabel(labels, "batch-culprit:abc123") {
		t.Errorf("labels = %v, want batch-culprit:abc123 still present", labels)
	}
}

// Neither of these can be marked, and neither may reach beads: a synthetic
// merge-mechanics MR is a test fixture with no bead, and an MR with no
// recorded head has no head to key the mark to.
func TestRecordBatchCulprits_SkipsUnmarkableMRs(t *testing.T) {
	t.Parallel()
	store := newCulpritLabelStore(nil)
	e := newCulpritTestEngineer(t, store)

	e.recordBatchCulprits([]*MRInfo{
		nil,
		{ID: "", SourceIssue: "gt-work-a", CommitSHA: "abc123"},
		{ID: "mr-synthetic", CommitSHA: "abc123"},
		{ID: "gt-mr-nohead", SourceIssue: "gt-work-a"},
	})

	if got := store.labelsOf("mr-synthetic"); len(got) != 0 {
		t.Errorf("mr-synthetic labels = %v, want none (synthetic MRs have no bead)", got)
	}
	if got := store.labelsOf("gt-mr-nohead"); len(got) != 0 {
		t.Errorf("gt-mr-nohead labels = %v, want none (no head to key the mark to)", got)
	}
}

func containsLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

// TestProcessBatch_BisectionMarksCulprit is the gt-gz8l acceptance case end to
// end: a bisection that isolates an MR records the mark on that MR's bead, so
// the next cycle's candidate listing can skip it. Real (non-synthetic) MRs are
// required — the mark is a beads write, and only a real MR has a bead.
func TestProcessBatch_BisectionMarksCulprit(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	// Both MRs fail the gate, so bisection isolates both and nothing merges:
	// the only bead write ProcessBatch makes is the mark under test.
	createFeatureBranch(t, workDir, "feature-a", "FAIL_A", "fail a\n")
	createFeatureBranch(t, workDir, "feature-b", "FAIL_B", "fail b\n")
	pushBranch(t, workDir, "feature-a")
	pushBranch(t, workDir, "feature-b")

	e := newTestEngineer(t, workDir, g)
	e.config.Gates = map[string]*GateConfig{
		"check": {Cmd: "test ! -f FAIL_A && test ! -f FAIL_B"},
	}
	e.config.GatesParallel = false

	headA := run(t, workDir, "git", "rev-parse", "feature-a")
	headB := run(t, workDir, "git", "rev-parse", "feature-b")
	store := newCulpritLabelStore(map[string][]string{"gt-mr-a": {"gt:merge-request"}})
	for _, issue := range []*beadsdk.Issue{
		culpritMRIssue("gt-mr-a", "feature-a", "gt-work-a", headA),
		culpritMRIssue("gt-mr-b", "feature-b", "gt-work-b", headB),
		culpritWorkIssue("gt-work-a"),
		culpritWorkIssue("gt-work-b"),
	} {
		store.issues[issue.ID] = issue
	}
	e.beads = beads.NewWithStore(workDir, store)

	batch := []*MRInfo{
		culpritMRInfo("gt-mr-a", "feature-a", "gt-work-a", headA),
		culpritMRInfo("gt-mr-b", "feature-b", "gt-work-b", headB),
	}
	cfg := &BatchConfig{MaxBatchSize: 5, RetryBatchOnFlaky: false}

	result := e.ProcessBatch(context.Background(), batch, "main", cfg)

	if len(result.Culprits) != 2 {
		t.Fatalf("expected both MRs isolated as culprits, got %v", mrIDs(result.Culprits))
	}
	for _, mr := range result.Culprits {
		head := headA
		if mr.ID == "gt-mr-b" {
			head = headB
		}
		if labels := store.labelsOf(mr.ID); !containsLabel(labels, batchCulpritLabelPrefix+head) {
			t.Errorf("%s labels = %v, want %s", mr.ID, labels, batchCulpritLabelPrefix+head)
		}
	}
}

// culpritMRIssue builds an open MR bead carrying the merge-request fields the
// eligibility recheck reads back, including the submitted head the mark keys
// on.
func culpritMRIssue(id, branch, sourceIssue, commitSHA string) *beadsdk.Issue {
	now := time.Now()
	return &beadsdk.Issue{
		ID:          id,
		Title:       id,
		Description: beads.FormatMRFields(&beads.MRFields{Branch: branch, Target: "main", SourceIssue: sourceIssue, CommitSHA: commitSHA, Rig: "test-rig"}),
		Status:      beadsdk.StatusOpen,
		IssueType:   beadsdk.IssueType("task"),
		Priority:    2,
		CreatedAt:   now,
		UpdatedAt:   now,
		Labels:      []string{"gt:merge-request"},
	}
}

// culpritWorkIssue is the open source issue behind a culpritMRIssue, which
// recheckMRStillMergeable reads before the batch may stack the MR.
func culpritWorkIssue(id string) *beadsdk.Issue {
	now := time.Now()
	return &beadsdk.Issue{
		ID:        id,
		Title:     id,
		Status:    beadsdk.StatusOpen,
		IssueType: beadsdk.IssueType("task"),
		Priority:  2,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// culpritMRInfo is the MRInfo a batch would be assembled with, matching what
// issueToMRInfo builds from culpritMRIssue.
func culpritMRInfo(id, branch, sourceIssue, commitSHA string) *MRInfo {
	return &MRInfo{
		ID:          id,
		Branch:      branch,
		Target:      "main",
		SourceIssue: sourceIssue,
		CommitSHA:   commitSHA,
		Priority:    2,
		CreatedAt:   time.Now().Add(-2 * time.Hour),
		Labels:      []string{"gt:merge-request"},
	}
}
