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
	// originDir is the bare repo repoDir's origin remote points at — the
	// notes ref Run pushes to, and so the place a concurrent writer's notes
	// race shows up.
	originDir string
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

	return &reviewFixture{repoDir: repoDir, rigDir: rigDir, base: base, head: head, originDir: bareDir}
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

// TestRun_ApproveRecordsResolvedBackendOnNote is the gt-iqr6 visibility
// fix: when om's verdict reports which backend it resolved and invoked, that
// identity is copied through onto the note, so a backend swap is auditable
// from refs/notes/om after the fact even though the backend itself is never
// pinned by the rig manifest (see Manifest's doc comment for why).
func TestRun_ApproveRecordsResolvedBackendOnNote(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	const wantBackend = "claude-deepseek-pro -p"
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.8, Verdict: "approve", Backend: wantBackend})
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
	if result.Note.ResolvedBackend != wantBackend {
		t.Errorf("Note.ResolvedBackend = %q, want %q", result.Note.ResolvedBackend, wantBackend)
	}

	gotNote, err := ReadNote(deps.Git, fixture.head)
	if err != nil {
		t.Fatalf("ReadNote: %v", err)
	}
	if gotNote.ResolvedBackend != wantBackend {
		t.Errorf("git note resolved_backend = %q, want %q", gotNote.ResolvedBackend, wantBackend)
	}
}

// TestRun_ApproveWithNoBackendReportedLeavesNoteFieldEmpty is the
// compatibility case: an om version that predates the verdict's Backend
// field leaves it unset, and the note's ResolvedBackend must stay empty
// (and so omitted from the JSON) exactly as it did before this field
// existed.
func TestRun_ApproveWithNoBackendReportedLeavesNoteFieldEmpty(t *testing.T) {
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
	if result.Note.ResolvedBackend != "" {
		t.Errorf("Note.ResolvedBackend = %q, want empty when om reports no backend", result.Note.ResolvedBackend)
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
				Findings: []Finding{{Title: "a", Severity: "minor"}, {Title: "b", Severity: "major"}},
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

// TestRun_TimeoutOverrideReachesGateArgsAndNote pins both halves of the
// ReviewRequest.TimeoutSeconds contract: a set override is appended to the
// gate-script args (which the gate script forwards to `om review
// --timeout`) and recorded on the note, and — the half that protects
// against a regression — an unset override appends NOTHING. The deployed
// gate script parses its args with a strict case and exits 2 on any
// unknown argument, so a --timeout that leaked into the default path would
// turn every ordinary review on an older script into a fail-closed infra
// failure.
func TestRun_TimeoutOverrideReachesGateArgsAndNote(t *testing.T) {
	cases := []struct {
		name       string
		timeout    int
		wantArg    string
		wantValue  int
		wantInJSON bool
	}{
		{name: "override", timeout: 900, wantArg: "--timeout", wantValue: 900, wantInJSON: true},
		{name: "default passes no flag", timeout: 0, wantArg: "", wantValue: 0, wantInJSON: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeBDForReview(t)
			fixture := newReviewFixture(t)
			store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))

			var gotArgs []string
			deps := Deps{
				Git:      git.NewGit(fixture.repoDir),
				Beads:    beads.NewWithStore(fixture.repoDir, store),
				Recorder: plugin.NewRecorder(t.TempDir()),
				Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
					gotArgs = append([]string(nil), args...)
					writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.8, Verdict: "approve"})
					return "", 0, nil
				},
			}

			req := fixture.request()
			req.TimeoutSeconds = tc.timeout
			result := Run(context.Background(), req, deps)
			if result.Exit != 0 {
				t.Fatalf("Exit = %d, want 0 (stderr=%q class=%q)", result.Exit, result.Stderr, result.Class)
			}

			gotValue, gotFlag := "", ""
			for i, a := range gotArgs {
				if a == "--timeout" {
					gotFlag = a
					if i+1 < len(gotArgs) {
						gotValue = gotArgs[i+1]
					}
				}
			}
			if tc.wantArg == "" {
				if gotFlag != "" {
					t.Errorf("gate script args contain %q %q, want no timeout flag on the default path (args=%v)", gotFlag, gotValue, gotArgs)
				}
			} else if want := fmt.Sprintf("%d", tc.wantValue); gotFlag != tc.wantArg || gotValue != want {
				t.Errorf("gate script args carry %q %q, want %q %q (args=%v)", gotFlag, gotValue, tc.wantArg, want, gotArgs)
			}

			if result.Note == nil {
				t.Fatal("expected a note")
			}
			if result.Note.TimeoutSeconds != tc.wantValue {
				t.Errorf("Note.TimeoutSeconds = %d, want %d", result.Note.TimeoutSeconds, tc.wantValue)
			}

			// The note on disk is what a later reader audits, so assert the
			// committed artifact, not just the in-memory struct.
			raw, err := deps.Git.NotesShow(NotesRef, fixture.head)
			if err != nil {
				t.Fatalf("NotesShow: %v", err)
			}
			if got := strings.Contains(raw, `"timeout_seconds"`); got != tc.wantInJSON {
				t.Errorf("note contains timeout_seconds = %v, want %v (note=%s)", got, tc.wantInJSON, raw)
			}
		})
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

// TestClassifyOutcome_GateUsageError pins the marker ordering: om-gate.sh's
// usage_error() prints its whole flag list, and that list advertises
// --timeout, so a substring search for "timeout" reads every rejected
// invocation as a backend timeout (gt-o6xh).
func TestClassifyOutcome_GateUsageError(t *testing.T) {
	// Byte-for-byte the line om-gate.sh's usage_error() prints, so the search
	// this guards is the one the deployed script actually feeds it.
	usageLine := "om-gate: usage: om-gate.sh --base <ref> --head <ref> [--dir <path>] " +
		"[--mr <bead-id>] [--worker <name>] [--rig <name>] [--out <file>] " +
		"[--prior-findings <file>] [--timeout <seconds>] [--fail-closed]\n"
	tests := []struct {
		name   string
		exit   int
		stderr string
		want   FailureClass
	}{
		{
			// The gt-o6xh repro: a gate that predates the flag.
			name:   "unknown argument --timeout",
			exit:   2,
			stderr: "om-gate: usage error: unknown argument: --timeout\n" + usageLine,
			want:   ConfigError,
		},
		{
			// A gate that DOES parse --timeout still prints it in the usage
			// line, so the word alone never distinguishes the two.
			name:   "missing required flag",
			exit:   2,
			stderr: "om-gate: usage error: --base is required\n" + usageLine,
			want:   ConfigError,
		},
		{
			name: "a real backend timeout is unchanged",
			exit: 2,
			stderr: "om-gate: reviewing abc..def in .\n" +
				"om-gate: backend timeout override: 1800s (forwarded to om review)\n" +
				"om-gate: execution error: om review exited 2: om: execution error: backend timed out\n" +
				"om-gate: fail-closed: blocking the merge (exit 2)\n",
			want: BackendTimeout,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			verdictPath := filepath.Join(t.TempDir(), "verdict.json")
			_, class, err := classifyOutcome(nil, tc.exit, tc.stderr, verdictPath)
			if class != tc.want {
				t.Fatalf("class = %q, want %q (err=%v)", class, tc.want, err)
			}
		})
	}
}

