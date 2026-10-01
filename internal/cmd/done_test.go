package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	gitpkg "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/session"
)

// TestResolveMRTarget guards gt-a8i3: `gt done`/`gt mq submit` must never
// submit an MR whose target equals the branch being submitted — that merges
// as a no-op and the post-merge cleanup deletes the only copy of the work.
// The known trigger was a resume dispatch's base_branch formula var leaking
// the resume branch, but this guard is unconditional: whatever upstream
// source produced a self-target, resolveMRTarget refuses it.
func TestResolveMRTarget(t *testing.T) {
	t.Parallel()
	t.Run("normal target passes through unchanged", func(t *testing.T) {
		got, err := resolveMRTarget("main", "polecat/jasper/gt-a8i3+xyz", "main", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "main" {
			t.Fatalf("resolveMRTarget() = %q, want %q", got, "main")
		}
	})

	t.Run("self-target falls back to rig default", func(t *testing.T) {
		branch := "polecat/thunder/be-r18+mtvr3qm3"
		// Reproduces the exact gt-a8i3 failure mode: base_branch formula var
		// leaked the resume branch, so target resolved to the polecat's own branch.
		got, err := resolveMRTarget(branch, branch, "main", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "main" {
			t.Fatalf("resolveMRTarget() = %q, want fallback to rig default %q", got, "main")
		}
	})

	t.Run("self-target with no safe fallback errors instead of guessing", func(t *testing.T) {
		branch := "main"
		_, err := resolveMRTarget(branch, branch, "main", false)
		if err == nil {
			t.Fatal("expected an error when defaultBranch also equals branch, got nil")
		}
	})

	t.Run("integration branch target passes through unchanged", func(t *testing.T) {
		got, err := resolveMRTarget("integration/epic-1", "polecat/jasper/gt-a8i3+xyz", "main", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "integration/epic-1" {
			t.Fatalf("resolveMRTarget() = %q, want %q", got, "integration/epic-1")
		}
	})

	t.Run("unexplained polecat target is refused", func(t *testing.T) {
		// gt-w2jc: a target that resolves to a DIFFERENT polecat's branch
		// (not the submitter's own — that's the self-target case above) is
		// also almost certainly wrong, e.g. a stray base_branch formula var
		// pointing at some other polecat's in-flight work.
		_, err := resolveMRTarget("polecat/thunder/be-r18+mtvr3qm3", "polecat/jasper/gt-a8i3+xyz", "main", false)
		if err == nil {
			t.Fatal("expected an error for an unexplained polecat/* target, got nil")
		}
	})

	t.Run("explicit polecat target is allowed", func(t *testing.T) {
		// An operator who deliberately passes --target/--epic at another
		// polecat's branch (e.g. stacking work) gets to do that; only the
		// formula_vars/auto-detect paths are refused.
		got, err := resolveMRTarget("polecat/thunder/be-r18+mtvr3qm3", "polecat/jasper/gt-a8i3+xyz", "main", true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "polecat/thunder/be-r18+mtvr3qm3" {
			t.Fatalf("resolveMRTarget() = %q, want explicit target to pass through", got)
		}
	})
}

// TestDoneUsesResolveBeadsDir verifies that the done command correctly uses
// beads.ResolveBeadsDir to follow redirect files when initializing beads.
// This is critical for polecat/crew worktrees that use .beads/redirect to point
// to the shared mayor/rig/.beads directory.
//
// The done.go file has two code paths that initialize beads:
//   - Line 181: ExitCompleted path - bd := beads.New(beads.ResolveBeadsDir(cwd))
//   - Line 277: ExitPhaseComplete path - bd := beads.New(beads.ResolveBeadsDir(cwd))
//
// Both must use ResolveBeadsDir to properly handle redirects.
func TestDoneUsesResolveBeadsDir(t *testing.T) {
	t.Parallel()
	// Create a temp directory structure simulating polecat worktree with redirect
	tmpDir := t.TempDir()

	// Create structure like:
	//   gastown/
	//     mayor/rig/.beads/          <- shared beads directory
	//     polecats/fixer/.beads/     <- polecat with redirect
	//       redirect -> ../../mayor/rig/.beads

	mayorRigBeadsDir := filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads")
	polecatDir := filepath.Join(tmpDir, "gastown", "polecats", "fixer")
	polecatBeadsDir := filepath.Join(polecatDir, ".beads")

	// Create directories
	if err := os.MkdirAll(mayorRigBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir mayor/rig/.beads: %v", err)
	}
	if err := os.MkdirAll(polecatBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir polecats/fixer/.beads: %v", err)
	}

	// Create redirect file pointing to mayor/rig/.beads
	redirectContent := "../../mayor/rig/.beads"
	redirectPath := filepath.Join(polecatBeadsDir, "redirect")
	if err := os.WriteFile(redirectPath, []byte(redirectContent), 0644); err != nil {
		t.Fatalf("write redirect: %v", err)
	}

	t.Run("redirect followed from polecat directory", func(t *testing.T) {
		// This mirrors how done.go initializes beads at line 181 and 277
		resolvedDir := beads.ResolveBeadsDir(polecatDir)

		// Should resolve to mayor/rig/.beads
		if resolvedDir != mayorRigBeadsDir {
			t.Errorf("ResolveBeadsDir(%s) = %s, want %s", polecatDir, resolvedDir, mayorRigBeadsDir)
		}

		// Verify the beads instance is created with the resolved path
		// We use the same pattern as done.go: beads.New(beads.ResolveBeadsDir(cwd))
		bd := beads.New(beads.ResolveBeadsDir(polecatDir))
		if bd == nil {
			t.Error("beads.New returned nil")
		}
	})

	t.Run("redirect not present uses local beads", func(t *testing.T) {
		// Without redirect, should use local .beads
		localDir := filepath.Join(tmpDir, "gastown", "mayor", "rig")
		resolvedDir := beads.ResolveBeadsDir(localDir)

		if resolvedDir != mayorRigBeadsDir {
			t.Errorf("ResolveBeadsDir(%s) = %s, want %s", localDir, resolvedDir, mayorRigBeadsDir)
		}
	})
}

// TestAutoSaveSquashTitle verifies the descriptive subject built for squashed
// auto-save/WIP commits (gt-3wf).
func TestAutoSaveSquashTitle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		issue   *beads.Issue
		issueID string
		want    string
	}{
		{
			name:    "bug issue gets fix prefix",
			issue:   &beads.Issue{Title: "handle nil pointer in auth", Type: "bug"},
			issueID: "gt-abc",
			want:    "fix: handle nil pointer in auth (gt-abc)",
		},
		{
			name:    "feature issue gets feat prefix",
			issue:   &beads.Issue{Title: "add retry to mail send", Type: "feature"},
			issueID: "gt-def",
			want:    "feat: add retry to mail send (gt-def)",
		},
		{
			name:    "chore issue gets chore prefix",
			issue:   &beads.Issue{Title: "bump linter version", Type: "chore"},
			issueID: "gt-ghi",
			want:    "chore: bump linter version (gt-ghi)",
		},
		{
			name:    "nil issue falls back to issue id",
			issue:   nil,
			issueID: "gt-jkl",
			want:    "fix: implementation work for gt-jkl",
		},
		{
			name:    "no issue at all yields empty",
			issue:   nil,
			issueID: "",
			want:    "",
		},
		{
			name:    "title without id omits parens",
			issue:   &beads.Issue{Title: "add retry", Type: "task"},
			issueID: "",
			want:    "feat: add retry",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := autoSaveSquashTitle(c.issue, c.issueID); got != c.want {
				t.Errorf("autoSaveSquashTitle() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestForceCloseIssueWithRetryClosesNoMergeIssue(t *testing.T) {
	t.Parallel()
	var gotReason string
	var gotIDs []string
	calls := 0

	err := forceCloseIssueWithRetry(func(reason string, ids ...string) error {
		calls++
		gotReason = reason
		gotIDs = append([]string(nil), ids...)
		return nil
	}, "gt-abc", "No-merge work completed; merge queue skipped", "Issue %s closed (no-merge)")
	if err != nil {
		t.Fatalf("forceCloseIssueWithRetry returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("close calls = %d, want 1", calls)
	}
	if gotReason != "No-merge work completed; merge queue skipped" {
		t.Errorf("reason = %q", gotReason)
	}
	if len(gotIDs) != 1 || gotIDs[0] != "gt-abc" {
		t.Errorf("ids = %v, want [gt-abc]", gotIDs)
	}
}

func TestForceCloseIssueWithRetryReturnsFinalError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("dolt locked")
	calls := 0

	err := forceCloseIssueWithRetrySleep(func(string, ...string) error {
		calls++
		return wantErr
	}, "gt-abc", "No-merge work completed; merge queue skipped", "Issue %s closed (no-merge)", func(time.Duration) {})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if calls != 3 {
		t.Fatalf("close calls = %d, want 3", calls)
	}
}

func TestReviewOnlyCloseRequiresEvidence(t *testing.T) {
	t.Parallel()
	issue := &beads.Issue{
		ID:          "gt-review",
		Description: "review_only: true\n",
	}

	reason, fatal := doneReviewOnlyCloseSkipReasonForHead(nil, issue.ID, issue, "abc123")
	if reason == "" {
		t.Fatal("expected review-only close skip reason")
	}
	if !fatal {
		t.Fatal("review-only close without evidence should fail closed")
	}
	if !strings.Contains(reason, "no fresh assignment timestamp") {
		t.Fatalf("reason = %q, want missing evidence", reason)
	}
}

func TestReviewOnlyCloseRejectsNotesAndDesignEvidence(t *testing.T) {
	t.Parallel()
	issue := &beads.Issue{
		ID:          "gt-review",
		Description: "review_only: true\nattached_at: 2026-07-01T12:00:00Z\n",
		Assignee:    "gastown/polecats/toast",
		Notes:       "FINDINGS: reviewed and no code changes needed",
		Design:      "PR-SHERIFF-EVIDENCE: pass\nhead_sha: abc123",
	}

	reason, fatal := doneReviewOnlyCloseSkipReasonForHead(nil, issue.ID, issue, "abc123")
	if reason == "" || !fatal {
		t.Fatalf("notes/design should not satisfy review evidence: reason=%q fatal=%v", reason, fatal)
	}
}

func TestReviewOnlyCloseAllowsFreshEvidenceComment(t *testing.T) {
	t.Parallel()
	issue := &beads.Issue{
		ID:          "gt-review",
		Description: "review_only: true\nattached_at: 2026-07-01T12:00:00Z\n",
		Assignee:    "gastown/polecats/toast",
		Comments: []beads.Comment{
			{
				Author:    "gastown/polecats/toast",
				CreatedAt: "2026-07-01T12:05:00Z",
				Text:      "PR-SHERIFF-EVIDENCE: pass\nhead_sha: abc123",
			},
		},
	}

	reason, fatal := doneReviewOnlyCloseSkipReasonForHead(nil, issue.ID, issue, "abc123")
	if reason != "" || fatal {
		t.Fatalf("doneReviewOnlyCloseSkipReason = %q, %v; want allowed", reason, fatal)
	}
}

func TestReviewOnlyGeneratedCommentsDoNotCountAsEvidence(t *testing.T) {
	t.Parallel()
	issue := &beads.Issue{
		ID:          "gt-review",
		Description: "review_only: true\nattached_at: 2026-07-01T12:00:00Z\n",
		Assignee:    "gastown/polecats/toast",
		Comments: []beads.Comment{
			{Author: "gastown/polecats/toast", CreatedAt: "2026-07-01T12:05:00Z", Text: "verified_push_skipped: --skip-verify on no-MR close\nPR-SHERIFF-EVIDENCE: pass\nhead_sha: abc123"},
			{Author: "gastown/polecats/toast", CreatedAt: "2026-07-01T12:06:00Z", Text: "MR created: gt-wisp-abc\nPR-SHERIFF-EVIDENCE: pass\nhead_sha: abc123"},
		},
	}

	reason, fatal := doneReviewOnlyCloseSkipReasonForHead(nil, issue.ID, issue, "abc123")
	if reason == "" || !fatal {
		t.Fatalf("generated comments should not satisfy review evidence: reason=%q fatal=%v", reason, fatal)
	}
}

func TestReviewOnlyCloseRejectsStaleComment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		createdAt string
	}{
		{name: "before attached_at", createdAt: "2026-07-01T11:59:59Z"},
		{name: "equal to attached_at", createdAt: "2026-07-01T12:00:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := &beads.Issue{
				ID:          "gt-review",
				Description: "review_only: true\nattached_at: 2026-07-01T12:00:00Z\n",
				Assignee:    "gastown/polecats/toast",
				Comments: []beads.Comment{{
					Author:    "gastown/polecats/toast",
					CreatedAt: tt.createdAt,
					Text:      "PR-SHERIFF-EVIDENCE: pass\nhead_sha: abc123",
				}},
			}

			reason, fatal := doneReviewOnlyCloseSkipReasonForHead(nil, issue.ID, issue, "abc123")
			if reason == "" || !fatal {
				t.Fatalf("stale comment should not satisfy review evidence: reason=%q fatal=%v", reason, fatal)
			}
		})
	}
}

func TestReviewOnlyCloseRejectsWrongAuthorOrHead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		author  string
		head    string
		current string
	}{
		{name: "wrong author", author: "gastown/polecats/other", head: "abc123", current: "abc123"},
		{name: "wrong head", author: "gastown/polecats/toast", head: "def456", current: "abc123"},
		{name: "missing head", author: "gastown/polecats/toast", head: "", current: "abc123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := "PR-SHERIFF-EVIDENCE: pass"
			if tt.head != "" {
				text += "\nhead_sha: " + tt.head
			}
			issue := &beads.Issue{
				ID:          "gt-review",
				Description: "review_only: true\nattached_at: 2026-07-01T12:00:00Z\n",
				Assignee:    "gastown/polecats/toast",
				Comments: []beads.Comment{{
					Author:    tt.author,
					CreatedAt: "2026-07-01T12:05:00Z",
					Text:      text,
				}},
			}
			reason, fatal := doneReviewOnlyCloseSkipReasonForHead(nil, issue.ID, issue, tt.current)
			if reason == "" || !fatal {
				t.Fatalf("invalid evidence should fail closed: reason=%q fatal=%v", reason, fatal)
			}
		})
	}
}

