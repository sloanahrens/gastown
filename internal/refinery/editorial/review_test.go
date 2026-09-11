package editorial

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/plugin"
)

// reviewStore is a minimal in-process beadsdk.Storage fake for Run's beads
// interactions (Show/Update on the MR bead, Create for follow-up beads),
// mirroring internal/refinery's prepushStore pattern for the same purpose in
// a different package.
type reviewStore struct {
	beadsdk.Storage
	issues map[string]*beadsdk.Issue
	nextID int
	// failCreate makes CreateIssue fail, standing in for a follow-up bead
	// filing failure on an otherwise-successful approve.
	failCreate bool
}

func newReviewStore(issues ...*beadsdk.Issue) *reviewStore {
	s := &reviewStore{issues: make(map[string]*beadsdk.Issue, len(issues))}
	for _, issue := range issues {
		s.issues[issue.ID] = issue
	}
	return s
}

func (s *reviewStore) GetIssue(_ context.Context, id string) (*beadsdk.Issue, error) {
	issue, ok := s.issues[id]
	if !ok {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	return issue, nil
}

func (s *reviewStore) GetLabels(_ context.Context, id string) ([]string, error) {
	issue, ok := s.issues[id]
	if !ok {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	return append([]string(nil), issue.Labels...), nil
}

func (s *reviewStore) GetDependenciesWithMetadata(_ context.Context, id string) ([]*beadsdk.IssueWithDependencyMetadata, error) {
	if _, ok := s.issues[id]; !ok {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	return nil, nil
}

func (s *reviewStore) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, _ string) error {
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

func (s *reviewStore) CreateIssue(_ context.Context, issue *beadsdk.Issue, actor string) error {
	if s.failCreate {
		return fmt.Errorf("simulated create failure")
	}
	s.nextID++
	issue.ID = fmt.Sprintf("gt-followup-%d", s.nextID)
	issue.CreatedAt = time.Now()
	issue.UpdatedAt = time.Now()
	issue.CreatedBy = actor
	s.issues[issue.ID] = issue
	return nil
}

func mrIssue(id, branch, target, sourceIssue, rig, worker string) *beadsdk.Issue {
	fields := &beads.MRFields{Branch: branch, Target: target, SourceIssue: sourceIssue, Rig: rig, Worker: worker}
	now := time.Now()
	return &beadsdk.Issue{
		ID:          id,
		Title:       id,
		Description: beads.FormatMRFields(fields),
		Status:      beadsdk.StatusOpen,
		IssueType:   beadsdk.IssueType("task"),
		Priority:    1,
		CreatedAt:   now,
		UpdatedAt:   now,
		Labels:      []string{"gt:merge-request"},
	}
}

// fakeBDForReview installs a bd stand-in on PATH so plugin.Recorder
// (RecordReceipt/RecordFailure, which always shell out — see
// internal/plugin/recording.go) and Beads.AddComment (which has no
// in-process store path) have something to talk to.
func fakeBDForReview(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	bdPath := filepath.Join(binDir, "bd")
	script := "#!/usr/bin/env bash\n" +
		"case \"$1\" in\n" +
		"  create) printf '{\\\"id\\\":\\\"gt-test-receipt\\\"}\\n' ;;\n" +
		"  close) exit 0 ;;\n" +
		"  comments) exit 0 ;;\n" +
		"  *) exit 2 ;;\n" +
		"esac\n"
	if err := os.WriteFile(bdPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func commitFileReview(t *testing.T, dir, path, content, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	g := git.NewGit(dir)
	if err := g.Add(path); err != nil {
		t.Fatalf("add %s: %v", path, err)
	}
	if err := g.Commit(message); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rev, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	return rev
}

// reviewFixture sets up a repo with a fake origin/main tracking ref and a
// head commit ready to review, plus a rig directory with a valid harness
// manifest pointing at a real (stub) om binary file.
type reviewFixture struct {
	repoDir string
	rigDir  string
	base    string
	head    string
}

func newReviewFixture(t *testing.T) *reviewFixture {
	t.Helper()
	repoDir := initTestRepo(t)
	g := git.NewGit(repoDir)
	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	cmd := exec.Command("git", "update-ref", "refs/remotes/origin/main", base)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-ref origin/main: %v\n%s", err, out)
	}

	// A real origin remote (unrelated to the fake refs/remotes/origin/main
	// tracking ref above) so Run's post-approve PushNotes has somewhere to
	// push refs/notes/om, matching every real rig clone.
	bareDir := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare origin: %v\n%s", err, out)
	}
	if _, err := g.AddRemote("origin", bareDir); err != nil {
		t.Fatalf("add remote origin: %v", err)
	}

	head := commitFileReview(t, repoDir, "feature.txt", "hello\n", "add feature")

	rigDir := t.TempDir()
	binPath := filepath.Join(rigDir, "om-stub-binary")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho om\n"), 0755); err != nil {
		t.Fatalf("write om stub binary: %v", err)
	}
	sum := sha256.Sum256([]byte("#!/bin/sh\necho om\n"))
	var m Manifest
	m.OMBinary.Path = binPath
	m.OMBinary.SHA256 = hex.EncodeToString(sum[:])
	m.OMBinary.Version = "1.4.0"
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, manifestFileName), data, 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	return &reviewFixture{repoDir: repoDir, rigDir: rigDir, base: base, head: head}
}

func (f *reviewFixture) request() ReviewRequest {
	return ReviewRequest{
		RigDir:        f.rigDir,
		RepoDir:       f.repoDir,
		MRID:          "gt-mr-1",
		Worker:        "marble",
		Rig:           "gastown",
		Target:        "main",
		Branch:        "polecat/marble/gt-real",
		RehearsedHead: f.head,
		Attempt:       2,
		Config:        config.EditorialConfig{Required: true},
	}
}

func writeVerdict(t *testing.T, path string, verdict verdictJSON) {
	t.Helper()
	data, err := json.Marshal(verdict)
	if err != nil {
		t.Fatalf("marshal verdict: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write verdict: %v", err)
	}
}

// exec paths in the stub Exec funcs use the --out flag's value to find
// where to write the canned verdict.
func verdictPathFromArgs(args []string) string {
	for i, a := range args {
		if a == "--out" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestRun_ApproveWritesNoteAndReceiptAndReviewedHead(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.8, Verdict: "approve"})
			return "", 0, nil
		},
	}

	result := Run(context.Background(), fixture.request(), deps)

	if result.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 (stderr=%q class=%q)", result.Exit, result.Stderr, result.Class)
	}
	if result.Note == nil {
		t.Fatal("expected a note")
	}
	wantPatchID, err := deps.Git.PatchID(fixture.base, fixture.head)
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	if result.Note.PatchID != wantPatchID {
		t.Errorf("Note.PatchID = %q, want %q", result.Note.PatchID, wantPatchID)
	}
	if result.Note.Verdict != "approve" {
		t.Errorf("Note.Verdict = %q, want approve", result.Note.Verdict)
	}

	gotNote, err := ReadNote(deps.Git, fixture.head)
	if err != nil {
		t.Fatalf("ReadNote: %v", err)
	}
	if gotNote.PatchID != wantPatchID {
		t.Errorf("git note PatchID = %q, want %q", gotNote.PatchID, wantPatchID)
	}

	updated, err := store.GetIssue(context.Background(), "gt-mr-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	fields := beads.ParseMRFields(&beads.Issue{Description: updated.Description})
	if fields == nil || fields.EditorialReviewedHead != fixture.head {
		t.Errorf("editorial_reviewed_head = %v, want %s", fields, fixture.head)
	}
}