// TestRun_GateRejectingTimeoutFlagDoesNotRetry is the end-to-end gt-o6xh
// repro. A usage error is deterministic, so the gate must run once and the
// failure must name the caller's mistake, not a backend that never ran.
func TestRun_GateRejectingTimeoutFlagDoesNotRetry(t *testing.T) {
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
			return "om-gate: usage error: unknown argument: --timeout\n" +
				"om-gate: usage: om-gate.sh --base <ref> --head <ref> [--timeout <seconds>]\n", 2, nil
		},
	}

	req := fixture.request()
	req.TimeoutSeconds = 1800
	result := Run(context.Background(), req, deps)

	if calls != 1 {
		t.Fatalf("Exec called %d times, want 1 (a usage error is deterministic)", calls)
	}
	if result.Class != ConfigError {
		t.Fatalf("Class = %q, want config_error", result.Class)
	}
	if result.Exit != 2 {
		t.Fatalf("Exit = %d, want 2", result.Exit)
	}
	if !strings.Contains(result.Stderr, "unknown argument: --timeout") {
		t.Errorf("Stderr = %q, want the gate's own rejection", result.Stderr)
	}
	if result.Note != nil {
		t.Fatal("expected no note on a failure")
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

// writeManagedFile writes content to rigDir/relPath and records its sha256
// in the fixture's manifest.Files, so tests can exercise AssertVersion's
// managed-harness-file check (gt-qxot) — the same check
// `gt doctor harness-drift` performs — independently of the om binary and
// rubric checks.
func (f *reviewFixture) writeManagedFile(t *testing.T, relPath, content string) {
	t.Helper()
	fullPath := filepath.Join(f.rigDir, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		t.Fatalf("mkdir for managed file %s: %v", relPath, err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		t.Fatalf("write managed file %s: %v", relPath, err)
	}
	sum := sha256.Sum256([]byte(content))

	m, err := LoadManifest(f.rigDir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	m.Files[relPath] = hex.EncodeToString(sum[:])
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.rigDir, manifestFileName), data, 0644); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}
}

func TestRun_ManagedFileDriftBeforeInvoke(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	fixture.writeManagedFile(t, "scripts/om-gate.sh", "#!/bin/sh\necho gate\n")
	// Hand-edit the managed file after the manifest recorded its sha —
	// harness drift (gt-qxot's repro): a hand-edited gate script must be
	// caught here, before it is ever invoked.
	if err := os.WriteFile(filepath.Join(fixture.rigDir, "scripts/om-gate.sh"), []byte("#!/bin/sh\necho hacked\n"), 0644); err != nil {
		t.Fatalf("hand-edit managed file: %v", err)
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

func TestRun_ManagedFileMissingBeforeInvoke(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	fixture.writeManagedFile(t, "scripts/om-gate.sh", "#!/bin/sh\necho gate\n")
	// The manifest records the file but it's gone from disk.
	if err := os.Remove(filepath.Join(fixture.rigDir, "scripts/om-gate.sh")); err != nil {
		t.Fatalf("remove managed file: %v", err)
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

func TestRun_ManagedFilesInSyncInvokesGate(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	fixture.writeManagedFile(t, "scripts/om-gate.sh", "#!/bin/sh\necho gate\n")
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	calls := 0
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			calls++
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 1, Verdict: "approve"})
			return "", 0, nil
		},
	}

	result := Run(context.Background(), fixture.request(), deps)

	if calls != 1 {
		t.Fatalf("gate script invoked %d times, want 1 (managed files in sync)", calls)
	}
	if result.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 (approve; stderr=%q class=%q)", result.Exit, result.Stderr, result.Class)
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

// foreignNotesWriter pushes a note for a fresh commit of its own from a clone
// that shares only origin with the fixture repo — standing in for the other
// writers of refs/notes/om this path races in production (a batch backfill,
// another rig's refinery, a concurrent `gt mq review`). It returns the
// commit it annotated.
func foreignNotesWriter(t *testing.T, originDir, content string) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "-c", "protocol.file.allow=always", "clone", originDir, dir).CombinedOutput(); err != nil {
		t.Fatalf("clone origin: %v\n%s", err, out)
	}
	for _, kv := range [][2]string{{"user.email", "test@test.com"}, {"user.name", "Test User"}} {
		cmd := exec.Command("git", "config", kv[0], kv[1])
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v\n%s", kv[0], err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "foreign.txt"), []byte("foreign\n"), 0644); err != nil {
		t.Fatalf("write foreign.txt: %v", err)
	}
	for _, args := range [][]string{{"add", "foreign.txt"}, {"commit", "-m", "foreign commit"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	g := git.NewGit(dir)
	head, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	if err := g.NotesAdd(NotesRef, head, content); err != nil {
		t.Fatalf("NotesAdd: %v", err)
	}
	if err := g.PushNotes("origin", NotesRef); err != nil {
		t.Fatalf("foreign PushNotes: %v", err)
	}
	return head
}

// notesPublishedOnOrigin returns the notes origin currently serves, keyed by
// annotated object, read from a clone that shares nothing else with the
// fixture repo.
func notesPublishedOnOrigin(t *testing.T, originDir string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "-c", "protocol.file.allow=always", "clone", originDir, dir).CombinedOutput(); err != nil {
		t.Fatalf("clone origin: %v\n%s", err, out)
	}
	g := git.NewGit(dir)
	if err := g.FetchNotes("origin", NotesRef); err != nil {
		t.Fatalf("FetchNotes: %v", err)
	}
	entries, err := g.NotesList(NotesRef)
	if err != nil {
		t.Fatalf("NotesList: %v", err)
	}
	published := make(map[string]string, len(entries))
	for _, e := range entries {
		published[e.Annotated] = e.Content
	}
	return published
}