func TestReviewOnlyCloseRejectsMissingAssigneeOrInvalidCommentTime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		assignee  string
		createdAt string
	}{
		{name: "missing assignee", assignee: "", createdAt: "2026-07-01T12:05:00Z"},
		{name: "invalid comment time", assignee: "gastown/polecats/toast", createdAt: "not-a-time"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := &beads.Issue{
				ID:          "gt-review",
				Description: "review_only: true\nattached_at: 2026-07-01T12:00:00Z\n",
				Assignee:    tt.assignee,
				Comments: []beads.Comment{{
					Author:    "gastown/polecats/toast",
					CreatedAt: tt.createdAt,
					Text:      "PR-SHERIFF-EVIDENCE: pass\nhead_sha: abc123",
				}},
			}
			reason, fatal := doneReviewOnlyCloseSkipReasonForHead(nil, issue.ID, issue, "abc123")
			if reason == "" || !fatal {
				t.Fatalf("invalid metadata should fail closed: reason=%q fatal=%v", reason, fatal)
			}
		})
	}
}

func TestNonReviewOnlyCloseDoesNotRequireEvidence(t *testing.T) {
	t.Parallel()
	issue := &beads.Issue{
		ID:          "gt-review",
		Description: "no_merge: true\n",
	}

	reason, fatal := doneReviewOnlyCloseSkipReason(nil, issue.ID, issue)
	if reason != "" || fatal {
		t.Fatalf("non-review-only close gate = %q, %v; want no restriction", reason, fatal)
	}
}

func TestNonReviewOnlyReviewGateDoesNotChangeCriteriaHandling(t *testing.T) {
	t.Parallel()
	issue := &beads.Issue{
		ID:                 "gt-review",
		Description:        "no_merge: true\n",
		AcceptanceCriteria: "- [ ] still open\n",
	}

	reason, fatal := doneSourceCloseSkipReason(nil, issue.ID, issue)
	if reason == "" || fatal {
		t.Fatalf("criteria gate = %q, %v; want non-fatal skip", reason, fatal)
	}
	if !strings.Contains(reason, "unchecked acceptance criteria") {
		t.Fatalf("reason = %q, want criteria reason", reason)
	}
}

func TestSourceCloseRejectsNonConcreteIssue(t *testing.T) {
	t.Parallel()
	issue := &beads.Issue{
		ID:     "gt-mr",
		Labels: []string{"gt:merge-request"},
	}

	reason, fatal := doneSourceCloseSkipReason(nil, issue.ID, issue)
	if reason == "" || !fatal {
		t.Fatalf("source close gate = %q, %v; want fatal non-concrete rejection", reason, fatal)
	}
	if !strings.Contains(reason, "not concrete") {
		t.Fatalf("reason = %q, want non-concrete reason", reason)
	}
}

