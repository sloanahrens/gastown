package polecat

import (
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Every other Manager test builds its manager with newTestManager, which
// injects a fake bd, a fake tmux and gitfake. These are the wiring guards
// for NewManager itself: the exported constructor must hand the manager the
// caller's tmux and git, and real git for the directories it opens. The bd
// on PATH is guarded in the integration tier
// (TestIntegrationNewManagerRunsTheBdOnPath).

// TestNewManagerKeepsTheCallersTmux checks that NewManager stores the tmux it
// is given, and stores no tmux at all (not a typed nil, which would defeat
// every "m.tmux != nil" guard) when given none.
func TestNewManagerKeepsTheCallersTmux(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{Name: "rig", Path: t.TempDir()}
	if m := NewManager(r, nil, nil); m.tmux != nil {
		t.Fatalf("NewManager(nil tmux).tmux = %#v, want nil", m.tmux)
	}
	tm := tmux.NewTmuxWithSocket("gt-test-unused")
	if m := NewManager(r, nil, tm); m.tmux != tm {
		t.Fatalf("NewManager(tm).tmux = %#v, want the caller's %p", m.tmux, tm)
	}
}

// TestNewManagerOpensRealGit checks that NewManager's opener builds *git.Git,
// for the working directories and bare repositories it opens, and keeps the
// caller's git (and no typed nil when given none). Constructing a *git.Git
// runs nothing.
func TestNewManagerOpensRealGit(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{Name: "rig", Path: t.TempDir()}
	if m := NewManager(r, nil, nil); m.git != nil {
		t.Fatalf("NewManager(nil git).git = %#v, want nil", m.git)
	}
	//testpolicy:allow no-git — a *git.Git NewManager must keep; nothing runs it
	g := git.NewGit(r.Path)
	m := NewManager(r, g, nil)
	if m.git != g {
		t.Fatalf("NewManager(g).git = %#v, want the caller's %p", m.git, g)
	}
	if _, ok := m.gits.Open(r.Path).(*git.Git); !ok {
		t.Fatalf("Open = %T, want *git.Git", m.gits.Open(r.Path))
	}
	if _, ok := m.gits.OpenDir(filepath.Join(r.Path, ".repo.git"), "").(*git.Git); !ok {
		t.Fatalf("OpenDir = %T, want *git.Git", m.gits.OpenDir(r.Path, ""))
	}
	if sm := NewSessionManager(nil, r); sm.tmux != nil {
		t.Fatalf("NewSessionManager(nil tmux).tmux = %#v, want nil", sm.tmux)
	}
	if _, ok := NewSessionManager(nil, r).gits.Open(r.Path).(*git.Git); !ok {
		t.Fatal("NewSessionManager's opener does not build *git.Git")
	}
}