// TestRun_ApproveSurvivesConcurrentNotesPush covers gt-2rcx end to end: a
// verdict note the rig wrote must still reach origin when another writer
// pushed to refs/notes/om since this clone's last look. Git rejects that push
// non-fast-forward, and classifying it record_failed exited 2 on a race the
// notes ref resolves for free — so Run must land both notes instead.
func TestRun_ApproveSurvivesConcurrentNotesPush(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	foreignHead := foreignNotesWriter(t, fixture.originDir, `{"mr":"mr-foreign"}`)

	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.9, Verdict: "approve"})
			return "", 0, nil
		},
	}

	result := Run(context.Background(), fixture.request(), deps)

	if result.Exit != 0 || result.Class != "" {
		t.Fatalf("Exit/Class = %d/%q, want 0 (a notes push race must not be record_failed; stderr=%q)", result.Exit, result.Class, result.Stderr)
	}
	if _, err := ReadNote(deps.Git, fixture.head); err != nil {
		t.Fatalf("verdict note unreadable locally after the race: %v", err)
	}

	published := notesPublishedOnOrigin(t, fixture.originDir)
	if _, ok := published[fixture.head]; !ok {
		t.Errorf("the verdict note never reached origin (published: %v)", keysOf(published))
	}
	if _, ok := published[foreignHead]; !ok {
		t.Errorf("the other writer's note was clobbered (published: %v)", keysOf(published))
	}
}

func keysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
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
				Findings: []Finding{
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
				Findings: []Finding{{Title: "leaky abstraction", Severity: "major"}},
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

// TestRun_ApproveCleanDoesNotLeakGateScriptStderr covers the companion bug: a
// clean approve (no majors, no follow-up filing failure) must not surface the
// gate script's own raw stderr as ReviewResult.Stderr. Callers (mq_review.go,
// batch_editorial.go) treat any non-empty Stderr on an approve as "approved
// with warning" and print it verbatim, so a gate script that merely logs
// diagnostics to stderr on a clean run must not make every approve look
// suspect (gt-evk4).
func TestRun_ApproveCleanDoesNotLeakGateScriptStderr(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.8, Verdict: "approve"})
			return "gate-script: routine diagnostic chatter, not an error", 0, nil
		},
	}

	result := Run(context.Background(), fixture.request(), deps)

	if result.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 (stderr=%q class=%q)", result.Exit, result.Stderr, result.Class)
	}
	if result.Stderr != "" {
		t.Fatalf("Stderr = %q, want empty — a clean approve must not surface the gate script's routine stderr", result.Stderr)
	}
}

// TestRehearsal_MergeConflictLeavesLiveCloneUntouched: because a rehearsal runs
// in its own worktree rather than in the clone it is handed, a conflict must
// leave that clone exactly as it found it — same detached HEAD, no branch
// created, no worktree registration left behind. The failure modes this
// replaces are gt-evk4 (clone stranded on a doomed rehearsal branch) and
// gt-kmul (a mid-gate clone restored onto the target name).
func TestRehearsal_MergeConflictLeavesLiveCloneUntouched(t *testing.T) {
	dir := initTestRepo(t)
	g := git.NewGit(dir)
	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}

	mainHead := commitFileReview(t, dir, "conflict.txt", "main version\n", "main change")
	setTestRef(t, dir, "refs/remotes/origin/main", mainHead)

	// Branch off base (not mainHead) so the merge actually conflicts instead
	// of fast-forwarding.
	if err := g.CheckoutDetach(base); err != nil {
		t.Fatalf("checkout base: %v", err)
	}
	branchHead := commitFileReview(t, dir, "conflict.txt", "branch version\n", "branch change")
	setTestRef(t, dir, "refs/remotes/origin/feature", branchHead)

	// Leave the repo detached at base before rehearsing — the rehearsal must
	// not depend on, or disturb, whatever was checked out beforehand.
	if err := g.CheckoutDetach(base); err != nil {
		t.Fatalf("re-detach at base: %v", err)
	}
	before := branchList(t, g)
	worktreesBefore, wtErr := g.WorktreeList()
	if wtErr != nil {
		t.Fatalf("WorktreeList: %v", wtErr)
	}

	rehearsal, err := BeginRehearsal(g, "main")
	if err != nil {
		t.Fatalf("BeginRehearsal: %v", err)
	}
	if _, err := rehearsal.Branch("feature"); err == nil {
		t.Fatal("Rehearsal.Branch succeeded, want a merge conflict error")
	}
	if closeErr := rehearsal.Close(); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}

	current, cbErr := g.CurrentBranch()
	if cbErr != nil {
		t.Fatalf("CurrentBranch: %v", cbErr)
	}
	if current != "HEAD" {
		t.Fatalf("CurrentBranch = %q, want %q (detached) — rehearsal moved the live clone's HEAD", current, "HEAD")
	}
	head, revErr := g.Rev("HEAD")
	if revErr != nil {
		t.Fatalf("rev HEAD: %v", revErr)
	}
	if head != base {
		t.Fatalf("HEAD = %s, want %s (base, where the live clone already was) after a failed rehearsal", head, base)
	}

	if after := branchList(t, g); after != before {
		t.Fatalf("branches = %q, want %q unchanged — rehearsal created or deleted a branch in the live clone", after, before)
	}
	worktreesAfter, wt2Err := g.WorktreeList()
	if wt2Err != nil {
		t.Fatalf("WorktreeList: %v", wt2Err)
	}
	if len(worktreesAfter) != len(worktreesBefore) {
		t.Fatalf("worktrees = %v, want %v — a rehearsal worktree was left registered", worktreesAfter, worktreesBefore)
	}
}

// branchList returns the live clone's branches as one comparable string.
func branchList(t *testing.T, g *git.Git) string {
	t.Helper()
	branches, err := g.ListBranches("*")
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	return strings.Join(branches, "\n")
}

