package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// danglingRig is a rig with a crew clone crew/sloan that has a .githooks
// directory and one commit, and a linked worktree crew/jay-inv of that clone.
// The worktree reads the clone's config, as git makes it.
func danglingRig(t *testing.T) (ctx *CheckContext, clone, worktree string) {
	t.Helper()
	const rigName = "testrig"
	townRoot := t.TempDir()
	clone = filepath.Join(townRoot, rigName, "crew", "sloan")
	worktree = filepath.Join(townRoot, rigName, "crew", "jay-inv")
	gf := gitfake.New()
	gf.InitRepo(t, clone)
	if err := os.MkdirAll(filepath.Join(clone, ".githooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	gf.Commit(t, clone, "main", "init", map[string]string{"README": "x\n"})
	ctx = withGit(&CheckContext{TownRoot: townRoot, RigName: rigName}, gf)
	if err := ctx.git(clone).WorktreeAddExistingForce(worktree, "main"); err != nil {
		t.Fatalf("WorktreeAddExistingForce: %v", err)
	}
	return ctx, clone, worktree
}

// TestHooksPathChecksReportADanglingPath: a clone whose core.hooksPath names a
// directory that does not exist is reported by both checks, with the config it
// comes from and the linked worktrees that share it. Fix writes .githooks into
// that shared config, so the worktree reads it too.
func TestHooksPathChecksReportADanglingPath(t *testing.T) {
	t.Parallel()
	ctx, clone, worktree := danglingRig(t)
	dangling := filepath.Join(ctx.TownRoot, "gone", ".beads", "hooks")
	if err := ctx.git(clone).ConfigSet("core.hooksPath", dangling); err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(clone, ".git", "config")

	perRig := NewHooksPathConfiguredCheck()
	perRigResult := perRig.Run(ctx)
	assertDanglingDetail(t, "per-rig", perRigResult, "crew/sloan", dangling, configFile, worktree)

	allRigs := NewHooksPathAllRigsCheck()
	allRigsResult := allRigs.Run(ctx)
	assertDanglingDetail(t, "all-rigs", allRigsResult, filepath.Join("testrig", "crew", "sloan"), dangling, configFile, worktree)

	if err := perRig.Fix(ctx); err != nil {
		t.Fatalf("per-rig Fix: %v", err)
	}
	for _, dir := range []string{clone, worktree} {
		got, err := ctx.git(dir).ConfigGet("core.hooksPath")
		if err != nil {
			t.Fatalf("ConfigGet(%s): %v", dir, err)
		}
		if got != ".githooks" {
			t.Fatalf("core.hooksPath read from %s = %q after Fix, want .githooks", dir, got)
		}
	}
	if got := NewHooksPathConfiguredCheck().Run(ctx); got.Status != StatusOK {
		t.Fatalf("per-rig after Fix = %v %v, want OK", got.Status, got.Details)
	}
}

// assertDanglingDetail requires one Warning detail for the clone at rel that
// names the dangling value, its config file and the linked worktree.
func assertDanglingDetail(t *testing.T, label string, result *CheckResult, rel, value, configFile, worktree string) {
	t.Helper()
	if result.Status != StatusWarning {
		t.Fatalf("%s = %v %v, want Warning", label, result.Status, result.Details)
	}
	if len(result.Details) != 1 {
		t.Fatalf("%s details = %v, want one detail for %s", label, result.Details, rel)
	}
	detail := result.Details[0]
	for _, want := range []string{rel, value, configFile, worktree} {
		if !strings.Contains(detail, want) {
			t.Fatalf("%s detail %q does not name %q", label, detail, want)
		}
	}
}

// TestHooksPathChecksLeaveAValidPathAlone: a core.hooksPath that names an
// existing directory, anywhere, is configured. Neither check reports it, and
// Fix does not rewrite it.
func TestHooksPathChecksLeaveAValidPathAlone(t *testing.T) {
	t.Parallel()
	ctx := hooksPathRig(t, "testrig", filepath.Join("crew", "sloan"))
	clone := filepath.Join(ctx.RigPath(), "crew", "sloan")
	valid := t.TempDir()
	if err := ctx.git(clone).ConfigSet("core.hooksPath", valid); err != nil {
		t.Fatal(err)
	}

	if got := NewHooksPathConfiguredCheck().Run(ctx); got.Status != StatusOK {
		t.Fatalf("per-rig = %v %v, want OK", got.Status, got.Details)
	}
	if got := NewHooksPathAllRigsCheck().Run(ctx); got.Status != StatusOK {
		t.Fatalf("all-rigs = %v %v, want OK", got.Status, got.Details)
	}
	if err := NewHooksPathConfiguredCheck().Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	got, err := ctx.git(clone).ConfigGet("core.hooksPath")
	if err != nil {
		t.Fatal(err)
	}
	if got != valid {
		t.Fatalf("core.hooksPath = %q after Fix, want the valid path %q", got, valid)
	}
}

// TestHooksPathChecksRefuseAnUnreadablePath: a core.hooksPath whose target
// cannot be stat'd (a parent is a file) is not judged. The check errors, and Fix
// does not rewrite the value.
func TestHooksPathChecksRefuseAnUnreadablePath(t *testing.T) {
	t.Parallel()
	ctx := hooksPathRig(t, "testrig", filepath.Join("crew", "sloan"))
	clone := filepath.Join(ctx.RigPath(), "crew", "sloan")
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(file, "hooks")
	if err := ctx.git(clone).ConfigSet("core.hooksPath", unreadable); err != nil {
		t.Fatal(err)
	}

	check := NewHooksPathConfiguredCheck()
	if got := check.Run(ctx); got.Status != StatusError {
		t.Fatalf("per-rig = %v %v, want Error", got.Status, got.Details)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	got, err := ctx.git(clone).ConfigGet("core.hooksPath")
	if err != nil {
		t.Fatal(err)
	}
	if got != unreadable {
		t.Fatalf("core.hooksPath = %q after Fix, want the unreadable value kept", got)
	}
}

// TestHooksPathChecksRefuseAnUnreadableClone: a clone with a .git and a
// .githooks directory that git cannot open is not judged unset. Its config
// cannot be located, so the check errors and names the clone.
func TestHooksPathChecksRefuseAnUnreadableClone(t *testing.T) {
	t.Parallel()
	const rigName = "testrig"
	townRoot := t.TempDir()
	clone := filepath.Join(townRoot, rigName, "crew", "sloan")
	for _, dir := range []string{filepath.Join(clone, ".git"), filepath.Join(clone, ".githooks")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx := withGit(&CheckContext{TownRoot: townRoot, RigName: rigName}, gitfake.New())

	got := NewHooksPathConfiguredCheck().Run(ctx)
	if got.Status != StatusError {
		t.Fatalf("per-rig = %v %v, want Error", got.Status, got.Details)
	}
	if len(got.Details) != 1 || !strings.HasPrefix(got.Details[0], "crew/sloan: ") {
		t.Fatalf("details = %v, want one detail naming crew/sloan", got.Details)
	}
}

// failingRepo is a doctor Repo whose config read, config write or worktree list
// is refused. Each test sets the one call it drives.
type failingRepo struct {
	Repo
	failGet, failSet, failList bool
}

func (r failingRepo) ConfigGet(key string) (string, error) {
	if r.failGet {
		return "", errors.New("config read refused")
	}
	return r.Repo.ConfigGet(key)
}

func (r failingRepo) ConfigSet(key, value string) error {
	if r.failSet {
		return errors.New("config write refused")
	}
	return r.Repo.ConfigSet(key, value)
}

func (r failingRepo) WorktreeList() ([]git.Worktree, error) {
	if r.failList {
		return nil, errors.New("worktree list refused")
	}
	return r.Repo.WorktreeList()
}

// failGitOpens makes every repo ctx opens from now on refuse the calls in fail.
func failGitOpens(ctx *CheckContext, fail failingRepo) {
	open := ctx.openGit
	ctx.openGit = func(gitDir, workDir string) Repo {
		wrapped := fail
		wrapped.Repo = open(gitDir, workDir)
		return wrapped
	}
}

// TestHooksPathChecksRefuseAConfigThatCannotBeRead: a clone whose config cannot
// be read is not judged unset. The check errors rather than report it.
func TestHooksPathChecksRefuseAConfigThatCannotBeRead(t *testing.T) {
	t.Parallel()
	ctx := hooksPathRig(t, "testrig", filepath.Join("crew", "sloan"))
	failGitOpens(ctx, failingRepo{failGet: true})

	if got := NewHooksPathConfiguredCheck().Run(ctx); got.Status != StatusError {
		t.Fatalf("per-rig = %v %v, want Error", got.Status, got.Details)
	}
}

// TestHooksPathChecksRefuseAWorktreeListThatCannotBeRead: a dangling clone whose
// linked worktrees cannot be listed is not reported with a partial sharer list.
// The check errors.
func TestHooksPathChecksRefuseAWorktreeListThatCannotBeRead(t *testing.T) {
	t.Parallel()
	ctx := hooksPathRig(t, "testrig", filepath.Join("crew", "sloan"))
	clone := filepath.Join(ctx.RigPath(), "crew", "sloan")
	if err := ctx.git(clone).ConfigSet("core.hooksPath", filepath.Join(ctx.TownRoot, "gone")); err != nil {
		t.Fatal(err)
	}
	failGitOpens(ctx, failingRepo{failList: true})

	if got := NewHooksPathConfiguredCheck().Run(ctx); got.Status != StatusError {
		t.Fatalf("per-rig = %v %v, want Error", got.Status, got.Details)
	}
}

// TestHooksPathFixReportsAFailedWrite: when a clone is reported unset and Fix
// cannot write its config, Fix returns the error instead of passing.
func TestHooksPathFixReportsAFailedWrite(t *testing.T) {
	t.Parallel()
	ctx := hooksPathRig(t, "testrig", filepath.Join("crew", "sloan"))
	check := NewHooksPathConfiguredCheck()
	if got := check.Run(ctx); got.Status != StatusWarning {
		t.Fatalf("per-rig = %v %v, want Warning", got.Status, got.Details)
	}
	failGitOpens(ctx, failingRepo{failSet: true})

	if err := check.Fix(ctx); err == nil {
		t.Fatal("Fix succeeded, want the refused write returned as an error")
	}
}