func TestRun_RequestChangesNoReviewedHeadChange(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{
				Score: 0.4, Verdict: "request_changes",
				Findings: []verdictFinding{{Title: "a", Severity: "minor"}, {Title: "b", Severity: "major"}},
			})
			return "", 1, nil
		},
	}

	result := Run(context.Background(), fixture.request(), deps)

	if result.Exit != 1 {
		t.Fatalf("Exit = %d, want 1", result.Exit)
	}
	if result.Note == nil || result.Note.Verdict != "request_changes" {
		t.Fatalf("Note = %+v, want verdict request_changes", result.Note)
	}

	updated, _ := store.GetIssue(context.Background(), "gt-mr-1")
	fields := beads.ParseMRFields(&beads.Issue{Description: updated.Description})
	if fields != nil && fields.EditorialReviewedHead != "" {
		t.Errorf("editorial_reviewed_head should be unset on request_changes, got %q", fields.EditorialReviewedHead)
	}
}

func TestRun_BackendTimeoutRetriesOnceThenFails(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	calls := 0
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, _ []string, _ string) (string, int, error) {
			calls++
			return "om: execution error: backend timed out", 2, nil
		},
	}

	result := Run(context.Background(), fixture.request(), deps)

	if calls != 2 {
		t.Fatalf("Exec called %d times, want 2 (one retry)", calls)
	}
	if result.Exit != 2 {
		t.Fatalf("Exit = %d, want 2", result.Exit)
	}
	if result.Class != BackendTimeout {
		t.Fatalf("Class = %q, want backend_timeout", result.Class)
	}
	if result.Retries != 1 {
		t.Fatalf("Retries = %d, want 1", result.Retries)
	}
	if result.Note != nil {
		t.Fatal("expected no note on a failure")
	}
	if _, err := ReadNote(deps.Git, fixture.head); err != git.ErrNoNote {
		t.Fatalf("ReadNote on head after a failure: got %v, want ErrNoNote", err)
	}
}