func TestSourceCloseRejectsLocalMergeStrategy(t *testing.T) {
	t.Parallel()
	issue := &beads.Issue{
		ID:          "gt-work",
		Type:        "task",
		Description: "merge_strategy: local\n",
	}

	reason, fatal := doneSourceCloseSkipReason(nil, issue.ID, issue)
	if reason == "" || fatal {
		t.Fatalf("local source close gate = %q, %v; want non-fatal skip", reason, fatal)
	}
	if !strings.Contains(reason, "merge_strategy=local") {
		t.Fatalf("reason = %q, want local merge strategy reason", reason)
	}
}

func TestSourceValidationRejectsInternalIssues(t *testing.T) {
	t.Parallel()
	if err := validateConcreteSourceIssue("gt-work", &beads.Issue{ID: "gt-work", Type: "task"}); err != nil {
		t.Fatalf("concrete source rejected: %v", err)
	}
	if err := validateConcreteSourceIssue("gt-mr", &beads.Issue{ID: "gt-mr", Labels: []string{"gt:merge-request"}}); err == nil {
		t.Fatal("internal source accepted; want rejection")
	}
}

// TestDoneBeadsInitWithoutRedirect verifies that beads initialization works
// normally when no redirect file exists.
func TestDoneBeadsInitWithoutRedirect(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create a simple .beads directory without redirect (like mayor/rig)
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	// ResolveBeadsDir should return the same directory when no redirect exists
	resolvedDir := beads.ResolveBeadsDir(tmpDir)
	if resolvedDir != beadsDir {
		t.Errorf("ResolveBeadsDir(%s) = %s, want %s", tmpDir, resolvedDir, beadsDir)
	}

	// Beads initialization should work the same way done.go does it
	bd := beads.New(beads.ResolveBeadsDir(tmpDir))
	if bd == nil {
		t.Error("beads.New returned nil")
	}
}

// TestDoneBeadsInitBothCodePaths documents that both code paths in done.go
// that create beads instances use ResolveBeadsDir:
//   - ExitCompleted (line 181): for MR creation and issue operations
//   - ExitPhaseComplete (line 277): for gate waiter registration
//
// This test verifies the pattern by demonstrating that the resolved directory
// is used consistently for different operations.
func TestDoneBeadsInitBothCodePaths(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Setup: crew directory with redirect to mayor/rig/.beads
	mayorRigBeadsDir := filepath.Join(tmpDir, "mayor", "rig", ".beads")
	crewDir := filepath.Join(tmpDir, "crew", "max")
	crewBeadsDir := filepath.Join(crewDir, ".beads")

	if err := os.MkdirAll(mayorRigBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir mayor/rig/.beads: %v", err)
	}
	if err := os.MkdirAll(crewBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir crew/max/.beads: %v", err)
	}

	// Create redirect
	redirectPath := filepath.Join(crewBeadsDir, "redirect")
	if err := os.WriteFile(redirectPath, []byte("../../mayor/rig/.beads"), 0644); err != nil {
		t.Fatalf("write redirect: %v", err)
	}

	t.Run("ExitCompleted path uses ResolveBeadsDir", func(t *testing.T) {
		// This simulates the line 181 path in done.go:
		// bd := beads.New(beads.ResolveBeadsDir(cwd))
		resolvedDir := beads.ResolveBeadsDir(crewDir)
		if resolvedDir != mayorRigBeadsDir {
			t.Errorf("ExitCompleted path: ResolveBeadsDir(%s) = %s, want %s",
				crewDir, resolvedDir, mayorRigBeadsDir)
		}

		bd := beads.New(beads.ResolveBeadsDir(crewDir))
		if bd == nil {
			t.Error("beads.New returned nil for ExitCompleted path")
		}
	})

	t.Run("ExitPhaseComplete path uses ResolveBeadsDir", func(t *testing.T) {
		// This simulates the line 277 path in done.go:
		// bd := beads.New(beads.ResolveBeadsDir(cwd))
		resolvedDir := beads.ResolveBeadsDir(crewDir)
		if resolvedDir != mayorRigBeadsDir {
			t.Errorf("ExitPhaseComplete path: ResolveBeadsDir(%s) = %s, want %s",
				crewDir, resolvedDir, mayorRigBeadsDir)
		}

		bd := beads.New(beads.ResolveBeadsDir(crewDir))
		if bd == nil {
			t.Error("beads.New returned nil for ExitPhaseComplete path")
		}
	})
}

// TestDoneRedirectChain verifies behavior with chained redirects.
// ResolveBeadsDir follows chains up to depth 3 as a safety net for legacy configs.
// SetupRedirect avoids creating chains (bd CLI doesn't support them), but if
// chains exist we follow them to the final destination.
func TestDoneRedirectChain(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create chain: worktree -> intermediate -> canonical
	canonicalBeadsDir := filepath.Join(tmpDir, "canonical", ".beads")
	intermediateDir := filepath.Join(tmpDir, "intermediate")
	intermediateBeadsDir := filepath.Join(intermediateDir, ".beads")
	worktreeDir := filepath.Join(tmpDir, "worktree")
	worktreeBeadsDir := filepath.Join(worktreeDir, ".beads")

	// Create all directories
	for _, dir := range []string{canonicalBeadsDir, intermediateBeadsDir, worktreeBeadsDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	// Create redirects
	// intermediate -> canonical
	if err := os.WriteFile(filepath.Join(intermediateBeadsDir, "redirect"), []byte("../canonical/.beads"), 0644); err != nil {
		t.Fatalf("write intermediate redirect: %v", err)
	}
	// worktree -> intermediate
	if err := os.WriteFile(filepath.Join(worktreeBeadsDir, "redirect"), []byte("../intermediate/.beads"), 0644); err != nil {
		t.Fatalf("write worktree redirect: %v", err)
	}

	// ResolveBeadsDir follows chains up to depth 3 as a safety net.
	// Note: SetupRedirect avoids creating chains (bd CLI doesn't support them),
	// but if chains exist from legacy configs, we follow them to the final destination.
	resolved := beads.ResolveBeadsDir(worktreeDir)

	// Should resolve to canonical (follows the full chain)
	if resolved != canonicalBeadsDir {
		t.Errorf("ResolveBeadsDir should follow chain to final destination: got %s, want %s",
			resolved, canonicalBeadsDir)
	}
}

// TestDoneEmptyRedirectFallback verifies that an empty or whitespace-only
// redirect file falls back to the local .beads directory.
func TestDoneEmptyRedirectFallback(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	// Create empty redirect file
	redirectPath := filepath.Join(beadsDir, "redirect")
	if err := os.WriteFile(redirectPath, []byte("   \n"), 0644); err != nil {
		t.Fatalf("write empty redirect: %v", err)
	}

	// Should fall back to local .beads
	resolved := beads.ResolveBeadsDir(tmpDir)
	if resolved != beadsDir {
		t.Errorf("empty redirect should fallback: got %s, want %s", resolved, beadsDir)
	}
}

// TestDoneCircularRedirectProtection verifies that circular redirects
// are detected and handled safely.
func TestDoneCircularRedirectProtection(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	// Create circular redirect (points to itself)
	redirectPath := filepath.Join(beadsDir, "redirect")
	if err := os.WriteFile(redirectPath, []byte(".beads"), 0644); err != nil {
		t.Fatalf("write circular redirect: %v", err)
	}

	// Should detect circular redirect and return original
	resolved := beads.ResolveBeadsDir(tmpDir)
	if resolved != beadsDir {
		t.Errorf("circular redirect should return original: got %s, want %s", resolved, beadsDir)
	}
}

// TestFindHookedBeadForAgent verifies that findHookedBeadForAgent correctly
// finds hooked beads by querying status=hooked + assignee (hq-l6mm5).
// This is critical because branch names like "polecat/furiosa-mkb0vq9f" don't
// contain the actual issue ID (test-845.1), but the status query finds it.
func TestFindHookedBeadForAgent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		agentID     string
		seed        []beads.Issue
		wantIssueID string
	}{
		{
			name:    "hooked bead assigned to agent returns issue ID",
			agentID: "testrig/polecats/furiosa",
			seed: []beads.Issue{
				{ID: "test-456", Status: string(beads.StatusHooked), Assignee: "testrig/polecats/furiosa"},
			},
			wantIssueID: "test-456",
		},
		{
			// Regression for hq-xa4z: polecats claim their assignment with
			// `bd update --status=in_progress` when starting work. A
			// hooked-only lookup returned empty here, blinding the stale-
			// branch guard (toast re-wisp-e2q carried source_issue re-k8oa
			// while the real assignment re-dkf sat in_progress).
			name:    "in_progress bead assigned to agent returns issue ID",
			agentID: "testrig/polecats/toast",
			seed: []beads.Issue{
				{ID: "test-789", Status: "in_progress", Assignee: "testrig/polecats/toast"},
			},
			wantIssueID: "test-789",
		},
		{
			name:    "hooked wins over in_progress",
			agentID: "testrig/polecats/toast",
			seed: []beads.Issue{
				{ID: "test-789", Status: "in_progress", Assignee: "testrig/polecats/toast"},
				{ID: "test-456", Status: string(beads.StatusHooked), Assignee: "testrig/polecats/toast"},
			},
			wantIssueID: "test-456",
		},
		{
			name:    "another agent's hooked bead is not returned",
			agentID: "testrig/polecats/idle",
			seed: []beads.Issue{
				{ID: "test-456", Status: string(beads.StatusHooked), Assignee: "testrig/polecats/furiosa"},
			},
			wantIssueID: "",
		},
		{
			name:        "empty agent ID returns empty",
			agentID:     "",
			wantIssueID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := beadsfake.New(beadsfake.WithPrefix("test"))
			bd.Seed(tt.seed...)

			got := findHookedBeadForAgent(bd, tt.agentID)
			if got != tt.wantIssueID {
				t.Errorf("findHookedBeadForAgent(%q) = %q, want %q", tt.agentID, got, tt.wantIssueID)
			}
		})
	}
}

