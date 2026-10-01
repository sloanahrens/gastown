package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// The tests in this file pin canonicalizeToolPath to kernel path resolution
// (gt-22hdp.41). The oracle is the kernel itself: each case asks the guard
// where a path lands, then writes through the ORIGINAL spelling with
// os.WriteFile — which hands the string to open(2) unchanged — and checks that
// the file the kernel created is the one the guard named. filepath.EvalSymlinks
// cannot serve as the oracle on the raw spelling, because it cleans "sym/.."
// lexically exactly as the old guard did.

// mustSymlink creates a symlink whose stored text is exactly target.
func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("creating the parent of %s: %v", link, err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
}

// resolvedTempDir is t.TempDir with its own symlinks resolved (/var -> /private/var
// on macOS), so expected paths can be spelled in canonical form.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temp dir: %v", err)
	}
	return dir
}

// assertKernelLandsAt checks the guard's answer for raw against the kernel:
// canonicalizeToolPath(raw, cwd) must equal want, want must be symlink-free, and
// a write through raw must create exactly the file at want.
func assertKernelLandsAt(t *testing.T, raw, cwd, want string) {
	t.Helper()
	got, ok := canonicalizeToolPath(bareGuardProcess, raw, cwd)
	if !ok {
		t.Fatalf("canonicalizeToolPath(%q) failed to resolve; the kernel resolves it to %s", raw, want)
	}
	if got != want {
		t.Errorf("canonicalizeToolPath(%q) = %s, want %s (where the kernel writes)", raw, got, want)
	}

	// Kernel oracle: write through the raw spelling (relative spellings through
	// cwd, which is how the session's shell sees them).
	writePath := raw
	if !filepath.IsAbs(raw) {
		writePath = cwd + string(filepath.Separator) + raw
	}
	if err := os.WriteFile(writePath, []byte("probe\n"), 0o644); err != nil {
		t.Fatalf("kernel write through %q: %v", writePath, err)
	}
	viaRaw, err := os.Stat(writePath)
	if err != nil {
		t.Fatalf("stat through %q: %v", writePath, err)
	}
	atWant, err := os.Stat(want)
	if err != nil {
		t.Fatalf("the kernel wrote through %q but nothing exists at %s: %v", writePath, want, err)
	}
	if !os.SameFile(viaRaw, atWant) {
		t.Errorf("the kernel wrote %q somewhere other than %s", writePath, want)
	}
	if resolved, err := filepath.EvalSymlinks(want); err != nil || resolved != want {
		t.Errorf("expected path %s is not canonical (EvalSymlinks = %s, %v)", want, resolved, err)
	}
}

