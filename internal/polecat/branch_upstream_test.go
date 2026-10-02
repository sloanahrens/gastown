package polecat

import "testing"

// gt-voz8q: a polecat branch tracks where its work lands, never where it was
// cut from.

func TestPolecatBaseRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		base          string
		defaultBranch string
		want          string
	}{
		{"no base uses the rig default", "", "main", "origin/main"},
		{"no base follows a configured default", "", "trunk", "origin/trunk"},
		{"a bare base is origin-qualified", "develop", "main", "origin/develop"},
		{"an origin base passes through", "origin/develop", "main", "origin/develop"},
		{"an upstream base keeps its remote", "upstream/develop", "main", "upstream/develop"},
		{"an integration base passes through", "origin/integration/epic-1", "main", "origin/integration/epic-1"},
		{"a foreign polecat base falls back", "origin/polecat/slate/gt-hqji+mturmak0", "main", "origin/main"},
		{"an unqualified polecat base falls back", "polecat/slate/gt-hqji+mturmak0", "main", "origin/main"},
		{"surrounding space is trimmed", "  origin/develop  ", "main", "origin/develop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := polecatBaseRef(tt.base, tt.defaultBranch); got != tt.want {
				t.Errorf("polecatBaseRef(%q, %q) = %q, want %q", tt.base, tt.defaultBranch, got, tt.want)
			}
		})
	}
}

// TestAddPinsUpstreamToMergeBase: a fresh polecat branch carries the rig default
// as its upstream. git writes that pair itself here, so this pins the behaviour
// against a later start-point change rather than a present defect.
func TestAddPinsUpstreamToMergeBase(t *testing.T) {
	t.Parallel()
	m, w, mayorRig := addRig(t, fakeAddBeads, map[string]string{"README.md": "# Test\n"})

	p, err := m.AddWithOptions("toast", AddOptions{HookBead: "gt-work"})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}
	assertUpstream(t, w, mayorRig, p.Branch, "origin", "refs/heads/main")
}

// TestAddWithForeignPolecatBaseDoesNotInheritUpstream is the regression for the
// observed poison: the dispatch's base names another polecat's branch, so git
// creates the worktree's branch from that remote ref and points the new branch
// at it (branch.autoSetupMerge). Reflogs of the live cases read
// "branch: Created from origin/polecat/slate/gt-hqji+mturmak0".
func TestAddWithForeignPolecatBaseDoesNotInheritUpstream(t *testing.T) {
	t.Parallel()
	m, w, mayorRig := addRig(t, fakeAddBeads, map[string]string{"README.md": "# Test\n"})
	main := w.rev(t, mayorRig, "origin/main")
	foreign := "polecat/slate/gt-hqji+mturmak0"
	w.SetRef(t, mayorRig, "refs/remotes/origin/"+foreign, main)

	p, err := m.AddWithOptions("toast", AddOptions{
		HookBead:   "gt-hqji",
		BaseBranch: "origin/" + foreign,
	})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}
	if tip := w.rev(t, mayorRig, "refs/heads/"+p.Branch); tip != main {
		t.Errorf("branch tip = %s, want the base it was cut from, %s", tip, main)
	}
	assertUpstream(t, w, mayorRig, p.Branch, "origin", "refs/heads/main")
}

// TestReuseRepointsInheritedUpstream: a sandbox re-pointed at a branch repairs
// that branch's tracking instead of leaving the pair a previous life inherited.
func TestReuseRepointsInheritedUpstream(t *testing.T) {
	t.Parallel()
	mgr, mayorRig, _, added, w := canonicalWithPolecats(t, true, "toast")
	toast := added["toast"]
	repo := w.repo(toast.ClonePath)
	for key, value := range map[string]string{
		"branch." + toast.Branch + ".merge":  "refs/heads/polecat/slate/gt-hqji+mturmak0",
		"branch." + toast.Branch + ".remote": "origin",
	} {
		if err := repo.ConfigSet(key, value); err != nil {
			t.Fatalf("ConfigSet(%s): %v", key, err)
		}
	}
	w.SetRef(t, mayorRig, "refs/remotes/origin/"+toast.Branch, w.rev(t, mayorRig, "refs/heads/"+toast.Branch))

	reused, err := mgr.ReuseIdlePolecat("toast", AddOptions{HookBead: "gt-next", ResumeBranch: toast.Branch})
	if err != nil {
		t.Fatalf("ReuseIdlePolecat: %v", err)
	}
	if reused.Branch != toast.Branch {
		t.Fatalf("reused branch = %q, want %q", reused.Branch, toast.Branch)
	}
	assertUpstream(t, w, reused.ClonePath, reused.Branch, "origin", "refs/heads/main")
}

// assertUpstream reads branch's tracking pair out of the repo at dir.
func assertUpstream(t *testing.T, w *world, dir, branch, wantRemote, wantMerge string) {
	t.Helper()
	repo := w.repo(dir)
	for key, want := range map[string]string{
		"branch." + branch + ".remote": wantRemote,
		"branch." + branch + ".merge":  wantMerge,
	} {
		got, err := repo.ConfigGet(key)
		if err != nil {
			t.Fatalf("ConfigGet(%s): %v", key, err)
		}
		if got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}