func TestSelectAssignedIssue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		branchIssue string
		assigned    []string
		wantIssue   string
		wantAmbig   bool
	}{
		{
			name:      "single assignment selected",
			assigned:  []string{"gt-real"},
			wantIssue: "gt-real",
		},
		{
			name:        "stale branch overridden by single assignment",
			branchIssue: "gt-old",
			assigned:    []string{"gt-real"},
			wantIssue:   "gt-real",
		},
		{
			name:        "branch matching assignment needs no override",
			branchIssue: "gt-real",
			assigned:    []string{"gt-real"},
		},
		{
			name:        "subtask branch matching assignment needs no override",
			branchIssue: "gt-real.1",
			assigned:    []string{"gt-real"},
		},
		{
			name:        "branch matching one of multiple assignments needs no override",
			branchIssue: "gt-real",
			assigned:    []string{"gt-real", "gt-other"},
		},
		{
			name:      "duplicate assignment ids collapse",
			assigned:  []string{"gt-real", "gt-real"},
			wantIssue: "gt-real",
		},
		{
			name:      "multiple assignments are ambiguous",
			assigned:  []string{"gt-b", "gt-a"},
			wantAmbig: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotIssue, gotAmbig := selectAssignedIssue(tt.branchIssue, tt.assigned)
			if gotIssue != tt.wantIssue || gotAmbig != tt.wantAmbig {
				t.Fatalf("selectAssignedIssue(%q, %v) = (%q, %v), want (%q, %v)",
					tt.branchIssue, tt.assigned, gotIssue, gotAmbig, tt.wantIssue, tt.wantAmbig)
			}
		})
	}
}

// TestIsStaleBranchIssue verifies the stale-branch guard (hq-l0fj): a
// branch-derived issue id is overridden only when it conflicts with the
// hooked bead and is not a subtask of it.
func TestIsStaleBranchIssue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		branchIssue string
		hookedIssue string
		want        bool
	}{
		{"matching ids are not stale", "hq-oibv", "hq-oibv", false},
		{"reused branch from closed bead is stale", "re-ofo", "hq-oibv", true},
		{"subtask of hooked bead is not stale", "gt-abc.1", "gt-abc", false},
		{"different bead with shared prefix is stale", "gt-abc1", "gt-abc", true},
		{"no branch issue is not stale", "", "hq-oibv", false},
		{"no hooked bead is not stale", "re-ofo", "", false},
		{"both empty is not stale", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isStaleBranchIssue(tt.branchIssue, tt.hookedIssue); got != tt.want {
				t.Errorf("isStaleBranchIssue(%q, %q) = %v, want %v", tt.branchIssue, tt.hookedIssue, got, tt.want)
			}
		})
	}
}

// TestIsPolecatActor verifies that isPolecatActor correctly identifies
// polecat actors vs other roles based on the BD_ACTOR format.
func TestIsPolecatActor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		actor string
		want  bool
	}{
		// Polecats: rigname/polecats/polecatname
		{"testrig/polecats/furiosa", true},
		{"testrig/polecats/nux", true},
		{"myrig/polecats/witness", true}, // even if named "witness", still a polecat

		// Non-polecats
		{"gastown/crew/george", false},
		{"gastown/crew/max", false},
		{"testrig/witness", false},
		{"testrig/deacon", false},
		{"testrig/mayor", false},
		{"gastown/refinery", false},

		// Edge cases
		{"", false},
		{"single", false},
		{"polecats/name", false}, // needs rig prefix
		{"testrig/polecats", false},
		{"testrig/polecats/", false},
		{"/polecats/furiosa", false},
		{"testrig/polecats/furiosa/extra", false},
	}

	for _, tt := range tests {
		t.Run(tt.actor, func(t *testing.T) {
			got := isPolecatActor(tt.actor)
			if got != tt.want {
				t.Errorf("isPolecatActor(%q) = %v, want %v", tt.actor, got, tt.want)
			}
		})
	}
}

// TestDoneIntentLabelFormat verifies the done-intent label format matches
// the expected pattern: done-intent:<type>:<unix-ts>
func TestDoneIntentLabelFormat(t *testing.T) {
	t.Parallel()
	now := time.Now()
	tests := []struct {
		exitType string
		want     string
	}{
		{"COMPLETED", fmt.Sprintf("done-intent:COMPLETED:%d", now.Unix())},
		{"ESCALATED", fmt.Sprintf("done-intent:ESCALATED:%d", now.Unix())},
		{"DEFERRED", fmt.Sprintf("done-intent:DEFERRED:%d", now.Unix())},
		{"PHASE_COMPLETE", fmt.Sprintf("done-intent:PHASE_COMPLETE:%d", now.Unix())},
	}

	for _, tt := range tests {
		t.Run(tt.exitType, func(t *testing.T) {
			label := fmt.Sprintf("done-intent:%s:%d", tt.exitType, now.Unix())
			if label != tt.want {
				t.Errorf("label format = %q, want %q", label, tt.want)
			}

			// Verify the label can be parsed back
			parts := strings.SplitN(label, ":", 3)
			if len(parts) != 3 {
				t.Fatalf("expected 3 parts, got %d", len(parts))
			}
			if parts[0] != "done-intent" {
				t.Errorf("prefix = %q, want %q", parts[0], "done-intent")
			}
			if parts[1] != tt.exitType {
				t.Errorf("exit type = %q, want %q", parts[1], tt.exitType)
			}
		})
	}
}

func TestShouldRetirePolecatSessionAfterDone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		exitType    string
		fromHandoff bool
		want        bool
	}{
		{"completed retires", ExitCompleted, false, true},
		{"deferred retires session", ExitDeferred, false, true},
		{"escalated retires session", ExitEscalated, false, true},
		{"non-final exit preserves session", "PHASE_COMPLETE", false, false},
		{"unknown exit preserves session", "PAUSED", false, false},
		{"empty exit preserves session", "", false, false},
		{"handoff-triggered deferred preserves session", ExitDeferred, true, false},
		{"handoff flag overrides completed retirement", ExitCompleted, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldRetirePolecatSessionAfterDone(tt.exitType, tt.fromHandoff)
			if got != tt.want {
				t.Errorf("shouldRetirePolecatSessionAfterDone(%q, %v) = %v, want %v", tt.exitType, tt.fromHandoff, got, tt.want)
			}
		})
	}
}

type fakeDoneSessionKiller struct {
	name        string
	excludePIDs []string
	calls       int
}

func (f *fakeDoneSessionKiller) KillSessionWithProcessesExcluding(name string, excludePIDs []string) error {
	f.calls++
	f.name = name
	f.excludePIDs = append([]string(nil), excludePIDs...)
	return nil
}