func TestRun_MalformedVerdictZeroByteRetriesThenFails(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	calls := 0
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			calls++
			if err := os.WriteFile(verdictPathFromArgs(args), nil, 0644); err != nil {
				t.Fatalf("write empty verdict: %v", err)
			}
			return "", 0, nil
		},
	}

	result := Run(context.Background(), fixture.request(), deps)

	if calls != 2 {
		t.Fatalf("Exec called %d times, want 2 (one retry)", calls)
	}
	if result.Exit != 2 || result.Class != MalformedVerdict {
		t.Fatalf("Exit/Class = %d/%q, want 2/malformed_verdict", result.Exit, result.Class)
	}
	if result.Retries != 1 {
		t.Fatalf("Retries = %d, want 1", result.Retries)
	}
}

func TestRun_BinaryMissingNoRetry(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	calls := 0
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, _ []string, _ string) (string, int, error) {
			calls++
			return "", -1, exec.ErrNotFound
		},
	}

	result := Run(context.Background(), fixture.request(), deps)

	if calls != 1 {
		t.Fatalf("Exec called %d times, want 1 (no retry)", calls)
	}
	if result.Exit != 2 || result.Class != BinaryMissing {
		t.Fatalf("Exit/Class = %d/%q, want 2/binary_missing", result.Exit, result.Class)
	}
}

func TestRun_VersionMismatchBeforeInvoke(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	// Corrupt the manifest's recorded sha so it no longer matches the binary.
	m, err := LoadManifest(fixture.rigDir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	m.OMBinary.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	data, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(fixture.rigDir, manifestFileName), data, 0644); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	calls := 0
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, _ []string, _ string) (string, int, error) {
			calls++
			return "", 0, nil
		},
	}

	result := Run(context.Background(), fixture.request(), deps)

	if calls != 0 {
		t.Fatalf("gate script invoked %d times, want 0 (version_mismatch caught before invoke)", calls)
	}
	if result.Exit != 2 || result.Class != VersionMismatch {
		t.Fatalf("Exit/Class = %d/%q, want 2/version_mismatch", result.Exit, result.Class)
	}
}

func TestRun_ConfigErrorNoRetry(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	calls := 0
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, _ []string, _ string) (string, int, error) {
			calls++
			return "om: config error: rubric missing criterion", 2, nil
		},
	}

	result := Run(context.Background(), fixture.request(), deps)

	if calls != 1 {
		t.Fatalf("Exec called %d times, want 1 (no retry)", calls)
	}
	if result.Exit != 2 || result.Class != ConfigError {
		t.Fatalf("Exit/Class = %d/%q, want 2/config_error", result.Exit, result.Class)
	}
}

