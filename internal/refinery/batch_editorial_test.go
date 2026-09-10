package refinery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
	"github.com/steveyegge/gastown/internal/rig"
)

// batchReviewStore is a minimal in-process beadsdk.Storage fake for the MR
// beads that editorial.Run reads/updates during a batch review — mirrors
// internal/refinery/editorial's own reviewStore for the same purpose.
type batchReviewStore struct {
	beadsdk.Storage
	mu     sync.Mutex
	issues map[string]*beadsdk.Issue
}

func newBatchReviewStore(issues ...*beadsdk.Issue) *batchReviewStore {
	s := &batchReviewStore{issues: make(map[string]*beadsdk.Issue, len(issues))}
	for _, issue := range issues {
		s.issues[issue.ID] = issue
	}
	return s
}

func (s *batchReviewStore) GetIssue(_ context.Context, id string) (*beadsdk.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	issue, ok := s.issues[id]
	if !ok {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	return issue, nil
}

func (s *batchReviewStore) GetLabels(_ context.Context, id string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	issue, ok := s.issues[id]
	if !ok {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	return append([]string(nil), issue.Labels...), nil
}

func (s *batchReviewStore) GetDependenciesWithMetadata(_ context.Context, id string) ([]*beadsdk.IssueWithDependencyMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.issues[id]; !ok {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	return nil, nil
}

func (s *batchReviewStore) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	issue, ok := s.issues[id]
	if !ok {
		return fmt.Errorf("issue %s not found", id)
	}
	if v, ok := updates["description"]; ok {
		issue.Description, _ = v.(string)
	}
	issue.UpdatedAt = time.Now()
	return nil
}

func batchMRIssue(id, branch, target, worker string) *beadsdk.Issue {
	fields := &beads.MRFields{Branch: branch, Target: target, Worker: worker, Rig: "test-rig"}
	now := time.Now()
	return &beadsdk.Issue{
		ID:          id,
		Title:       id,
		Description: beads.FormatMRFields(fields),
		Status:      beadsdk.StatusOpen,
		IssueType:   beadsdk.IssueType("task"),
		Priority:    2,
		CreatedAt:   now,
		UpdatedAt:   now,
		Labels:      []string{"gt:merge-request"},
	}
}

// fakeBDForBatch installs a bd stand-in on PATH so plugin.Recorder
// (RecordReceipt/RecordFailure, which always shells out) has something to
// talk to during a batch review test.
func fakeBDForBatch(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	bdPath := filepath.Join(binDir, "bd")
	script := "#!/usr/bin/env bash\n" +
		"case \"$1\" in\n" +
		"  create) printf '{\\\"id\\\":\\\"gt-test-receipt\\\"}\\n' ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(bdPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// writeEditorialManifest writes a valid harness manifest + stub om binary
// at rigDir, satisfying editorial.LoadManifest/AssertVersion.
func writeEditorialManifest(t *testing.T, rigDir string) {
	t.Helper()
	binPath := filepath.Join(rigDir, "om-stub-binary")
	binContent := []byte("#!/bin/sh\necho om\n")
	if err := os.WriteFile(binPath, binContent, 0755); err != nil {
		t.Fatalf("write om stub binary: %v", err)
	}
	sum := sha256.Sum256(binContent)
	var m editorial.Manifest
	m.OMBinary.Path = binPath
	m.OMBinary.SHA256 = hex.EncodeToString(sum[:])
	m.OMBinary.Version = "1.4.0"
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".gastown-harness-manifest.json"), data, 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func verdictPathFromArgs(args []string) string {
	for i, a := range args {
		if a == "--out" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func mrIDFromArgs(args []string) string {
	for i, a := range args {
		if a == "--mr" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestReviewBatchCandidates_NotRequired_NoOp is the om-gate T7 upstream
// compatibility case: a rig that never set merge_queue.editorial.required
// must see the batch pass through untouched, exactly like before this
// feature existed.
func TestReviewBatchCandidates_NotRequired_NoOp(t *testing.T) {
	r := &rig.Rig{Name: "test-rig", Path: t.TempDir()}
	e := NewEngineer(r)

	candidates := []*MRInfo{
		makeMR("mr-1", "branch-1", "main"),
		makeMR("mr-2", "branch-2", "main"),
	}

	approved, reviewed, notes := e.reviewBatchCandidates(context.Background(), candidates, "main")

	if len(approved) != 2 {
		t.Fatalf("expected 2 approved (pass-through), got %d", len(approved))
	}
	if reviewed != nil {
		t.Fatalf("expected nil reviewed list when editorial not required, got %v", reviewed)
	}
	if notes != nil {
		t.Fatalf("expected nil notes map when editorial not required, got %v", notes)
	}
}

// TestReviewBatchCandidates_RequiredFalse_NoOp covers the same pass-through
// when Editorial is explicitly configured but Required is false.
func TestReviewBatchCandidates_RequiredFalse_NoOp(t *testing.T) {
	r := &rig.Rig{Name: "test-rig", Path: t.TempDir()}
	e := NewEngineer(r)
	e.config.Editorial = &config.EditorialConfig{Required: false}

	candidates := []*MRInfo{makeMR("mr-1", "branch-1", "main")}

	approved, reviewed, notes := e.reviewBatchCandidates(context.Background(), candidates, "main")

	if len(approved) != 1 {
		t.Fatalf("expected 1 approved (pass-through), got %d", len(approved))
	}
	if reviewed != nil {
		t.Fatalf("expected nil reviewed list when editorial required=false, got %v", reviewed)
	}
	if notes != nil {
		t.Fatalf("expected nil notes map when editorial required=false, got %v", notes)
	}
}

// TestReviewBatchCandidates_BoundedParallelism_DropsRequestChanges is the
// om-gate T7 acceptance case: 3 approve + 1 request_changes -> 3 approved
// candidates proceed, the request_changes candidate is dropped (left
// queued, not touched), and no more than ReviewParallelism reviews run
// concurrently.
func TestReviewBatchCandidates_BoundedParallelism_DropsRequestChanges(t *testing.T) {
	fakeBDForBatch(t)
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	candidateIDs := []string{"mr-a", "mr-b", "mr-c", "mr-d"}
	branches := map[string]string{"mr-a": "feature-a", "mr-b": "feature-b", "mr-c": "feature-c", "mr-d": "feature-d"}
	for _, id := range candidateIDs {
		createFeatureBranch(t, workDir, branches[id], id+".txt", "hello "+id+"\n")
		// editorial.Run's rehearsal reads origin/<branch>, not the local branch.
		run(t, workDir, "git", "push", "origin", branches[id])
	}
	// Written after the feature branches: createFeatureBranch's `git add .`
	// would otherwise sweep these untracked files into a feature branch's
	// commit, and the following `git checkout main` would delete them again
	// (main's tree doesn't have them).
	writeEditorialManifest(t, workDir)

	var issues []*beadsdk.Issue
	for _, id := range candidateIDs {
		issues = append(issues, batchMRIssue(id, branches[id], "main", "polecats/test"))
	}
	store := newBatchReviewStore(issues...)

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true, ReviewParallelism: 3}
	e.beads = beads.NewWithStore(workDir, store)

	var mu sync.Mutex
	concurrent := 0
	maxConcurrent := 0
	e.editorialExec = func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
		mu.Lock()
		concurrent++
		if concurrent > maxConcurrent {
			maxConcurrent = concurrent
		}
		mu.Unlock()

		time.Sleep(50 * time.Millisecond)

		mu.Lock()
		concurrent--
		mu.Unlock()

		verdict := "approve"
		if mrIDFromArgs(args) == "mr-d" {
			verdict = "request_changes"
		}
		data, _ := json.Marshal(map[string]interface{}{"score": 0.8, "verdict": verdict})
		if err := os.WriteFile(verdictPathFromArgs(args), data, 0644); err != nil {
			return "", 0, err
		}
		return "", 0, nil
	}

	candidates := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
		makeMR("mr-c", "feature-c", "main"),
		makeMR("mr-d", "feature-d", "main"),
	}
	for _, mr := range candidates {
		mr.Worker = "polecats/test"
	}

	approved, reviewed, notes := e.reviewBatchCandidates(context.Background(), candidates, "main")

	if len(approved) != 3 {
		t.Fatalf("expected 3 approved, got %d: %v (output:\n%s)", len(approved), mrIDs(approved), e.output)
	}
	for _, mr := range approved {
		if mr.ID == "mr-d" {
			t.Fatalf("mr-d (request_changes) must not be in approved set")
		}
		if notes[mr.ID] == nil || notes[mr.ID].Verdict != "approve" {
			t.Fatalf("expected an approve note for %s, got %v", mr.ID, notes[mr.ID])
		}
	}
	if notes["mr-d"] != nil {
		t.Fatalf("expected no note recorded for dropped mr-d, got %v", notes["mr-d"])
	}
	if len(reviewed) != 4 {
		t.Fatalf("expected 4 reviewed entries, got %d", len(reviewed))
	}
	if maxConcurrent > 3 {
		t.Fatalf("expected at most 3 concurrent review invocations, saw %d", maxConcurrent)
	}
	if maxConcurrent < 2 {
		t.Fatalf("expected some real concurrency (>=2), saw max %d — semaphore may be serializing", maxConcurrent)
	}
}

// --- ejectPatchIDChanged tests ---

func TestEjectPatchIDChanged_NotRequired_NoOp(t *testing.T) {
	r := &rig.Rig{Name: "test-rig", Path: t.TempDir()}
	e := NewEngineer(r)

	stacked := []*MRInfo{makeMR("mr-a", "feature-a", "main")}
	kept, ejected, err := e.ejectPatchIDChanged(stacked, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(kept) != 1 || len(ejected) != 0 {
		t.Fatalf("expected pass-through (1 kept, 0 ejected), got kept=%d ejected=%d", len(kept), len(ejected))
	}
}

func TestEjectPatchIDChanged_MatchingPatchIDs_KeepsAll(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")
	createFeatureBranch(t, workDir, "feature-b", "b.txt", "hello b\n")

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
	}
	stacked, conflicts, err := e.BuildRebaseStack(context.Background(), batch, "main")
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("BuildRebaseStack: stacked=%d conflicts=%d err=%v", len(stacked), len(conflicts), err)
	}

	// Recompute the actual on-stack patch-id for each member and feed it
	// back as its own note, so nothing should be ejected.
	n := len(stacked)
	notes := make(map[string]*editorial.Note, n)
	base, err := g.Rev(fmt.Sprintf("HEAD~%d", n))
	if err != nil {
		t.Fatalf("rev base: %v", err)
	}
	pre := base
	for i, mr := range stacked {
		post, err := g.Rev(fmt.Sprintf("HEAD~%d", n-1-i))
		if err != nil {
			t.Fatalf("rev tip %d: %v", i, err)
		}
		patchID, err := g.PatchID(pre, post)
		if err != nil {
			t.Fatalf("PatchID: %v", err)
		}
		notes[mr.ID] = &editorial.Note{PatchID: patchID}
		pre = post
	}

	kept, ejected, err := e.ejectPatchIDChanged(stacked, notes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ejected) != 0 {
		t.Fatalf("expected 0 ejected, got %d: %v", len(ejected), ejected)
	}
	if len(kept) != 2 {
		t.Fatalf("expected 2 kept, got %d", len(kept))
	}
}

func TestEjectPatchIDChanged_MismatchedPatchID_Ejects(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")
	createFeatureBranch(t, workDir, "feature-b", "b.txt", "hello b\n")

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
	}
	stacked, conflicts, err := e.BuildRebaseStack(context.Background(), batch, "main")
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("BuildRebaseStack: stacked=%d conflicts=%d err=%v", len(stacked), len(conflicts), err)
	}

	// mr-a's note carries a stale patch-id (as if its content changed once
	// stacked); mr-b's note is correct.
	n := len(stacked)
	base, err := g.Rev(fmt.Sprintf("HEAD~%d", n))
	if err != nil {
		t.Fatalf("rev base: %v", err)
	}
	bHead, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	aHead, err := g.Rev("HEAD~1")
	if err != nil {
		t.Fatalf("rev HEAD~1: %v", err)
	}
	bPatchID, err := g.PatchID(aHead, bHead)
	if err != nil {
		t.Fatalf("PatchID b: %v", err)
	}
	_ = base
	notes := map[string]*editorial.Note{
		"mr-a": {PatchID: "stale-patch-id-does-not-match"},
		"mr-b": {PatchID: bPatchID},
	}

	kept, ejected, err := e.ejectPatchIDChanged(stacked, notes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ejected) != 1 || ejected[0].ID != "mr-a" || ejected[0].Reason != "patch_id_changed_on_stack" {
		t.Fatalf("expected mr-a ejected with reason patch_id_changed_on_stack, got %v", ejected)
	}
	if len(kept) != 1 || kept[0].ID != "mr-b" {
		t.Fatalf("expected mr-b kept, got %v", stackedIDs(kept))
	}
}

// TestProcessBatch_EditorialReview_EjectsPatchIDChanged is the om-gate T7
// acceptance case end-to-end: 4 candidates, 3 approve + 1 request_changes ->
// batch of 3; one of the 3 has its branch content change between review and
// stacking (simulating a conflict-resolved-during-stacking drift) -> it is
// ejected; the batch lands the remaining 2 with editorial notes copied.
func TestProcessBatch_EditorialReview_EjectsPatchIDChanged(t *testing.T) {
	fakeBDForBatch(t)
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	candidateIDs := []string{"mr-a", "mr-b", "mr-c", "mr-d"}
	branches := map[string]string{"mr-a": "feature-a", "mr-b": "feature-b", "mr-c": "feature-c", "mr-d": "feature-d"}
	for _, id := range candidateIDs {
		createFeatureBranch(t, workDir, branches[id], id+".txt", "hello "+id+"\n")
		run(t, workDir, "git", "push", "origin", branches[id])
	}
	writeEditorialManifest(t, workDir)

	var issues []*beadsdk.Issue
	for _, id := range candidateIDs {
		issues = append(issues, batchMRIssue(id, branches[id], "main", "polecats/test"))
	}
	store := newBatchReviewStore(issues...)

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true, ReviewParallelism: 3}
	e.beads = beads.NewWithStore(workDir, store)

	var mu sync.Mutex
	amended := false
	e.editorialExec = func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
		mrID := mrIDFromArgs(args)
		verdict := "approve"
		if mrID == "mr-d" {
			verdict = "request_changes"
		}
		if mrID == "mr-c" {
			// Simulate the branch changing after its patch-id was already
			// computed for review (e.g. a resolved conflict during
			// stacking): append a commit to feature-c's local branch here,
			// mid-review, before Run gets to writing the note.
			mu.Lock()
			if !amended {
				run(t, workDir, "git", "checkout", "feature-c")
				writeFile(t, workDir, "c-extra.txt", "surprise\n")
				run(t, workDir, "git", "add", ".")
				run(t, workDir, "git", "commit", "-m", "drift after review")
				run(t, workDir, "git", "checkout", "main")
				amended = true
			}
			mu.Unlock()
		}
		data, _ := json.Marshal(map[string]interface{}{"score": 0.8, "verdict": verdict})
		if err := os.WriteFile(verdictPathFromArgs(args), data, 0644); err != nil {
			return "", 0, err
		}
		return "", 0, nil
	}

	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
		makeMR("mr-c", "feature-c", "main"),
		makeMR("mr-d", "feature-d", "main"),
	}
	for _, mr := range batch {
		mr.Worker = "polecats/test"
	}

	result := e.ProcessBatch(context.Background(), batch, "main", DefaultBatchConfig())
	if result.Error != nil {
		t.Fatalf("unexpected error: %v (output:\n%s)", result.Error, e.output)
	}

	if len(result.Reviewed) != 4 {
		t.Fatalf("expected 4 reviewed entries, got %d: %v", len(result.Reviewed), result.Reviewed)
	}
	if len(result.Ejected) != 1 || result.Ejected[0].ID != "mr-c" || result.Ejected[0].Reason != "patch_id_changed_on_stack" {
		t.Fatalf("expected mr-c ejected with reason patch_id_changed_on_stack, got %v", result.Ejected)
	}
	if len(result.Merged) != 2 {
		t.Fatalf("expected 2 merged, got %d: %v (output:\n%s)", len(result.Merged), mrIDs(result.Merged), e.output)
	}
	mergedIDs := map[string]bool{}
	for _, mr := range result.Merged {
		mergedIDs[mr.ID] = true
	}
	if !mergedIDs["mr-a"] || !mergedIDs["mr-b"] {
		t.Fatalf("expected mr-a and mr-b merged, got %v", mrIDs(result.Merged))
	}
	if mergedIDs["mr-c"] || mergedIDs["mr-d"] {
		t.Fatalf("mr-c (ejected) and mr-d (request_changes) must not be merged, got %v", mrIDs(result.Merged))
	}

	// Verify pushed to origin by re-cloning: mr-a.txt and mr-b.txt land,
	// mr-c.txt/c-extra.txt (ejected) and mr-d.txt (request_changes) do not.
	verifyDir := filepath.Join(filepath.Dir(workDir), "verify")
	bareDir := filepath.Join(filepath.Dir(workDir), "origin.git")
	run(t, filepath.Dir(workDir), "git", "clone", bareDir, verifyDir)
	for _, f := range []string{"mr-a.txt", "mr-b.txt"} {
		if _, statErr := os.Stat(filepath.Join(verifyDir, f)); os.IsNotExist(statErr) {
			t.Errorf("expected %s in cloned repo after push", f)
		}
	}
	for _, f := range []string{"mr-c.txt", "c-extra.txt", "mr-d.txt"} {
		if _, statErr := os.Stat(filepath.Join(verifyDir, f)); statErr == nil {
			t.Errorf("did not expect %s in cloned repo (its MR was ejected/dropped)", f)
		}
	}
}