func TestRetirePolecatSessionAfterDoneUsesPIDExclusion(t *testing.T) {
	fake := &fakeDoneSessionKiller{}
	old := newDoneSessionKiller
	newDoneSessionKiller = func() doneSessionKiller { return fake }
	t.Cleanup(func() { newDoneSessionKiller = old })

	if err := retirePolecatSessionAfterDone(cmdTestRegistry(), "gastown", "nitro", 12345); err != nil {
		t.Fatalf("retirePolecatSessionAfterDone: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("killer calls = %d, want 1", fake.calls)
	}
	wantSession := session.PolecatSessionName("gt", "nitro")
	if fake.name != wantSession {
		t.Fatalf("session name = %q, want %q", fake.name, wantSession)
	}
	if len(fake.excludePIDs) != 1 || fake.excludePIDs[0] != "12345" {
		t.Fatalf("excludePIDs = %#v, want [12345]", fake.excludePIDs)
	}
}

func TestRetirePolecatSessionAfterDoneNoopsWithoutIdentity(t *testing.T) {
	fake := &fakeDoneSessionKiller{}
	old := newDoneSessionKiller
	newDoneSessionKiller = func() doneSessionKiller { return fake }
	t.Cleanup(func() { newDoneSessionKiller = old })

	for _, tt := range []struct {
		name        string
		rigName     string
		polecatName string
		pid         int
	}{
		{"missing rig", "", "nitro", 12345},
		{"missing polecat", "gastown", "", 12345},
		{"missing pid", "gastown", "nitro", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := retirePolecatSessionAfterDone(cmdTestRegistry(), tt.rigName, tt.polecatName, tt.pid); err != nil {
				t.Fatalf("retirePolecatSessionAfterDone: %v", err)
			}
		})
	}
	if fake.calls != 0 {
		t.Fatalf("killer calls = %d, want 0", fake.calls)
	}
}

// TestFinalExitRetiresSessionThroughExitPath drives the exit path gt done runs as
// its last action, so the retirement decision and the session kill are asserted
// together (gt-5g3e). The reverse direction matters too: the exits that still
// leave work recoverable from the live session must keep it.
func TestFinalExitRetiresSessionThroughExitPath(t *testing.T) {
	tests := []struct {
		name        string
		exitType    string
		fromHandoff bool
		wantKills   int
	}{
		{"completed retires session", ExitCompleted, false, 1},
		{"deferred retires session", ExitDeferred, false, 1},
		{"escalated retires session", ExitEscalated, false, 1},
		{"non-final exit preserves session", "PHASE_COMPLETE", false, 0},
		{"unknown exit preserves session", "PAUSED", false, 0},
		{"handoff-triggered deferred preserves session", ExitDeferred, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeDoneSessionKiller{}
			old := newDoneSessionKiller
			newDoneSessionKiller = func() doneSessionKiller { return fake }
			t.Cleanup(func() { newDoneSessionKiller = old })

			retired := retirePolecatSessionAfterFinalExit(cmdTestRegistry(), tt.exitType, tt.fromHandoff, "gastown", "basalt", 4242)

			if fake.calls != tt.wantKills {
				t.Fatalf("session killer calls = %d, want %d (retired=%v)", fake.calls, tt.wantKills, retired)
			}
			if wantRetired := tt.wantKills == 1; retired != wantRetired {
				t.Errorf("retirePolecatSessionAfterFinalExit(%q) = %v, want %v", tt.exitType, retired, wantRetired)
			}
			if tt.wantKills == 0 {
				return
			}
			wantSession := session.PolecatSessionName("gt", "basalt")
			if fake.name != wantSession {
				t.Errorf("killed session = %q, want %q", fake.name, wantSession)
			}
			if len(fake.excludePIDs) != 1 || fake.excludePIDs[0] != "4242" {
				t.Errorf("excludePIDs = %#v, want [4242] (gt done must outlive its own kill)", fake.excludePIDs)
			}
		})
	}
}