func setTestRef(t *testing.T, dir, ref, sha string) {
	t.Helper()
	cmd := exec.Command("git", "update-ref", ref, sha)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-ref %s: %v\n%s", ref, err, out)
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

// TestRun_RehearsalNeverMovesRepoDirOffItsOriginalCheckout reproduces
// gt-dcku: the refinery's live clone (RepoDir) can be mid-gate on its own
// branch (e.g. a batch's "temp") when `gt mq review/retry` rehearses a
// different MR. The old implementation created a temp branch in RepoDir,
// checked it out, then "restored" by checking out the TARGET NAME — which
// silently moved RepoDir's HEAD onto stale local "main" instead of back to
// "temp", so a concurrent build/test run silently tested the wrong tree.
// Rehearsal must never touch RepoDir's checkout at all.
func TestRun_RehearsalNeverMovesRepoDirOffItsOriginalCheckout(t *testing.T) {
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
	commitFileReview(t, repoDir, "feature.txt", "hello\n", "add feature")
	if err := g.Push("origin", "polecat/marble/gt-real", false); err != nil {
		t.Fatalf("push feature branch: %v", err)
	}

	// Simulate the refinery being mid-gate on its own "temp" branch — a
	// stale local "main" (never updated after the initial push) stands in
	// for the pre-uq28 tree the real incident silently re-tested.
	if err := g.CreateBranchFrom("temp", "polecat/marble/gt-real"); err != nil {
		t.Fatalf("create temp branch: %v", err)
	}
	if err := g.Checkout("temp"); err != nil {
		t.Fatalf("checkout temp: %v", err)
	}
	tempHead, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev temp HEAD: %v", err)
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
		t.Fatalf("expected a rehearsed head distinct from base, got %q", result.Note.HeadSHA)
	}

	branch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if branch != "temp" {
		t.Fatalf("RepoDir moved off its original branch: now on %q, want temp", branch)
	}
	head, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	if head != tempHead {
		t.Fatalf("RepoDir HEAD moved: now %q, want unchanged temp head %q", head, tempHead)
	}
	leftover, err := g.ListBranches("gt-mq-review-*")
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if len(leftover) != 0 {
		t.Fatalf("rehearsal temp branch(es) leaked into RepoDir: %v", leftover)
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

// The gate script must diff from the merge-base, never from origin/<target>:
// when main moves while a rehearsal is under review, a two-dot
// origin/main..head diff shows main's own new commits as deletions and om
// rejects the branch for work it never touched (two phantom rejections on
// 2026-09-19, gt-x1x3).
func TestRun_GateBaseIsMergeBaseNotMovedTarget(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)

	// origin/main moves past the rehearsal's base after the head was cut.
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = fixture.repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	tree := run("rev-parse", fixture.base+"^{tree}")
	moved := run("commit-tree", tree, "-p", fixture.base, "-m", "main moved during the gate")
	run("update-ref", "refs/remotes/origin/main", moved)

	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	var gateBase string
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			for i, a := range args {
				if a == "--base" && i+1 < len(args) {
					gateBase = args[i+1]
				}
			}
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.8, Verdict: "approve"})
			return "", 0, nil
		},
	}

	result := Run(context.Background(), fixture.request(), deps)

	if result.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 (stderr=%q)", result.Exit, result.Stderr)
	}
	if gateBase != fixture.base {
		t.Errorf("gate --base = %q, want the merge-base sha %s (origin/main moved to %s)", gateBase, fixture.base, moved[:7])
	}
	if result.Note.BaseSHA != fixture.base {
		t.Errorf("Note.BaseSHA = %q, want %s", result.Note.BaseSHA, fixture.base)
	}
	// BaseSHA (the merge-base) does not move as target advances past it —
	// that is the point of the test above. ReviewedTargetTip is the field
	// that does: it must record target's actual tip at review time (moved),
	// not the merge-base, or the push precondition's drift check (gt-6bsp)
	// has nothing to compare against.
	if result.Note.ReviewedTargetTip != moved {
		t.Errorf("Note.ReviewedTargetTip = %q, want origin/main's current tip %s", result.Note.ReviewedTargetTip, moved)
	}
}

// TestRun_NoteRecordsReviewedTargetTip_Unmoved is the baseline for the
// moved-target test above: when target has not moved since the branch was
// cut, ReviewedTargetTip and BaseSHA agree.
func TestRun_NoteRecordsReviewedTargetTip_Unmoved(t *testing.T) {
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
		t.Fatalf("Exit = %d, want 0 (stderr=%q)", result.Exit, result.Stderr)
	}
	if result.Note.ReviewedTargetTip != fixture.base {
		t.Errorf("Note.ReviewedTargetTip = %q, want %s (target has not moved)", result.Note.ReviewedTargetTip, fixture.base)
	}
}

// recordVerdict attaches n as the note on fixture's head, standing in for the
// review that produced it.
func recordVerdict(t *testing.T, fixture *reviewFixture, n Note) {
	t.Helper()
	n.HeadSHA = fixture.head
	if err := WriteNote(git.NewGit(fixture.repoDir), n); err != nil {
		t.Fatalf("WriteNote: %v", err)
	}
}

// recordedNoteFor builds the note a first review of fixture's head would have
// written, with the patch-id Run itself computes for that head.
func recordedNoteFor(t *testing.T, fixture *reviewFixture, score float64, verdict string) Note {
	t.Helper()
	patchID, err := git.NewGit(fixture.repoDir).PatchID(fixture.base, fixture.head)
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	return Note{
		OMVersion:  "1.4.0",
		Rig:        "gastown",
		MR:         "gt-mr-1",
		Worker:     "marble",
		BaseSHA:    fixture.base,
		HeadSHA:    fixture.head,
		PatchID:    patchID,
		Score:      score,
		Verdict:    verdict,
		Attempt:    1,
		ReviewedAt: time.Now().UTC().Add(-time.Minute),
	}
}