func TestRun_NoteWriteFailureAfterVerdictIsRecordFailed(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))

	// Make .git read-only so NotesAdd's ref write fails while the reads
	// Run already performed (merge-base, patch-id) before this point stay
	// unaffected — standing in for a read-only notes ref.
	gitDir := filepath.Join(fixture.repoDir, ".git")
	if out, err := exec.Command("chmod", "-R", "a-w", gitDir).CombinedOutput(); err != nil {
		t.Fatalf("chmod -w .git: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("chmod", "-R", "u+w", gitDir).Run()
	})

	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.9, Verdict: "approve"})
			return "", 0, nil
		},
	}
	req := fixture.request()

	result := Run(context.Background(), req, deps)

	if result.Exit != 2 || result.Class != RecordFailed {
		t.Fatalf("Exit/Class = %d/%q, want 2/record_failed", result.Exit, result.Class)
	}
}

func TestRun_ApproveWithMajorFindingsFilesFollowups(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{
				Score: 0.75, Verdict: "approve",
				Findings: []verdictFinding{
					{Title: "leaky abstraction", Severity: "major", Path: "foo.go", Line: 10},
					{Title: "nit", Severity: "minor"},
				},
			})
			return "", 0, nil
		},
	}

	result := Run(context.Background(), fixture.request(), deps)

	if result.Exit != 0 {
		t.Fatalf("Exit = %d, want 0", result.Exit)
	}
	if len(result.Note.Followups) != 1 {
		t.Fatalf("Note.Followups = %v, want exactly 1 id", result.Note.Followups)
	}
	if _, ok := store.issues[result.Note.Followups[0]]; !ok {
		t.Fatalf("followup bead %s not found in store", result.Note.Followups[0])
	}
	if len(store.issues) != 2 { // MR bead + one followup
		t.Fatalf("store has %d issues, want 2 (MR + 1 followup)", len(store.issues))
	}

	raw, err := deps.Git.NotesShow(NotesRef, fixture.head)
	if err != nil {
		t.Fatalf("NotesShow: %v", err)
	}
	if !strings.Contains(raw, `"followups":["`+result.Note.Followups[0]+`"]`) {
		t.Fatalf("note JSON followups field missing or empty: %s", raw)
	}
}

// TestRun_ApproveFollowupFilingFailureSurfacedInStderr covers the majors
// finding that a follow-up filing failure on an approve verdict was silently
// swallowed: fileFollowups' error must still reach ReviewResult.Stderr so an
// approve-path caller can surface it, rather than the approve looking clean.
func TestRun_ApproveFollowupFilingFailureSurfacedInStderr(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	store.failCreate = true
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{
				Score: 0.75, Verdict: "approve",
				Findings: []verdictFinding{{Title: "leaky abstraction", Severity: "major"}},
			})
			return "", 0, nil
		},
	}

	result := Run(context.Background(), fixture.request(), deps)

	if result.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 (approval must still stand — DECISION 8)", result.Exit)
	}
	if len(result.Note.Followups) != 0 {
		t.Fatalf("Note.Followups = %v, want none (filing failed)", result.Note.Followups)
	}
	if result.Stderr == "" {
		t.Fatal("Stderr is empty, want the follow-up filing failure surfaced")
	}
}