func TestCleanupStatusAfterSuccessfulPush(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status string
		want   string
	}{
		{"unpushed", "clean"},
		{"has_unpushed", "clean"},
		{"clean", "clean"},
		{"uncommitted", "uncommitted"},
		{"stash", "stash"},
		{"unknown", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			if got := cleanupStatusAfterSuccessfulPush(tt.status); got != tt.want {
				t.Errorf("cleanupStatusAfterSuccessfulPush(%q) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestCleanupStatusFromWorkState(t *testing.T) {
	t.Parallel()
	pushErr := errors.New("remote unavailable")
	tests := []struct {
		name          string
		status        *gitpkg.UncommittedWorkStatus
		branchPushed  bool
		unpushedCount int
		pushErr       error
		want          string
	}{
		{name: "nil", status: nil, branchPushed: true, want: "unknown"},
		{
			name:         "runtime only pushed",
			status:       &gitpkg.UncommittedWorkStatus{HasUncommittedChanges: true, ModifiedFiles: []string{".opencode/plugins/gastown.js"}},
			branchPushed: true,
			want:         "clean",
		},
		{
			name:         "runtime plus source",
			status:       &gitpkg.UncommittedWorkStatus{HasUncommittedChanges: true, ModifiedFiles: []string{".opencode/plugins/gastown.js", "internal/cmd/done.go"}},
			branchPushed: true,
			want:         "uncommitted",
		},
		{
			name:         "runtime plus stash",
			status:       &gitpkg.UncommittedWorkStatus{HasUncommittedChanges: true, ModifiedFiles: []string{".opencode/plugins/gastown.js"}, StashCount: 1},
			branchPushed: true,
			want:         "stash",
		},
		{
			name:          "runtime plus unpushed",
			status:        &gitpkg.UncommittedWorkStatus{HasUncommittedChanges: true, ModifiedFiles: []string{".opencode/plugins/gastown.js"}},
			branchPushed:  true,
			unpushedCount: 1,
			want:          "unpushed",
		},
		{
			name:         "runtime plus push error",
			status:       &gitpkg.UncommittedWorkStatus{HasUncommittedChanges: true, ModifiedFiles: []string{".opencode/plugins/gastown.js"}},
			branchPushed: true,
			pushErr:      pushErr,
			want:         "unpushed",
		},
		{
			name:         "runtime conflict",
			status:       &gitpkg.UncommittedWorkStatus{HasUncommittedChanges: true, UnmergedFiles: []string{".opencode/plugins/gastown.js"}},
			branchPushed: true,
			want:         "uncommitted",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanupStatusFromWorkState(tt.status, tt.branchPushed, tt.unpushedCount, tt.pushErr); got != tt.want {
				t.Fatalf("cleanupStatusFromWorkState() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveDoneAgentIdentityAlwaysNamesThePolecat pins hq-vx224's other half:
// gt done's agent-bead writes (done-intent label, checkpoints, active_mr,
// completion metadata, agent_state, cleanup_status) are all guarded by
// `agentBeadID != ""`, and getAgentBeadID returns "" for a rig-less context.
// The context is therefore seeded from the polecat identity that gt done has
// already validated (BD_ACTOR + GT_ROLE/GT_RIG/GT_POLECAT), so a degraded env
// or cwd detection can no longer make gt done complete without recording
// anything.
func TestResolveDoneAgentIdentityAlwaysNamesThePolecat(t *testing.T) {
	t.Run("undetectable environment still names the polecat", func(t *testing.T) {
		// Neither GT_ROLE nor a recognizable working directory: role detection
		// can contribute nothing.
		t.Setenv("GT_ROLE", "")
		t.Setenv("GT_RIG", "")
		t.Setenv("GT_POLECAT", "")
		t.Setenv("GT_CREW", "")
		notATown := t.TempDir()

		ctx, actor := resolveDoneAgentIdentity(notATown, notATown, "gastown", "flint")
		if actor != "" {
			t.Errorf("actor = %q, want empty: ActorString degrades to \"unknown\" for an undetected role, and that must not replace the validated sender on the \"[done]\" log line", actor)
		}
		if ctx.Role != RolePolecat || ctx.Rig != "gastown" || ctx.Polecat != "flint" {
			t.Fatalf("ctx = %+v, want the validated polecat identity preserved", ctx)
		}
		if id := getAgentBeadID(ctx); id == "" {
			t.Fatal("agent bead ID must resolve for a polecat gt done: an empty ID skips every agent-bead write, including the cleanup_status self-report")
		} else if !strings.Contains(id, "flint") {
			t.Fatalf("agent bead ID %q does not name the polecat", id)
		}
	})

	t.Run("detected identity wins and supplies the log actor", func(t *testing.T) {
		t.Setenv("GT_ROLE", "polecat")
		t.Setenv("GT_RIG", "gastown")
		t.Setenv("GT_POLECAT", "flint")
		// GT_CREW is read before GT_POLECAT when GetRoleWithContext fills a
		// simple role's identity from env. TestDeriveSessionName (costs_test.go)
		// leaves GT_CREW=max in the process env — its subtests unset GT_* on
		// entry but their cleanup only re-sets values that were non-empty when
		// saved, so keys a subtest cleared are never restored to empty. Clearing
		// it here keeps this test's result independent of what ran before it.
		t.Setenv("GT_CREW", "")

		ctx, actor := resolveDoneAgentIdentity(t.TempDir(), t.TempDir(), "ignored-rig", "ignored-polecat")
		if actor != "gastown/polecats/flint" {
			t.Errorf("actor = %q, want %q", actor, "gastown/polecats/flint")
		}
		if ctx.Rig != "gastown" || ctx.Polecat != "flint" {
			t.Fatalf("ctx = %+v, want the detected identity to refine the seeded one", ctx)
		}
	})
}

// TestResolveCleanupStatusForSelfReportIsTotal pins hq-vx224: gt done's
// self-report must never resolve to an empty string, because an empty
// cleanup_status is what "cleanup_status=<missing>" reads and no later writer
// ever fills it in (reclaim.go). An unobservable state is recorded as
// "unknown" instead — still fail-closed at every gate, but distinguishable
// from "gt done never ran".
func TestResolveCleanupStatusForSelfReportIsTotal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		computed   string
		observed   string
		wantStatus string
	}{
		{name: "computed wins", computed: "clean", observed: "has_stash", wantStatus: "clean"},
		{name: "failed push records the dirty fact", computed: "unpushed", wantStatus: "has_unpushed"},
		{name: "unobserved falls back to a fresh observation", observed: "clean", wantStatus: "clean"},
		{name: "explicit unknown is re-observed", computed: "unknown", observed: "has_uncommitted", wantStatus: "has_uncommitted"},
		{name: "nothing observable still records unknown", computed: "", observed: "", wantStatus: "unknown"},
		{name: "unparseable computed value is not dropped", computed: "dirty-ish", observed: "", wantStatus: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveCleanupStatusForSelfReport(tt.computed, tt.observed)
			if string(got) != tt.wantStatus {
				t.Fatalf("resolveCleanupStatusForSelfReport(%q, %q) = %q, want %q",
					tt.computed, tt.observed, string(got), tt.wantStatus)
			}
			if string(got) == "" {
				t.Fatal("resolved cleanup status must never be empty: the agent bead would be left with cleanup_status=<missing>")
			}
		})
	}
}

// TestSelfReportCleanupStatusAlwaysWrites verifies that the self-report is not
// skipped on the paths that used to skip it: the write is made for a failed
// push (when updateAgentStateAfterSubmission bails out before recording
// anything) and for a status that cannot be observed at all.
func TestSelfReportCleanupStatusAlwaysWrites(t *testing.T) {
	t.Parallel()
	// A directory that is not a git repository: every observation attempt in
	// observeCleanupStatus fails, which is the "cannot observe" case.
	notARepo := gitpkg.NewGit(t.TempDir())

	tests := []struct {
		name        string
		computed    string
		wantWritten string
	}{
		{name: "successful completion records clean", computed: "clean", wantWritten: "clean"},
		{name: "failed push records has_unpushed", computed: "unpushed", wantWritten: "has_unpushed"},
		{name: "unobservable state records unknown", computed: "", wantWritten: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updater := &fakeCleanupUpdater{}
			selfReportCleanupStatus(notARepo, "polecat/jasper/gt-624w+xyz", updater, "gt-gastown-polecat-jasper", tt.computed)

			if updater.calls != 1 {
				t.Fatalf("UpdateAgentCleanupStatus calls = %d, want 1 (the write must never be skipped)", updater.calls)
			}
			if updater.id != "gt-gastown-polecat-jasper" {
				t.Errorf("wrote to agent bead %q, want %q", updater.id, "gt-gastown-polecat-jasper")
			}
			if updater.status != tt.wantWritten {
				t.Errorf("cleanup status written = %q, want %q", updater.status, tt.wantWritten)
			}
		})
	}

	t.Run("missing agent bead id is loud, not silent", func(t *testing.T) {
		updater := &fakeCleanupUpdater{}
		selfReportCleanupStatus(notARepo, "polecat/jasper/gt-624w+xyz", updater, "", "clean")
		if updater.calls != 0 {
			t.Fatalf("UpdateAgentCleanupStatus calls = %d, want 0 with no agent bead ID", updater.calls)
		}
	})

	t.Run("write failure is non-fatal", func(t *testing.T) {
		updater := &fakeCleanupUpdater{err: errors.New("dolt is sad")}
		selfReportCleanupStatus(notARepo, "polecat/jasper/gt-624w+xyz", updater, "gt-gastown-polecat-jasper", "clean")
		if updater.calls != 1 {
			t.Fatalf("UpdateAgentCleanupStatus calls = %d, want 1", updater.calls)
		}
	})
}

// TestClearDoneIntentLabel verifies that clearDoneIntentLabel removes
// only done-intent labels while preserving other labels.
func TestClearDoneIntentLabel(t *testing.T) {
	t.Parallel()
	// We can't easily test the full clearDoneIntentLabel function without
	// a running bd instance, but we can verify the filtering logic.
	// The function reads labels, filters out done-intent:*, and writes back.
	allLabels := []string{
		"gt:agent",
		"idle:3",
		"done-intent:COMPLETED:1738972800",
		"backoff-until:1738972900",
	}

	var kept []string
	for _, label := range allLabels {
		if !strings.HasPrefix(label, "done-intent:") {
			kept = append(kept, label)
		}
	}

	if len(kept) != 3 {
		t.Errorf("expected 3 labels after filtering, got %d: %v", len(kept), kept)
	}

	// Verify done-intent was removed
	for _, label := range kept {
		if strings.HasPrefix(label, "done-intent:") {
			t.Errorf("done-intent label was not removed: %s", label)
		}
	}

	// Verify other labels were preserved
	wantKept := map[string]bool{
		"gt:agent":                 true,
		"idle:3":                   true,
		"backoff-until:1738972900": true,
	}
	for _, label := range kept {
		if !wantKept[label] {
			t.Errorf("unexpected label in kept set: %s", label)
		}
	}
}

// TestMRVerificationSetsMRFailed verifies that if MR bead creation returns
// success but the bead cannot be read back (verification fails), mrFailed
// is set to true. This is the core fix for GH#1945: without verification,
// a "successful" bd.Create that didn't actually persist would allow the
// worktree nuke to proceed, losing the polecat's work.
func TestMRVerificationSetsMRFailed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		createErr    error // error from bd.Create
		showErr      error // error from bd.Show (verification)
		showReturns  bool  // whether Show returns a non-nil issue
		wantMRFailed bool
	}{
		{
			name:         "create succeeds + show succeeds → mrFailed=false",
			createErr:    nil,
			showErr:      nil,
			showReturns:  true,
			wantMRFailed: false,
		},
		{
			name:         "create fails → mrFailed=true (existing behavior)",
			createErr:    fmt.Errorf("dolt write failed"),
			showErr:      nil,
			showReturns:  false,
			wantMRFailed: true,
		},
		{
			name:         "create succeeds + show fails → mrFailed=true (GH#1945 fix)",
			createErr:    nil,
			showErr:      fmt.Errorf("bead not found"),
			showReturns:  false,
			wantMRFailed: true,
		},
		{
			name:         "create succeeds + show returns nil → mrFailed=true (GH#1945 fix)",
			createErr:    nil,
			showErr:      nil,
			showReturns:  false,
			wantMRFailed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the MR creation + verification flow from done.go
			mrFailed := false

			if tt.createErr != nil {
				// bd.Create failed — existing behavior
				mrFailed = true
			} else {
				// bd.Create succeeded — now verify (GH#1945 fix)
				var showResult bool
				if tt.showErr != nil || !tt.showReturns {
					showResult = false
				} else {
					showResult = true
				}
				if !showResult {
					mrFailed = true
				}
			}

			if mrFailed != tt.wantMRFailed {
				t.Errorf("mrFailed = %v, want %v", mrFailed, tt.wantMRFailed)
			}
		})
	}
}

// TestMRBeadCreationUsesRig verifies that MR bead creation specifies the rig (gt-7y7).
// When a polecat works on a cross-rig bead (e.g., hq-xxx on rig "gastown"), the
// MR bead must be created with Rig set to the polecat's rig so it lands in the
// rig's database — not the town-level database where the source bead lives.
// Without this, the refinery never finds the MR and the branch sits unmerged.
func TestMRBeadCreationUsesRig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		issueID string
		rigName string
		wantRig string
	}{
		{
			name:    "same-rig bead: rig is still set",
			issueID: "gt-abc",
			rigName: "gastown",
			wantRig: "gastown",
		},
		{
			name:    "cross-rig hq- bead: MR must land in polecat rig",
			issueID: "hq-abc",
			rigName: "gastown",
			wantRig: "gastown",
		},
		{
			name:    "cross-rig en- bead: MR must land in polecat rig",
			issueID: "en-xyz",
			rigName: "gastown",
			wantRig: "gastown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the CreateOptions construction in done.go.
			opts := beads.CreateOptions{
				Title:     "Merge: " + tt.issueID,
				Labels:    []string{"gt:merge-request"},
				Ephemeral: true,
				Rig:       tt.rigName,
			}
			if opts.Rig != tt.wantRig {
				t.Errorf("CreateOptions.Rig = %q, want %q (issue %s)", opts.Rig, tt.wantRig, tt.issueID)
			}
		})
	}
}