// TestRun_ReusesRecordedVerdictOnUnchangedHead pins the gt-bveg guard: a head
// that already carries a verdict for this diff and this rubric is answered
// from the note, so the gate is not re-invoked and a near-threshold score
// cannot be re-rolled into a different verdict.
func TestRun_ReusesRecordedVerdictOnUnchangedHead(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	recordVerdict(t, fixture, recordedNoteFor(t, fixture, 0.74, "approve"))

	execCalls := 0
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			execCalls++
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.50, Verdict: "request_changes"})
			return "", 1, nil
		},
	}

	result := Run(context.Background(), fixture.request(), deps)

	if execCalls != 0 {
		t.Errorf("gate invoked %d time(s), want 0: this diff already has a verdict", execCalls)
	}
	if result.Exit != 0 || !result.Reused {
		t.Fatalf("Exit/Reused = %d/%v, want 0/true (stderr=%q)", result.Exit, result.Reused, result.Stderr)
	}
	if result.Note.Score != 0.74 || result.Note.Verdict != "approve" {
		t.Errorf("Note = %.2f/%s, want the recorded 0.74/approve", result.Note.Score, result.Note.Verdict)
	}
	got, err := ReadNote(deps.Git, fixture.head)
	if err != nil {
		t.Fatalf("ReadNote: %v", err)
	}
	if got.Score != 0.74 || got.Verdict != "approve" {
		t.Errorf("note on disk = %.2f/%s, want the recorded 0.74/approve", got.Score, got.Verdict)
	}
	if got.Attempts != nil {
		t.Errorf("Attempts = %+v, want none: a reused verdict is not a fresh attempt", got.Attempts)
	}
	// The push precondition reads the MR bead for the reviewed head, so an
	// approve answered from the note must still leave it pointing there.
	updated, err := store.GetIssue(context.Background(), "gt-mr-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	fields := beads.ParseMRFields(&beads.Issue{Description: updated.Description})
	if fields == nil || fields.EditorialReviewedHead != fixture.head {
		t.Errorf("editorial_reviewed_head = %v, want %s", fields, fixture.head)
	}
}

// TestRun_ReusesRecordedRejectionWithoutRecordingReviewedHead is the other
// half: a rejected head answered from its note stays rejected, and still
// leaves no reviewed head behind (gt-wx2p owns making that visible to the
// queue; this only refuses to launder the rejection into an approval).
func TestRun_ReusesRecordedRejectionWithoutRecordingReviewedHead(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	recordVerdict(t, fixture, recordedNoteFor(t, fixture, 0.50, "request_changes"))

	execCalls := 0
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			execCalls++
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.80, Verdict: "approve"})
			return "", 0, nil
		},
	}

	result := Run(context.Background(), fixture.request(), deps)

	if execCalls != 0 {
		t.Errorf("gate invoked %d time(s), want 0: this diff already has a verdict", execCalls)
	}
	if result.Exit != 1 || !result.Reused {
		t.Fatalf("Exit/Reused = %d/%v, want 1/true (stderr=%q)", result.Exit, result.Reused, result.Stderr)
	}
	updated, _ := store.GetIssue(context.Background(), "gt-mr-1")
	fields := beads.ParseMRFields(&beads.Issue{Description: updated.Description})
	if fields != nil && fields.EditorialReviewedHead != "" {
		t.Errorf("editorial_reviewed_head = %q, want unset on a rejected head", fields.EditorialReviewedHead)
	}
}

// reRehearsedHead returns a second commit carrying the same diff as
// fixture.head — same tree, same parent, different message — which is the
// shape a fresh rehearsal of an unchanged branch produces. gt mq review merges
// the branch onto its target in a throwaway worktree on every invocation, so a
// second invocation's head is never the first's commit even when the diff is
// byte-identical. That head movement is what the reuse lookup has to survive
// (gt-qa2p), and the patch-id is asserted unchanged so a test built on this
// head is about a moved head and not a changed diff. label distinguishes one
// re-rehearsal from the next: without it two invocations in the same second
// would build the identical commit.
func reRehearsedHead(t *testing.T, fixture *reviewFixture, label string) string {
	t.Helper()
	g := git.NewGit(fixture.repoDir)
	tree, err := g.Rev(fixture.head + "^{tree}")
	if err != nil {
		t.Fatalf("rev tree of %s: %v", fixture.head, err)
	}
	msg := fmt.Sprintf("rehearsal merge for om review (%s)", label)
	cmd := exec.Command("git", "commit-tree", tree, "-p", fixture.base, "-m", msg)
	cmd.Dir = fixture.repoDir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("commit-tree: %v", err)
	}
	head := strings.TrimSpace(string(out))
	if head == fixture.head {
		t.Fatalf("rehearsed head is %s, the same commit as the fixture's: the test needs a different one", head)
	}
	before, err := g.PatchID(fixture.base, fixture.head)
	if err != nil {
		t.Fatalf("PatchID of the first head: %v", err)
	}
	after, err := g.PatchID(fixture.base, head)
	if err != nil {
		t.Fatalf("PatchID of the second head: %v", err)
	}
	if before != after {
		t.Fatalf("patch-id moved with the commit (%s -> %s): the test needs an unchanged diff", before, after)
	}
	return head
}

// TestRun_ReusesRecordedVerdictOnANewRehearsalHead is the gt-qa2p regression:
// the same diff rehearsed onto a fresh head is answered from its recorded
// verdict, so a caller cannot re-roll a rejected diff by invoking the command
// again. Before this, three invocations against one MR produced three verdicts
// for one patch-id (0.56, 0.62, 0.84 against a 0.60 threshold) because the
// lookup was keyed to a rehearsal head that is new every time.
//
// The head recorded for the push precondition is asserted too: it must name
// the commit the note is on, not this invocation's rehearsal commit, or the
// precondition would refuse to push an MR the gate just approved. That
// recorded head can be a commit that is not an ancestor of the branch tip
// (head2, the rehearsal commit this invocation produced, is a sibling of
// fixture.head): CheckPrecondition reads the note by sha and compares
// patch-id, never ancestry, so that relationship does not matter here. Which
// MR a note belongs to is a separate concern this reuse path does not
// enforce — see resume_landed_merge.go's ensureLandedEditorialNote (gt-bagu).
func TestRun_ReusesRecordedVerdictOnANewRehearsalHead(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	recordVerdict(t, fixture, recordedNoteFor(t, fixture, 0.74, "approve"))
	head2 := reRehearsedHead(t, fixture, "second invocation")

	execCalls := 0
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			execCalls++
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.50, Verdict: "request_changes"})
			return "", 1, nil
		},
	}
	req := fixture.request()
	req.RehearsedHead = head2

	result := Run(context.Background(), req, deps)

	if execCalls != 0 {
		t.Errorf("gate invoked %d time(s), want 0: this diff already has a verdict, whatever commit it was rehearsed onto", execCalls)
	}
	if result.Exit != 0 || !result.Reused {
		t.Fatalf("Exit/Reused = %d/%v, want 0/true (stderr=%q)", result.Exit, result.Reused, result.Stderr)
	}
	if result.Note.Score != 0.74 || result.Note.Verdict != "approve" {
		t.Errorf("Note = %.2f/%s, want the recorded 0.74/approve", result.Note.Score, result.Note.Verdict)
	}

	updated, err := store.GetIssue(context.Background(), "gt-mr-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	fields := beads.ParseMRFields(&beads.Issue{Description: updated.Description})
	if fields == nil || fields.EditorialReviewedHead != fixture.head {
		t.Fatalf("editorial_reviewed_head = %+v, want the reviewed head %s (the commit the note is on)", fields, fixture.head)
	}
	// The precondition reads the note by that head, so the recorded value has
	// to be a commit this clone can actually read the note from.
	if _, err := ReadNote(deps.Git, fields.EditorialReviewedHead); err != nil {
		t.Errorf("ReadNote at the recorded reviewed head: %v", err)
	}
	if _, perr := CheckPrecondition(deps.Git, req.Config, []LandedMR{{
		MRID:         "gt-mr-1",
		ReviewedHead: fields.EditorialReviewedHead,
		Base:         fixture.base,
		Head:         head2,
	}}); perr != nil {
		t.Errorf("CheckPrecondition after a reused approve = %s, want the push to be authorized", perr.Reason)
	}
}