func TestRun_RehearsesWhenNoRehearsedHeadGiven(t *testing.T) {
	fakeBDForReview(t)

	bareDir := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}

	repoDir := initTestRepo(t)
	g := git.NewGit(repoDir)
	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	if _, err := g.AddRemote("origin", bareDir); err != nil {
		t.Fatalf("add remote: %v", err)
	}
	renameCmd := exec.Command("git", "branch", "-M", "main")
	renameCmd.Dir = repoDir
	if out, err := renameCmd.CombinedOutput(); err != nil {
		t.Fatalf("rename branch to main: %v\n%s", err, out)
	}
	if err := g.Push("origin", "main", false); err != nil {
		t.Fatalf("push main: %v", err)
	}

	featureCmd := exec.Command("git", "checkout", "-b", "polecat/marble/gt-real", base)
	featureCmd.Dir = repoDir
	if out, err := featureCmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout feature branch: %v\n%s", err, out)
	}
	branchHead := commitFileReview(t, repoDir, "feature.txt", "hello\n", "add feature")
	if err := g.Push("origin", "polecat/marble/gt-real", false); err != nil {
		t.Fatalf("push feature branch: %v", err)
	}
	if err := g.Checkout("main"); err != nil {
		t.Fatalf("checkout main: %v", err)
	}

	rigDir := t.TempDir()
	binPath := filepath.Join(rigDir, "om-stub-binary")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho om\n"), 0755); err != nil {
		t.Fatalf("write om stub binary: %v", err)
	}
	sum := sha256.Sum256([]byte("#!/bin/sh\necho om\n"))
	var m Manifest
	m.OMBinary.Path = binPath
	m.OMBinary.SHA256 = hex.EncodeToString(sum[:])
	m.OMBinary.Version = "1.4.0"
	data, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(rigDir, manifestFileName), data, 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	store := newReviewStore(mrIssue("gt-mr-1", "polecat/marble/gt-real", "main", "gt-real", "gastown", "marble"))
	deps := Deps{
		Git:      g,
		Beads:    beads.NewWithStore(repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.8, Verdict: "approve"})
			return "", 0, nil
		},
	}
	req := ReviewRequest{
		RigDir:  rigDir,
		RepoDir: repoDir,
		MRID:    "gt-mr-1",
		Worker:  "marble",
		Rig:     "gastown",
		Target:  "main",
		Branch:  "polecat/marble/gt-real",
		Attempt: 1,
		Config:  config.EditorialConfig{Required: true},
	}

	result := Run(context.Background(), req, deps)

	if result.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 (stderr=%q class=%q)", result.Exit, result.Stderr, result.Class)
	}
	if result.Note.HeadSHA == base || result.Note.HeadSHA == "" {
		t.Fatalf("expected a rehearsed head distinct from base, got %q (base %q)", result.Note.HeadSHA, base)
	}
	if result.Note.HeadSHA == branchHead {
		t.Fatalf("rehearsed head should be a merge commit, not the bare branch head %q", branchHead)
	}

	// RepoDir here is the CLI's shared clone (real usage: the refinery's own
	// refinery/rig) — self-rehearsal must not leave it stranded on a
	// throwaway gt-mq-review-* branch, and must not leave that branch lying
	// around either.
	branch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if branch != "main" {
		t.Fatalf("RepoDir left on branch %q after rehearsal, want main", branch)
	}
	leftover, err := g.ListBranches("gt-mq-review-*")
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if len(leftover) != 0 {
		t.Fatalf("rehearsal temp branch(es) not cleaned up: %v", leftover)
	}
}

// TestRun_ResolvesRehearsedRefToSHA covers the --rehearsed CLI flag, which
// accepts any ref (branch name, not necessarily a sha already). Note.HeadSHA
// and editorial_reviewed_head must record the resolved commit sha so they
// still identify the reviewed commit after the ref moves or is deleted —
// not the ref name itself.
func TestRun_ResolvesRehearsedRefToSHA(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	g := git.NewGit(fixture.repoDir)
	if err := g.CreateBranchFrom("rehearsed-ref", fixture.head); err != nil {
		t.Fatalf("create rehearsed-ref branch: %v", err)
	}

	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	deps := Deps{
		Git:      g,
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.8, Verdict: "approve"})
			return "", 0, nil
		},
	}
	req := fixture.request()
	req.RehearsedHead = "rehearsed-ref"

	result := Run(context.Background(), req, deps)

	if result.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 (stderr=%q class=%q)", result.Exit, result.Stderr, result.Class)
	}
	if result.Note.HeadSHA != fixture.head {
		t.Errorf("Note.HeadSHA = %q, want resolved sha %q, not the ref name", result.Note.HeadSHA, fixture.head)
	}

	updated, err := store.GetIssue(context.Background(), "gt-mr-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	fields := beads.ParseMRFields(&beads.Issue{Description: updated.Description})
	if fields == nil || fields.EditorialReviewedHead != fixture.head {
		t.Errorf("editorial_reviewed_head = %v, want resolved sha %s, not the ref name", fields, fixture.head)
	}
}
