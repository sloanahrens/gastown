package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSessionChecker implements polecatSessionChecker for testing, returning
// a canned (alive, err) pair for every session name it's asked about.
type fakeSessionChecker struct {
	alive bool
	err   error
}

func (f *fakeSessionChecker) HasSession(name string) (bool, error) {
	return f.alive, f.err
}

// fakePolecatGit implements polecatGit for testing.
type fakePolecatGit struct {
	branch        string
	branchErr     error
	pushed        bool
	unpushedCount int
	pushCheckErr  error
}

func (f *fakePolecatGit) CurrentBranch() (string, error) {
	return f.branch, f.branchErr
}

func (f *fakePolecatGit) BranchPushedToRemote(localBranch, remote string) (bool, int, error) {
	return f.pushed, f.unpushedCount, f.pushCheckErr
}

// makePolecatDir creates townRoot/<rig>/polecats/<name>/<rig>/.git so
// resolveClonePath finds a real clone path for the polecat.
func makePolecatDir(t *testing.T, townRoot, rig, name string) {
	t.Helper()
	clonePath := filepath.Join(townRoot, rig, "polecats", name, rig)
	if err := os.MkdirAll(filepath.Join(clonePath, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
}

func TestStalledPolecatCheck_Properties(t *testing.T) {
	check := NewStalledPolecatCheck()

	if check.Name() != "stalled-polecats" {
		t.Errorf("Name() = %q, want %q", check.Name(), "stalled-polecats")
	}

	if check.Description() == "" {
		t.Error("Description() should not be empty")
	}

	if !check.CanFix() {
		t.Error("CanFix() should be true — stalled polecats can have branches pushed")
	}

	if check.Category() != CategoryCleanup {
		t.Errorf("Category() = %q, want %q", check.Category(), CategoryCleanup)
	}
}

func TestStalledPolecatCheck_EmptyTownRoot(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := &CheckContext{TownRoot: tmpDir}

	check := NewStalledPolecatCheck()
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want OK for empty town root", result.Status)
	}
}

func TestStalledPolecatCheck_NoPolecats(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: "testrig"}

	check := NewStalledPolecatCheck()
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want OK when no polecats dir exists", result.Status)
	}
}

func TestStalledPolecatCheck_FixNoStalled(t *testing.T) {
	check := NewStalledPolecatCheck()
	// Fix with no stalled polecats should be a no-op
	if err := check.Fix(&CheckContext{TownRoot: t.TempDir()}); err != nil {
		t.Errorf("Fix() with no stalled polecats returned error: %v", err)
	}
}

func TestStalledPolecatCheck_SessionLivenessErrorIsSkipped(t *testing.T) {
	tmpDir := t.TempDir()
	makePolecatDir(t, tmpDir, "testrig", "furiosa")

	check := NewStalledPolecatCheck()
	check.sessionCheckerForTest = &fakeSessionChecker{err: errors.New("tmux: no server running")}

	ctx := &CheckContext{TownRoot: tmpDir, RigName: "testrig"}
	result := check.Run(ctx)

	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v, want StatusSkipped — session liveness could not be determined", result.Status)
	}
	if !strings.HasPrefix(result.Message, "unknown:") {
		t.Errorf("Message = %q, want it to start with %q", result.Message, "unknown:")
	}
	if len(result.Details) == 0 || !strings.Contains(result.Details[0], "furiosa") {
		t.Errorf("Details = %v, want the polecat named in the unknown reason", result.Details)
	}
}

func TestStalledPolecatCheck_PushStatusErrorIsSkipped(t *testing.T) {
	tmpDir := t.TempDir()
	makePolecatDir(t, tmpDir, "testrig", "furiosa")

	check := NewStalledPolecatCheck()
	check.sessionCheckerForTest = &fakeSessionChecker{alive: false}
	check.gitForTest = func(clonePath string) polecatGit {
		return &fakePolecatGit{
			branch:       "polecat/furiosa-abc123",
			pushCheckErr: errors.New("git: unable to contact origin"),
		}
	}

	ctx := &CheckContext{TownRoot: tmpDir, RigName: "testrig"}
	result := check.Run(ctx)

	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v, want StatusSkipped — push status could not be determined", result.Status)
	}
	if !strings.HasPrefix(result.Message, "unknown:") {
		t.Errorf("Message = %q, want it to start with %q", result.Message, "unknown:")
	}
}

func TestStalledPolecatCheck_UnknownDoesNotMaskConfirmedStall(t *testing.T) {
	tmpDir := t.TempDir()
	makePolecatDir(t, tmpDir, "testrig", "furiosa")
	makePolecatDir(t, tmpDir, "testrig", "slate")

	check := NewStalledPolecatCheck()
	calls := 0
	check.sessionCheckerForTest = &fakeSessionChecker{alive: false}
	check.gitForTest = func(clonePath string) polecatGit {
		calls++
		if strings.Contains(clonePath, "furiosa") {
			return &fakePolecatGit{branchErr: errors.New("not a git repository")}
		}
		return &fakePolecatGit{branch: "polecat/slate-def456", unpushedCount: 3}
	}

	ctx := &CheckContext{TownRoot: tmpDir, RigName: "testrig"}
	result := check.Run(ctx)

	// A real, confirmed stall must still be reported even when another
	// polecat in the same run could not be checked.
	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning — a confirmed stall must not be masked by an unknown", result.Status)
	}
	if !strings.Contains(result.Message, "1 stalled") {
		t.Errorf("Message = %q, want it to report the 1 confirmed stall", result.Message)
	}
}

func TestStalledPolecatCheck_ResolveClonePath_NoDir(t *testing.T) {
	check := NewStalledPolecatCheck()
	path := check.resolveClonePath(t.TempDir(), "testrig", "furiosa")
	if path != "" {
		t.Errorf("resolveClonePath() = %q, want empty for nonexistent", path)
	}
}