// TestRun_ReusesRecordedRejectionOnANewRehearsalHead is the other half: a
// rejected diff stays rejected when it is rehearsed onto a new head, and still
// leaves no reviewed head behind.
func TestRun_ReusesRecordedRejectionOnANewRehearsalHead(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	recordVerdict(t, fixture, recordedNoteFor(t, fixture, 0.50, "request_changes"))
	head2 := reRehearsedHead(t, fixture, "second invocation")

	execCalls := 0
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			execCalls++
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.84, Verdict: "approve"})
			return "", 0, nil
		},
	}
	req := fixture.request()
	req.RehearsedHead = head2

	result := Run(context.Background(), req, deps)

	if execCalls != 0 {
		t.Errorf("gate invoked %d time(s), want 0: this diff already has a verdict", execCalls)
	}
	if result.Exit != 1 || !result.Reused {
		t.Fatalf("Exit/Reused = %d/%v, want 1/true (stderr=%q)", result.Exit, result.Reused, result.Stderr)
	}
	updated, _ := store.GetIssue(context.Background(), "gt-mr-1")
	if fields := beads.ParseMRFields(&beads.Issue{Description: updated.Description}); fields != nil && fields.EditorialReviewedHead != "" {
		t.Errorf("editorial_reviewed_head = %q, want unset on a rejected diff", fields.EditorialReviewedHead)
	}
}

// TestRun_DriftedApproveNoteIsNotReusedAfterRefusal is the gt-6bsp livelock
// regression: an approve note whose ReviewedTargetTip predates a target move
// onto a file the MR's own diff also touches must not be reused on the next
// rehearsal, even though the MR's own diff (patch-id) is byte-identical to
// what was approved. Before this, the reuse path in Run only checked
// verdict/patch-id/rubric/version (recordedVerdictApplies) and answered from
// the stale note every time, so CheckPrecondition's ReasonTargetDriftMaterial
// refusal never saw a fresh ReviewedTargetTip and refused forever — the queue
// promise that "the next review writes a fresh note" was never kept.
func TestRun_DriftedApproveNoteIsNotReusedAfterRefusal(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))

	note := recordedNoteFor(t, fixture, 0.8, "approve")
	note.ReviewedTargetTip = fixture.base // target had not moved at review time
	recordVerdict(t, fixture, note)

	// Move origin/main onto a commit that independently edits feature.txt —
	// the same file the MR's own diff touches — so the drift overlaps.
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	worktreeDir := t.TempDir()
	run(fixture.repoDir, "worktree", "add", "--detach", worktreeDir, fixture.base)
	if err := os.WriteFile(filepath.Join(worktreeDir, "feature.txt"), []byte("hello\nfrom target\n"), 0644); err != nil {
		t.Fatalf("write feature.txt in target worktree: %v", err)
	}
	run(worktreeDir, "add", "feature.txt")
	run(worktreeDir, "commit", "-m", "target edits feature.txt independently")
	moved := run(worktreeDir, "rev-parse", "HEAD")
	run(fixture.repoDir, "worktree", "remove", "--force", worktreeDir)
	run(fixture.repoDir, "update-ref", "refs/remotes/origin/main", moved)

	head2 := reRehearsedHead(t, fixture, "after drift refusal")

	execCalls := 0
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			execCalls++
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.8, Verdict: "approve"})
			return "", 0, nil
		},
	}
	req := fixture.request()
	req.RehearsedHead = head2

	result := Run(context.Background(), req, deps)

	if execCalls != 1 {
		t.Fatalf("gate invoked %d time(s), want 1: the stale note (reviewed before target drifted onto feature.txt, which this MR also touches) must not be reused", execCalls)
	}
	if result.Reused {
		t.Error("Reused = true, want false: a note whose ReviewedTargetTip predates a material target drift must trigger a fresh review")
	}
	if result.Exit != 0 || result.Note == nil {
		t.Fatalf("Exit/Note = %d/%v, want 0/non-nil (stderr=%q)", result.Exit, result.Note, result.Stderr)
	}
	if result.Note.ReviewedTargetTip != moved {
		t.Errorf("fresh Note.ReviewedTargetTip = %q, want the moved target tip %s", result.Note.ReviewedTargetTip, moved)
	}

	// The fresh note now clears CheckPrecondition against the moved target —
	// the recovery the drift refusal promised but the reuse path never let
	// happen.
	updated, err := store.GetIssue(context.Background(), "gt-mr-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	fields := beads.ParseMRFields(&beads.Issue{Description: updated.Description})
	if fields == nil || fields.EditorialReviewedHead == "" {
		t.Fatalf("editorial_reviewed_head not recorded, got %+v", fields)
	}
	if _, perr := CheckPrecondition(deps.Git, req.Config, []LandedMR{{
		MRID:         "gt-mr-1",
		ReviewedHead: fields.EditorialReviewedHead,
		Base:         fixture.base,
		Head:         head2,
		TargetTip:    moved,
	}}); perr != nil {
		t.Errorf("CheckPrecondition after the fresh review = %s, want the push authorized", perr.Reason)
	}
}