// TestCanonicalizeToolPathMatchesKernel covers each place the old
// longest-existing-prefix resolution disagreed with the kernel.
func TestCanonicalizeToolPathMatchesKernel(t *testing.T) {
	t.Parallel()

	// (1) "sym/.." — the kernel follows sym, THEN takes "..". A lexical Clean
	// first collapses it to the link's own directory.
	t.Run("dot-dot after a symlink applies to the link target", func(t *testing.T) {
		t.Parallel()
		root := resolvedTempDir(t)
		mustMkdirAll(t, filepath.Join(root, "town", "mayor"))
		mustMkdirAll(t, filepath.Join(root, "tmp", "L"))
		mustSymlink(t, filepath.Join(root, "town", "mayor"), filepath.Join(root, "tmp", "L", "sym"))
		raw := filepath.Join(root, "tmp", "L") + "/sym/../PWNED"
		assertKernelLandsAt(t, raw, "", filepath.Join(root, "town", "PWNED"))
	})

	// (2) Relative link text is relative to the LINK's directory, never to the
	// session cwd; an intermediate relative link whose target does not exist
	// yet exercises the path the old code resolved by walking up.
	t.Run("relative intermediate link resolves against the link's directory", func(t *testing.T) {
		t.Parallel()
		root := resolvedTempDir(t)
		cwd := filepath.Join(root, "work", "deep", "er")
		mustMkdirAll(t, cwd)
		mustMkdirAll(t, filepath.Join(root, "town", "mayor"))
		mustMkdirAll(t, filepath.Join(root, "tmp", "L", "a"))
		// Resolved against cwd this text would name <root>/work/town/...; from
		// the link's directory it names <root>/town/mayor/inbox.
		mustSymlink(t, "../../../town/mayor/inbox", filepath.Join(root, "tmp", "L", "a", "rel"))
		mustMkdirAll(t, filepath.Join(root, "town", "mayor", "inbox"))
		raw := filepath.Join(root, "tmp", "L", "a", "rel", "PWNED")
		assertKernelLandsAt(t, raw, cwd, filepath.Join(root, "town", "mayor", "inbox", "PWNED"))
	})

	// (3) A dangling final symlink: O_CREAT follows it and creates the target.
	t.Run("dangling final absolute symlink creates its target", func(t *testing.T) {
		t.Parallel()
		root := resolvedTempDir(t)
		mustMkdirAll(t, filepath.Join(root, "town", "mayor"))
		mustSymlink(t, filepath.Join(root, "town", "mayor", "PWNED"), filepath.Join(root, "tmp", "dl"))
		assertKernelLandsAt(t, filepath.Join(root, "tmp", "dl"), "", filepath.Join(root, "town", "mayor", "PWNED"))
	})

	// The reviewer's end-to-end reproduction: a dangling final link whose
	// relative text climbs out with ".." segments.
	t.Run("dangling final relative symlink with dot-dot segments", func(t *testing.T) {
		t.Parallel()
		root := resolvedTempDir(t)
		mustMkdirAll(t, filepath.Join(root, "town", "mayor"))
		mustSymlink(t, "../../../../town/mayor/PWNED", filepath.Join(root, "tmp", "L", "a", "b", "dl"))
		assertKernelLandsAt(t, filepath.Join(root, "tmp", "L", "a", "b", "dl"), "", filepath.Join(root, "town", "mayor", "PWNED"))
	})

	t.Run("chain of relative links ending dangling", func(t *testing.T) {
		t.Parallel()
		root := resolvedTempDir(t)
		mustMkdirAll(t, filepath.Join(root, "town", "mayor"))
		mustMkdirAll(t, filepath.Join(root, "tmp", "x", "y"))
		mustSymlink(t, "x/y/second", filepath.Join(root, "tmp", "first"))
		mustSymlink(t, "../../../town/mayor/PWNED", filepath.Join(root, "tmp", "x", "y", "second"))
		assertKernelLandsAt(t, filepath.Join(root, "tmp", "first"), "", filepath.Join(root, "town", "mayor", "PWNED"))
	})

	t.Run("relative raw path through a relative dangling link", func(t *testing.T) {
		t.Parallel()
		root := resolvedTempDir(t)
		cwd := filepath.Join(root, "tmp", "L")
		mustMkdirAll(t, filepath.Join(root, "town", "mayor"))
		mustSymlink(t, "../../../town/mayor/PWNED", filepath.Join(cwd, "a", "dl"))
		assertKernelLandsAt(t, "a/dl", cwd, filepath.Join(root, "town", "mayor", "PWNED"))
	})

	// Kept behaviour: a missing leaf (and a missing directory run below it) is
	// appended as spelled, and ".." after a missing directory pops it, as
	// mkdir -p would create it and then climb back out.
	t.Run("missing components are kept as spelled", func(t *testing.T) {
		t.Parallel()
		root := resolvedTempDir(t)
		mustMkdirAll(t, filepath.Join(root, "w"))
		got, ok := canonicalizeToolPath(bareGuardProcess, filepath.Join(root, "w")+"/new/sub/../x.go", "")
		if !ok {
			t.Fatal("expected a path under an existing directory to resolve")
		}
		if want := filepath.Join(root, "w", "new", "x.go"); got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("symlink cycle fails closed", func(t *testing.T) {
		t.Parallel()
		root := resolvedTempDir(t)
		mustSymlink(t, "b", filepath.Join(root, "a"))
		mustSymlink(t, "a", filepath.Join(root, "b"))
		if got, ok := canonicalizeToolPath(bareGuardProcess, filepath.Join(root, "a", "x"), ""); ok {
			t.Errorf("a symlink cycle resolved to %s, want a fail-closed failure", got)
		}
	})
}

// polecatSymlinkTownTmp is the hermetic $TMPDIR newPolecatTestTown sets up
// (<root>/tmp), resolved so symlinks built below it compare canonically.
func polecatSymlinkTownTmp(t *testing.T, p polecatTestTown) string {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(filepath.Join(p.root, "tmp"))
	if err != nil {
		t.Fatalf("resolving the test town's temp dir: %v", err)
	}
	return tmp
}

// TestPolecatPathGuardSymlinkResolution drives the same resolution cases
// through the guard: each raw path looks like temp scratch, but the kernel
// writes it into the town's mayor dir, and the guard must decide on that real
// location — for a Write tool call and for a Bash redirect alike.
func TestPolecatPathGuardSymlinkResolution(t *testing.T) {
	t.Parallel()
	p := newPolecatTestTown(t)
	tmp := polecatSymlinkTownTmp(t, p)
	town, err := filepath.EvalSymlinks(p.town)
	if err != nil {
		t.Fatalf("resolving the town: %v", err)
	}
	mayor := filepath.Join(town, "mayor")

	// (1) sym/.. : <tmp>/L/sym -> <town>/mayor, so sym/../PWNED is <town>/PWNED.
	mustSymlink(t, mayor, filepath.Join(tmp, "L", "sym"))
	// (3) dangling absolute final link into the mayor dir.
	mustSymlink(t, filepath.Join(mayor, "PWNED-abs"), filepath.Join(tmp, "dl-abs"))
	// (2)+(3) the reviewer's repro: dangling relative link climbing with "..".
	relText, err := filepath.Rel(filepath.Join(tmp, "L", "a", "b"), filepath.Join(mayor, "PWNED-rel"))
	if err != nil {
		t.Fatalf("computing link text: %v", err)
	}
	mustSymlink(t, relText, filepath.Join(tmp, "L", "a", "b", "dl"))

	cases := []struct {
		name string
		raw  string
		want string // where the kernel writes
	}{
		{"dot-dot after a symlink", filepath.Join(tmp, "L") + "/sym/../PWNED", filepath.Join(town, "PWNED")},
		{"dangling absolute final link", filepath.Join(tmp, "dl-abs"), filepath.Join(mayor, "PWNED-abs")},
		{"dangling relative final link with dot-dot text", filepath.Join(tmp, "L", "a", "b", "dl"), filepath.Join(mayor, "PWNED-rel")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := canonicalizeToolPath(p.proc(), tc.raw, p.worktree); !ok || got != tc.want {
				t.Errorf("canonicalizeToolPath(%q) = %q, %v; the kernel writes %s", tc.raw, got, ok, tc.want)
			}
			if err := p.run(t, "Write", fileInput(tc.raw)); err == nil {
				t.Errorf("Write to %s (lands at %s): expected BLOCK, got allow", tc.raw, tc.want)
			}
			if err := p.run(t, "Bash", commandInput("echo pwned > "+tc.raw)); err == nil {
				t.Errorf("redirect to %s (lands at %s): expected BLOCK, got allow", tc.raw, tc.want)
			}
			// The kernel oracle, after the guard has decided.
			if err := os.WriteFile(tc.raw, []byte("probe\n"), 0o644); err != nil {
				t.Fatalf("kernel write through %s: %v", tc.raw, err)
			}
			if _, err := os.Stat(tc.want); err != nil {
				t.Errorf("the kernel did not write %s through %s: %v", tc.want, tc.raw, err)
			}
		})
	}
}

// TestPolecatPathGuardSymlinkDotDotBackToScratch is the false-positive side of
// case (1): a link inside the worktree that points into temp scratch, followed
// by "..", lands in scratch — the guard must allow it, not judge the lexical
// spelling (which names the polecat dir, outside the worktree).
func TestPolecatPathGuardSymlinkDotDotBackToScratch(t *testing.T) {
	t.Parallel()
	p := newPolecatTestTown(t)
	tmp := polecatSymlinkTownTmp(t, p)
	mustMkdirAll(t, filepath.Join(tmp, "L", "a"))
	mustSymlink(t, filepath.Join(tmp, "L", "a"), filepath.Join(p.worktree, "toscratch"))
	raw := p.worktree + "/toscratch/../../x.go"
	if err := p.run(t, "Write", fileInput(raw)); err != nil {
		t.Errorf("Write to %s lands at %s (scratch): expected allow, got block: %v", raw, filepath.Join(tmp, "x.go"), err)
	}
}