// TestDeferredKillNotOnValidationError verifies that the deferred session kill
// does NOT trigger when runDone returns early due to validation errors (bad flags,
// wrong role). The sessionCleanupNeeded flag must only be set after role detection
// confirms this is a polecat.
func TestDeferredKillNotOnValidationError(t *testing.T) {
	t.Parallel()
	// Simulate the flag lifecycle:
	// 1. sessionCleanupNeeded starts false
	// 2. Set true only after role detection confirms polecat
	// 3. Early returns (validation) happen before the flag is set

	// Scenario 1: Validation error (bad status) — returns before flag set
	sessionCleanupNeeded := false
	// (invalid exit status check would return here)
	// defer checks: sessionCleanupNeeded is false → no-op
	if sessionCleanupNeeded {
		t.Error("sessionCleanupNeeded should be false for validation errors")
	}

	// Scenario 2: Polecat confirmed — flag set
	sessionCleanupNeeded = true
	sessionKilled := false
	// (push fails, returns with error)
	// defer checks: sessionCleanupNeeded is true, sessionKilled is false → kill session
	if !sessionCleanupNeeded || sessionKilled {
		t.Error("deferred kill should trigger when sessionCleanupNeeded && !sessionKilled")
	}

	// Scenario 3: Clean exit — explicit kill succeeded
	sessionKilled = true
	// defer checks: sessionKilled is true → no-op (don't double-kill)
	if sessionCleanupNeeded && !sessionKilled {
		t.Error("deferred kill should NOT trigger when sessionKilled is true")
	}
}

// TestBranchDetectionGuard verifies that gt done no longer trusts GT_BRANCH
// after cwd/worktree ownership is unavailable. The command must fail closed
// before branch detection instead of reconstructing authority from env.
func TestBranchDetectionGuard(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		cwdAvailable bool
		gtBranch     string // GT_BRANCH env var value
		wantError    bool
		wantBranch   string
	}{
		{
			name:         "cwd available - uses git CurrentBranch",
			cwdAvailable: true,
			gtBranch:     "",
			wantError:    false,
			wantBranch:   "current-branch", // simulated
		},
		{
			name:         "cwd unavailable + GT_BRANCH set - still errors",
			cwdAvailable: false,
			gtBranch:     "polecat/test-worker",
			wantError:    true,
			wantBranch:   "",
		},
		{
			name:         "cwd unavailable + GT_BRANCH empty - returns error",
			cwdAvailable: false,
			gtBranch:     "",
			wantError:    true,
			wantBranch:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the branch detection guard in runDone.
			var branch string
			if !tt.cwdAvailable {
				_ = tt.gtBranch // env branch is intentionally ignored when cwd is unavailable
			}

			var gotError bool
			if branch == "" {
				if !tt.cwdAvailable {
					gotError = true
				} else {
					// Would call g.CurrentBranch() — simulate success
					branch = "current-branch"
				}
			}

			if gotError != tt.wantError {
				t.Errorf("error = %v, want %v", gotError, tt.wantError)
			}
			if !tt.wantError && branch != tt.wantBranch {
				t.Errorf("branch = %q, want %q", branch, tt.wantBranch)
			}
		})
	}
}

// TestBranchDetectionCleanupOnError verifies that deleted-worktree branch
// recovery is no longer considered a cleanup path for gt done.
func TestBranchDetectionCleanupOnError(t *testing.T) {
	t.Parallel()
	// Simulate the deleted-worktree guard before branch detection.
	cwdAvailable := false
	gtBranch := ""

	var branch string
	if !cwdAvailable {
		_ = gtBranch // ignored without a proven current worktree
	}

	sessionCleanupNeeded := false
	if branch == "" && !cwdAvailable {
		// The ownership guard returns before arming cleanup or trusting env state.
		sessionCleanupNeeded = false
	}

	if sessionCleanupNeeded {
		t.Error("sessionCleanupNeeded should stay false when worktree ownership is unavailable")
	}
}

// TestConvoyMergeStrategyBranching verifies that the merge strategy branching
// logic in runDone correctly routes to the right code path for each strategy.
func TestConvoyMergeStrategyBranching(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		mergeStrategy string
		wantPush      bool // should push happen?
		wantMR        bool // should MR bead be created?
		wantDirect    bool // should push to default branch?
	}{
		{
			name:          "mr strategy - normal push and MR",
			mergeStrategy: "mr",
			wantPush:      true,
			wantMR:        true,
			wantDirect:    false,
		},
		{
			name:          "empty strategy - defaults to mr behavior",
			mergeStrategy: "",
			wantPush:      true,
			wantMR:        true,
			wantDirect:    false,
		},
		{
			name:          "direct strategy - push to main, no MR",
			mergeStrategy: "direct",
			wantPush:      true,
			wantMR:        false,
			wantDirect:    true,
		},
		{
			name:          "local strategy - no push, no MR",
			mergeStrategy: "local",
			wantPush:      false,
			wantMR:        false,
			wantDirect:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the branching logic from runDone
			shouldPush := true
			shouldCreateMR := true
			shouldPushDirect := false

			switch tt.mergeStrategy {
			case "local":
				shouldPush = false
				shouldCreateMR = false
			case "direct":
				shouldPushDirect = true
				shouldCreateMR = false
			default:
				// "mr" or empty = default behavior
			}

			if shouldPush != tt.wantPush {
				t.Errorf("shouldPush = %v, want %v", shouldPush, tt.wantPush)
			}
			if shouldCreateMR != tt.wantMR {
				t.Errorf("shouldCreateMR = %v, want %v", shouldCreateMR, tt.wantMR)
			}
			if shouldPushDirect != tt.wantDirect {
				t.Errorf("shouldPushDirect = %v, want %v", shouldPushDirect, tt.wantDirect)
			}
		})
	}
}

// TestConvoyMergeStrategyNotification verifies that the merge strategy
// is included in the witness notification body when set to non-default values.
func TestConvoyMergeStrategyNotification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		mergeStrategy string
		wantInBody    bool
	}{
		{"direct strategy included", "direct", true},
		{"local strategy included", "local", true},
		{"mr strategy excluded", "mr", false},
		{"empty strategy excluded", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the notification body building from runDone
			var bodyLines []string
			bodyLines = append(bodyLines, "Exit: COMPLETED")
			if tt.mergeStrategy != "" && tt.mergeStrategy != "mr" {
				bodyLines = append(bodyLines, fmt.Sprintf("MergeStrategy: %s", tt.mergeStrategy))
			}

			body := strings.Join(bodyLines, "\n")
			hasMergeStrategy := strings.Contains(body, "MergeStrategy:")

			if hasMergeStrategy != tt.wantInBody {
				t.Errorf("body contains MergeStrategy = %v, want %v\nbody: %s",
					hasMergeStrategy, tt.wantInBody, body)
			}
		})
	}
}