// TestRun_ReusesTheLatestVerdictRecordedForADiff pins which verdict governs
// when a re-roll has left more than one for the same diff, and that a plain
// invocation picks up the re-roll rather than the verdict it replaced. The
// answer has to agree with what CheckPrecondition will accept at push time, or
// a review reporting approve would be refused a push by its own re-roll.
func TestRun_ReusesTheLatestVerdictRecordedForADiff(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	superseded := recordedNoteFor(t, fixture, 0.74, "approve")
	superseded.ReviewedAt = time.Now().UTC().Add(-time.Minute)
	recordVerdict(t, fixture, superseded)

	// The re-roll's verdict, on a new head: same diff, later, rejected.
	head2 := reRehearsedHead(t, fixture, "second invocation")
	rolled := recordedNoteFor(t, fixture, 0.50, "request_changes")
	rolled.HeadSHA = head2
	rolled.ReviewedAt = time.Now().UTC()
	if err := WriteNote(git.NewGit(fixture.repoDir), rolled); err != nil {
		t.Fatalf("WriteNote: %v", err)
	}
	head3 := reRehearsedHead(t, fixture, "third invocation")
	if head3 == head2 {
		t.Fatalf("third rehearsal head is head2 (%s): the lookup would answer from the note on it, not from the notes ref", head3)
	}

	execCalls := 0
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			execCalls++
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.9, Verdict: "approve"})
			return "", 0, nil
		},
	}
	req := fixture.request()
	req.RehearsedHead = head3

	result := Run(context.Background(), req, deps)

	if execCalls != 0 {
		t.Errorf("gate invoked %d time(s), want 0: this diff already has a verdict", execCalls)
	}
	if result.Exit != 1 || !result.Reused {
		t.Fatalf("Exit/Reused = %d/%v, want 1/true (stderr=%q)", result.Exit, result.Reused, result.Stderr)
	}
	if result.Note.Score != 0.50 || result.Note.Verdict != "request_changes" {
		t.Errorf("Note = %.2f/%s, want the re-rolled 0.50/request_changes", result.Note.Score, result.Note.Verdict)
	}
}

// TestRun_RerollOnANewRehearsalHeadCarriesTheVerdictItReplaces covers the
// deliberate re-roll on the MR path: om runs, and the note it writes records
// the verdict it replaced even though that verdict was recorded on a different
// rehearsal commit. A re-roll that left no trace is what gt-bveg exists to
// stop, and on this path the verdict being replaced is never on the same
// commit (gt-qa2p).
func TestRun_RerollOnANewRehearsalHeadCarriesTheVerdictItReplaces(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	recorded := recordedNoteFor(t, fixture, 0.74, "approve")
	recordVerdict(t, fixture, recorded)
	head2 := reRehearsedHead(t, fixture, "second invocation")

	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.50, Verdict: "request_changes"})
			return "", 1, nil
		},
	}
	req := fixture.request()
	req.RehearsedHead = head2
	req.Reroll = true

	result := Run(context.Background(), req, deps)

	if result.Exit != 1 || result.Reused {
		t.Fatalf("Exit/Reused = %d/%v, want 1/false (stderr=%q)", result.Exit, result.Reused, result.Stderr)
	}
	attempts := result.Note.Attempts
	if len(attempts) != 2 {
		t.Fatalf("Attempts = %+v, want the replaced verdict and this one", attempts)
	}
	if attempts[0].Score != 0.74 || attempts[0].Verdict != "approve" {
		t.Errorf("Attempts[0] = %+v, want the replaced 0.74/approve", attempts[0])
	}
	if !attempts[0].ReviewedAt.Equal(recorded.ReviewedAt) {
		t.Errorf("Attempts[0].ReviewedAt = %v, want the replaced verdict's %v", attempts[0].ReviewedAt, recorded.ReviewedAt)
	}
	if attempts[1].Score != 0.50 || attempts[1].Verdict != "request_changes" {
		t.Errorf("Attempts[1] = %+v, want this review's 0.50/request_changes", attempts[1])
	}
	if result.Note.HeadSHA != head2 {
		t.Errorf("Note.HeadSHA = %s, want the head this review rehearsed (%s)", result.Note.HeadSHA, head2)
	}
}

// TestRun_RerollReReviewsAndRecordsAttemptHistory covers the deliberate
// re-roll: om runs, its verdict replaces the recorded one, and the note
// carries both — so the re-roll that used to be invisible is the one thing
// the proof now shows.
func TestRun_RerollReReviewsAndRecordsAttemptHistory(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	recorded := recordedNoteFor(t, fixture, 0.74, "approve")
	recordVerdict(t, fixture, recorded)

	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{
				Score: 0.50, Verdict: "request_changes",
				Findings: []Finding{{Title: "a", Severity: "minor"}, {Title: "b", Severity: "major"}},
			})
			return "", 1, nil
		},
	}
	req := fixture.request()
	req.Reroll = true

	result := Run(context.Background(), req, deps)

	if result.Exit != 1 || result.Reused {
		t.Fatalf("Exit/Reused = %d/%v, want 1/false (stderr=%q)", result.Exit, result.Reused, result.Stderr)
	}
	attempts := result.Note.Attempts
	if len(attempts) != 2 {
		t.Fatalf("Attempts = %+v, want the replaced verdict and this one", attempts)
	}
	if attempts[0].Score != 0.74 || attempts[0].Verdict != "approve" || attempts[0].Attempt != 1 {
		t.Errorf("Attempts[0] = %+v, want the replaced 0.74/approve attempt 1", attempts[0])
	}
	if !attempts[0].ReviewedAt.Equal(recorded.ReviewedAt) {
		t.Errorf("Attempts[0].ReviewedAt = %v, want the replaced verdict's %v", attempts[0].ReviewedAt, recorded.ReviewedAt)
	}
	if attempts[1].Score != 0.50 || attempts[1].Verdict != "request_changes" || attempts[1].Attempt != req.Attempt {
		t.Errorf("Attempts[1] = %+v, want this review's 0.50/request_changes attempt %d", attempts[1], req.Attempt)
	}
	if attempts[1].FindingsCount != 2 {
		t.Errorf("Attempts[1].FindingsCount = %d, want 2", attempts[1].FindingsCount)
	}

	got, err := ReadNote(deps.Git, fixture.head)
	if err != nil {
		t.Fatalf("ReadNote: %v", err)
	}
	if len(got.Attempts) != 2 || got.Score != 0.50 || got.Verdict != "request_changes" {
		t.Errorf("note on disk = %.2f/%s with %d attempts, want 0.50/request_changes with 2",
			got.Score, got.Verdict, len(got.Attempts))
	}
}

