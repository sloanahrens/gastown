package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
)

func fakeUncommittedWorkCheck(
	listFn func(*rig.Rig) ([]*polecat.Polecat, error),
	checkFn func(string) (*git.UncommittedWorkStatus, error),
	isTTYFn func() bool,
	promptFn func(string) bool,
) (uncommittedWorkCheck, *bytes.Buffer) {
	var out bytes.Buffer
	return uncommittedWorkCheck{
		listPolecats: listFn,
		workStatus:   checkFn,
		isTerminal:   isTTYFn,
		prompt:       promptFn,
		out:          &out,
	}, &out
}

func testRig() *rig.Rig {
	return &rig.Rig{
		Name: "testrig",
		Path: "/tmp/testrig",
	}
}

func TestCheckUncommittedWork_ListErrorBlocksWithoutForce(t *testing.T) {
	t.Parallel()
	c, out := fakeUncommittedWorkCheck(
		func(*rig.Rig) ([]*polecat.Polecat, error) {
			return nil, errors.New("list failed")
		},
		func(string) (*git.UncommittedWorkStatus, error) {
			t.Fatalf("check should not be called when list fails")
			return nil, nil
		},
		func() bool { return false },
		func(string) bool {
			t.Fatalf("prompt should not be called without --force")
			return false
		},
	)

	proceed := c.check(testRig(), "testrig", "stop", false)
	output := out.String()

	if proceed {
		t.Fatal("expected proceed=false when polecat listing fails without --force")
	}
	if !strings.Contains(output, "Could not check polecats for uncommitted work") {
		t.Fatalf("expected list-error warning, got: %q", output)
	}
	if !strings.Contains(output, "--force") || !strings.Contains(output, "--nuclear") {
		t.Fatalf("expected override hint, got: %q", output)
	}
}

func TestCheckUncommittedWork_ListErrorForceTTYPrompts(t *testing.T) {
	t.Parallel()
	c, _ := fakeUncommittedWorkCheck(
		func(*rig.Rig) ([]*polecat.Polecat, error) {
			return nil, errors.New("list failed")
		},
		func(string) (*git.UncommittedWorkStatus, error) {
			t.Fatalf("check should not be called when list fails")
			return nil, nil
		},
		func() bool { return true },
		func(question string) bool {
			if question != "Proceed anyway?" {
				t.Fatalf("unexpected prompt question: %q", question)
			}
			return true
		},
	)

	proceed := c.check(testRig(), "testrig", "shutdown", true)
	if !proceed {
		t.Fatal("expected proceed=true after force+TTY confirmation")
	}
}

func TestCheckUncommittedWork_PolecatStatusErrorBlocks(t *testing.T) {
	t.Parallel()
	c, out := fakeUncommittedWorkCheck(
		func(*rig.Rig) ([]*polecat.Polecat, error) {
			return []*polecat.Polecat{
				{Name: "alpha", ClonePath: "/tmp/alpha"},
			}, nil
		},
		func(string) (*git.UncommittedWorkStatus, error) {
			return nil, errors.New("git status failed")
		},
		func() bool { return false },
		func(string) bool {
			t.Fatalf("prompt should not be called without --force")
			return false
		},
	)

	proceed := c.check(testRig(), "testrig", "restart", false)
	output := out.String()

	if proceed {
		t.Fatal("expected proceed=false when polecat status check fails")
	}
	if !strings.Contains(output, "Could not verify uncommitted work for") {
		t.Fatalf("expected status-check error warning, got: %q", output)
	}
	if !strings.Contains(output, "alpha") {
		t.Fatalf("expected polecat name in warning, got: %q", output)
	}
}

func TestCheckUncommittedWork_DirtyForceNonTTYBlocks(t *testing.T) {
	t.Parallel()
	c, out := fakeUncommittedWorkCheck(
		func(*rig.Rig) ([]*polecat.Polecat, error) {
			return []*polecat.Polecat{
				{Name: "alpha", ClonePath: "/tmp/alpha"},
			}, nil
		},
		func(string) (*git.UncommittedWorkStatus, error) {
			return &git.UncommittedWorkStatus{
				HasUncommittedChanges: true,
				ModifiedFiles:         []string{"README.md"},
			}, nil
		},
		func() bool { return false },
		func(string) bool {
			t.Fatalf("prompt should not be called in non-TTY mode")
			return false
		},
	)

	proceed := c.check(testRig(), "testrig", "stop", true)
	output := out.String()

	if proceed {
		t.Fatal("expected proceed=false for force in non-TTY mode")
	}
	if !strings.Contains(output, "--force") || !strings.Contains(output, "interactive terminal") {
		t.Fatalf("expected non-TTY force hint, got: %q", output)
	}
}

func TestCheckUncommittedWork_DirtyForceTTYPrompts(t *testing.T) {
	t.Parallel()
	c, _ := fakeUncommittedWorkCheck(
		func(*rig.Rig) ([]*polecat.Polecat, error) {
			return []*polecat.Polecat{
				{Name: "alpha", ClonePath: "/tmp/alpha"},
			}, nil
		},
		func(string) (*git.UncommittedWorkStatus, error) {
			return &git.UncommittedWorkStatus{
				HasUncommittedChanges: true,
				ModifiedFiles:         []string{"README.md"},
			}, nil
		},
		func() bool { return true },
		func(question string) bool {
			if question != "Proceed anyway?" {
				t.Fatalf("unexpected prompt question: %q", question)
			}
			return true
		},
	)

	proceed := c.check(testRig(), "testrig", "stop", true)
	if !proceed {
		t.Fatal("expected proceed=true after force+TTY confirmation")
	}
}