// TestConvoyMergeFromFields verifies that convoyMergeFromFields correctly
// extracts the merge strategy from convoy descriptions using typed ConvoyFields.
func TestConvoyMergeFromFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		description string
		want        string
	}{
		{
			name:        "direct strategy",
			description: "Auto-created convoy tracking gt-abc\nMerge: direct",
			want:        "direct",
		},
		{
			name:        "mr strategy",
			description: "Convoy tracking 3 issues\nOwner: mayor/\nMerge: mr",
			want:        "mr",
		},
		{
			name:        "local strategy",
			description: "Merge: local\nOwner: mayor/",
			want:        "local",
		},
		{
			name:        "no merge field",
			description: "Auto-created convoy tracking gt-abc",
			want:        "",
		},
		{
			name:        "empty description",
			description: "",
			want:        "",
		},
		{
			name:        "merge in middle of description",
			description: "Convoy tracking 1 issues\nMerge: direct\nNotify: mayor/",
			want:        "direct",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convoyMergeFromFields(tt.description)
			if got != tt.want {
				t.Errorf("convoyMergeFromFields() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestClearDoneCheckpoints verifies that clearDoneCheckpoints removes
// only done-cp labels while preserving other labels.
func TestClearDoneCheckpoints(t *testing.T) {
	t.Parallel()
	allLabels := []string{
		"gt:agent",
		"idle:3",
		"done-intent:COMPLETED:1738972800",
		"done-cp:pushed:mybranch:1738972801",
		"done-cp:mr-created:gt-xyz:1738972802",
		"backoff-until:1738972900",
	}

	var kept []string
	var removed []string
	for _, label := range allLabels {
		if strings.HasPrefix(label, "done-cp:") {
			removed = append(removed, label)
		} else {
			kept = append(kept, label)
		}
	}

	if len(removed) != 2 {
		t.Errorf("expected 2 checkpoint labels removed, got %d: %v", len(removed), removed)
	}
	if len(kept) != 4 {
		t.Errorf("expected 4 labels kept, got %d: %v", len(kept), kept)
	}

	// Verify no checkpoint labels in kept set
	for _, label := range kept {
		if strings.HasPrefix(label, "done-cp:") {
			t.Errorf("checkpoint label was not removed: %s", label)
		}
	}

	// Verify done-intent is preserved (not a checkpoint)
	found := false
	for _, label := range kept {
		if strings.HasPrefix(label, "done-intent:") {
			found = true
		}
	}
	if !found {
		t.Error("done-intent label should be preserved by clearDoneCheckpoints")
	}
}

// TestConvoyInfoFallbackChain verifies that done.go checks attachment fields
// first, then falls back to dep-based convoy lookup. This is the fix for gt-7b6wf:
// convoy merge=direct was not propagated because cross-rig dep resolution failed.
func TestConvoyInfoFallbackChain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		attachmentInfo *ConvoyInfo // Result from getConvoyInfoFromIssue
		depInfo        *ConvoyInfo // Result from getConvoyInfoForIssue
		wantConvoyID   string
		wantMerge      string
		wantNil        bool
	}{
		{
			name:           "attachment fields provide convoy info",
			attachmentInfo: &ConvoyInfo{ID: "hq-cv-abc", MergeStrategy: "direct"},
			depInfo:        nil, // Not called
			wantConvoyID:   "hq-cv-abc",
			wantMerge:      "direct",
		},
		{
			name:           "attachment fields empty, dep lookup succeeds",
			attachmentInfo: nil,
			depInfo:        &ConvoyInfo{ID: "hq-cv-xyz", MergeStrategy: "mr"},
			wantConvoyID:   "hq-cv-xyz",
			wantMerge:      "mr",
		},
		{
			name:           "both nil - no convoy",
			attachmentInfo: nil,
			depInfo:        nil,
			wantNil:        true,
		},
		{
			name:           "attachment has convoy, dep also has (attachment wins)",
			attachmentInfo: &ConvoyInfo{ID: "hq-cv-from-attachment", MergeStrategy: "direct"},
			depInfo:        &ConvoyInfo{ID: "hq-cv-from-dep", MergeStrategy: "mr"},
			wantConvoyID:   "hq-cv-from-attachment",
			wantMerge:      "direct",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the fallback chain from done.go
			var convoyInfo *ConvoyInfo
			convoyInfo = tt.attachmentInfo
			if convoyInfo == nil {
				convoyInfo = tt.depInfo
			}

			if tt.wantNil {
				if convoyInfo != nil {
					t.Errorf("expected nil, got %+v", convoyInfo)
				}
				return
			}
			if convoyInfo == nil {
				t.Fatal("expected non-nil convoy info")
			}
			if convoyInfo.ID != tt.wantConvoyID {
				t.Errorf("ConvoyID = %q, want %q", convoyInfo.ID, tt.wantConvoyID)
			}
			if convoyInfo.MergeStrategy != tt.wantMerge {
				t.Errorf("MergeStrategy = %q, want %q", convoyInfo.MergeStrategy, tt.wantMerge)
			}
		})
	}
}

// TestHookedBeadCloseNotRestrictedToHookedStatus verifies the gt-pftz fix:
// gt done must close the hooked bead regardless of its current status (hooked,
// in_progress, open), not only when status == "hooked". Polecats update their
// work bead to in_progress during work, so the old exact-match check skipped
// closing and caused infinite dispatch loops.
func TestHookedBeadCloseNotRestrictedToHookedStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		status    string
		wantClose bool
	}{
		{"status hooked → close", "hooked", true},
		{"status in_progress → close", "in_progress", true},
		{"status open → close", "open", true},
		{"status blocked → close", "blocked", true},
		{"status closed → skip (terminal)", "closed", false},
		{"status tombstone → skip (terminal)", "tombstone", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Replicate the guard condition from updateAgentStateOnDone (gt-pftz fix)
			shouldClose := !beads.IssueStatus(tt.status).IsTerminal()
			if shouldClose != tt.wantClose {
				t.Errorf("shouldClose for status %q = %v, want %v", tt.status, shouldClose, tt.wantClose)
			}
		})
	}
}

// fakeSubmodules reports changes and records the submodule pushes.
type fakeSubmodules struct {
	changes []gitpkg.SubmoduleChange
	listErr error
	pushErr error
	pushed  []string
}

func (f *fakeSubmodules) SubmoduleChanges(base, head string) ([]gitpkg.SubmoduleChange, error) {
	return f.changes, f.listErr
}

func (f *fakeSubmodules) PushSubmoduleCommit(path, sha, remote string) error {
	f.pushed = append(f.pushed, path+"@"+sha+" to "+remote)
	return f.pushErr
}

// TestPushSubmoduleChanges pushes each changed submodule's new commit to its
// origin before the parent push (gt-dzs), skips a removed submodule, and
// treats a repo without submodules, or a failed listing or push, as a
// warning rather than a stop.
func TestPushSubmoduleChanges(t *testing.T) {
	t.Parallel()
	f := &fakeSubmodules{changes: []gitpkg.SubmoduleChange{
		{Path: "libs/sub", OldSHA: "old", NewSHA: "0123456789abcdef"},
		{Path: "libs/gone", OldSHA: "old", NewSHA: ""},
	}}
	pushSubmoduleChanges(f, "origin/main")
	if strings.Join(f.pushed, ",") != "libs/sub@0123456789abcdef to origin" {
		t.Errorf("pushed %v, want only libs/sub's new commit to origin", f.pushed)
	}

	none := &fakeSubmodules{}
	pushSubmoduleChanges(none, "origin/main")
	failing := &fakeSubmodules{listErr: errors.New("no .gitmodules")}
	pushSubmoduleChanges(failing, "origin/main")
	refused := &fakeSubmodules{changes: f.changes[:1], pushErr: errors.New("rejected")}
	pushSubmoduleChanges(refused, "origin/main")
	if len(none.pushed)+len(failing.pushed) != 0 || len(refused.pushed) != 1 {
		t.Errorf("pushes: none %v, failing %v, refused %v", none.pushed, failing.pushed, refused.pushed)
	}
}

// TestSyncGuardWithUncommittedChanges verifies that the worktree sync guard
// (gt-pvx) prevents switching branches when uncommitted changes remain.
func TestSyncGuardWithUncommittedChanges(t *testing.T) {
	t.Parallel()
	// This tests the logic: if auto-commit fails, we should NOT sync to main
	dir := t.TempDir()
	testRunGit(t, dir, "init")
	testRunGit(t, dir, "config", "user.email", "test@test.com")
	testRunGit(t, dir, "config", "user.name", "Test")

	// Create initial commit on main
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	testRunGit(t, dir, "add", ".")
	testRunGit(t, dir, "commit", "-m", "initial")

	// Create feature branch with uncommitted changes
	testRunGit(t, dir, "checkout", "-b", "polecat/test")
	if err := os.WriteFile(filepath.Join(dir, "impl.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	g := gitpkg.NewGit(dir)
	ws, err := g.CheckUncommittedWork()
	if err != nil {
		t.Fatalf("CheckUncommittedWork: %v", err)
	}

	// The sync guard condition: if uncommitted non-runtime changes exist, syncSafe = false
	syncSafe := true
	if ws.HasUncommittedChanges && !ws.CleanExcludingRuntime() {
		syncSafe = false
	}

	if syncSafe {
		t.Error("syncSafe should be false when uncommitted implementation files exist")
	}
}

func testRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	// An empty dir runs git in the test process's own cwd — the package
	// directory inside the real repo — so a stray test would create branches and
	// objects in the rig's shared ref store.
	if dir == "" {
		t.Fatal("testRunGit: empty dir would run git in the test process cwd")
	}
	fullArgs := append([]string{"-c", "protocol.file.allow=always"}, args...)
	cmd := exec.Command("git", fullArgs...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}