// TestRun_RecordedVerdictAppliesOnlyToTheDiffAndRubricItScored pins the three
// ways a recorded verdict stops applying. Each must re-review rather than
// answer from a verdict about something else — and must still carry the
// superseded verdict into the new note's history.
func TestRun_RecordedVerdictAppliesOnlyToTheDiffAndRubricItScored(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Note)
	}{
		{"patch-id moved", func(n *Note) { n.PatchID = "a-diff-this-head-no-longer-carries" }},
		{"rubric moved", func(n *Note) { n.RubricSHA256 = "a-rubric-this-rig-no-longer-deploys" }},
		{"recorded verdict unusable", func(n *Note) { n.Verdict = "abstain" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeBDForReview(t)
			fixture := newReviewFixture(t)
			store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
			recorded := recordedNoteFor(t, fixture, 0.74, "approve")
			tc.mutate(&recorded)
			recordVerdict(t, fixture, recorded)

			execCalls := 0
			deps := Deps{
				Git:      git.NewGit(fixture.repoDir),
				Beads:    beads.NewWithStore(fixture.repoDir, store),
				Recorder: plugin.NewRecorder(t.TempDir()),
				Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
					execCalls++
					writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.80, Verdict: "approve"})
					return "", 0, nil
				},
			}

			result := Run(context.Background(), fixture.request(), deps)

			if execCalls != 1 {
				t.Errorf("gate invoked %d time(s), want 1: the recorded verdict does not answer for this review", execCalls)
			}
			if result.Exit != 0 || result.Reused {
				t.Fatalf("Exit/Reused = %d/%v, want 0/false (stderr=%q)", result.Exit, result.Reused, result.Stderr)
			}
			if len(result.Note.Attempts) != 2 {
				t.Errorf("Attempts = %+v, want the superseded verdict carried forward", result.Note.Attempts)
			}
		})
	}
}

// TestRun_FirstAttemptRecordsNoHistory pins the default path: a note for a
// head reviewed once says so by omission, so every existing note shape
// round-trips unchanged.
func TestRun_FirstAttemptRecordsNoHistory(t *testing.T) {
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
		t.Fatalf("Exit = %d, want 0 (stderr=%q)", result.Exit, result.Stderr)
	}
	if result.Note.Attempts != nil {
		t.Errorf("Attempts = %+v, want none on a head's first review", result.Note.Attempts)
	}
	raw, err := deps.Git.NotesShow(NotesRef, fixture.head)
	if err != nil {
		t.Fatalf("NotesShow: %v", err)
	}
	if strings.Contains(raw, "attempts") {
		t.Errorf("note JSON carries attempts on a first review: %s", raw)
	}
}

// TestRun_RecordedVerdictBelowVersionFloorIsNotReused pins the fourth way a
// recorded verdict stops applying: the push precondition refuses a note whose
// om is below cfg.MinVersion, so answering from one would report approve for
// an MR that can never push.
func TestRun_RecordedVerdictBelowVersionFloorIsNotReused(t *testing.T) {
	fakeBDForReview(t)
	fixture := newReviewFixture(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	recorded := recordedNoteFor(t, fixture, 0.74, "approve")
	recorded.OMVersion = "0.9.0"
	recordVerdict(t, fixture, recorded)

	execCalls := 0
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			execCalls++
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.80, Verdict: "approve"})
			return "", 0, nil
		},
	}
	req := fixture.request()
	req.Config.MinVersion = "1.0.0"

	result := Run(context.Background(), req, deps)

	if execCalls != 1 {
		t.Errorf("gate invoked %d time(s), want 1: the recorded note is below the version floor", execCalls)
	}
	if result.Exit != 0 || result.Reused {
		t.Fatalf("Exit/Reused = %d/%v, want 0/false (stderr=%q)", result.Exit, result.Reused, result.Stderr)
	}
	if len(result.Note.Attempts) != 2 {
		t.Errorf("Attempts = %+v, want the superseded verdict carried forward", result.Note.Attempts)
	}
}

func TestClassOf(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want FailureClass
	}{
		{"plain error keeps the fallback", fmt.Errorf("boom"), VersionMismatch},
		{
			"a classified error's own class wins",
			&ClassifiedError{Class: BinaryMissing, Err: fmt.Errorf("no om")},
			BinaryMissing,
		},
		{
			"a wrapped classified error is still read",
			fmt.Errorf("assert version: %w", &ClassifiedError{Class: ConfigError, Err: fmt.Errorf("bad rubric")}),
			ConfigError,
		},
		{
			// The regression this guards: an unconditional `class = ce.Class`
			// overwrote the fallback with an empty class, which RecordFailure
			// then refuses — the version_mismatch would be recorded nowhere.
			"a classless classified error does not erase the fallback",
			&ClassifiedError{Err: fmt.Errorf("classified but unnamed")},
			VersionMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classOf(tc.err, VersionMismatch); got != tc.want {
				t.Errorf("classOf() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReceiptRefusalNotice(t *testing.T) {
	// The refusal error comes from the writer itself, so the two halves of the
	// loud path are proven to fit: a class the writer refuses is exactly what
	// the notice has to carry out to the caller.
	_, refusal := RecordFailure(plugin.NewRecorder(t.TempDir()), "gastown", "marble", "gt-wisp-x", "", "boom", 0)
	if refusal == nil {
		t.Fatal("expected RecordFailure to refuse an empty failure class")
	}

	t.Run("a recorded receipt leaves the output alone", func(t *testing.T) {
		if got := receiptRefusalNotice("gate said no", nil); got != "gate said no" {
			t.Errorf("receiptRefusalNotice() = %q, want the output unchanged", got)
		}
	})

	t.Run("a refused receipt is carried on the output", func(t *testing.T) {
		got := receiptRefusalNotice("gate said no", refusal)
		if !strings.HasPrefix(got, "gate said no") {
			t.Errorf("the captured output was dropped: %q", got)
		}
		if !strings.Contains(got, refusal.Error()) {
			t.Errorf("the refusal is not in the output, so the failure is recorded nowhere: %q", got)
		}
	})

	t.Run("a refusal with no output is still visible", func(t *testing.T) {
		got := receiptRefusalNotice("", refusal)
		if !strings.Contains(got, refusal.Error()) {
			t.Errorf("an empty capture swallowed the refusal: %q", got)
		}
	})
}
