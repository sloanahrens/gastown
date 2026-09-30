package testpolicy

import (
	"path/filepath"
	"strings"
	"testing"
)

// minGitFree is the number of entries gitfree.txt holds. The list only
// grows: converting a package onto gitfake adds its line AND raises this, in
// the same change.
const minGitFree = 10

func TestGitFreeFindingsFixtures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		lines      []int // lines of the violations, in order
		withoutGit bool
	}{
		{"clean", nil, true},             // gitfake and a comment only
		{"execgit", []int{11, 12}, true}, // exec.Command and CommandContext with "git"; "gitk" is not git
		{"newgit", []int{11, 12}, true},  // git.NewGit and git.NewGitWithDir, under an import alias
		{"inpkg", []int{7}, true},        // NewGit unqualified inside package git
		{"testutil", nil, true},          // WithoutGit unqualified inside package testutil
		{"nomain", nil, false},           // HermeticMain without WithoutGit
		{"integration_only", nil, true},  // only an integration-tagged file runs git
		{"allowed", nil, true},           // an exemption with a reason
	} {
		vs, withoutGit, err := GitFreeFindings(filepath.Join("testdata", "gitfree", tc.name))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var lines []int
		for _, v := range vs {
			if v.Rule != RuleNoGit {
				t.Errorf("%s: rule %s, want %s", tc.name, v.Rule, RuleNoGit)
			}
			lines = append(lines, v.Pos.Line)
		}
		if len(lines) != len(tc.lines) || (len(lines) > 0 && !equalInts(lines, tc.lines)) {
			t.Errorf("%s: violations %v, want lines %v", tc.name, vs, tc.lines)
		}
		if withoutGit != tc.withoutGit {
			t.Errorf("%s: withoutGit = %v, want %v", tc.name, withoutGit, tc.withoutGit)
		}
	}
}

func equalInts(a, b []int) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return len(a) == len(b)
}

// TestCheckGitFreeFixtures drives each finding CheckGitFree reports against
// the fixture tree, and shows an unlisted package is not checked.
func TestCheckGitFreeFixtures(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("testdata", "gitfree"))
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, name := range []string{"clean", "execgit", "newgit", "nomain"} {
		dirs = append(dirs, filepath.Join(root, name))
	}
	listed := map[string]bool{"clean": true, "execgit": true, "nomain": true, "gone": true}
	got, err := CheckGitFree(root, dirs, listed)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"execgit/x_test.go:11:6: [no-git] runs git",
		"execgit/x_test.go:12:6: [no-git] runs git",
		"gitfree.txt lists nomain, but its unit-tier TestMain does not pass testutil.WithoutGit()",
		"gitfree.txt lists gone, which is not a Go package directory",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("findings lack %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "newgit") {
		t.Errorf("an unlisted package was checked:\n%s", joined)
	}
	if len(got) != 4 {
		t.Errorf("got %d findings, want 4:\n%s", len(got), joined)
	}
}

// TestGitFree holds every package in gitfree.txt to the no-git rule: its
// unit tier neither runs git nor builds the real git wrapper, and its
// TestMain puts the refusing git on PATH (docs/testing.md, "Seams for
// external tools").
func TestGitFree(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	listed, err := ReadList("gitfree.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) < minGitFree {
		t.Errorf("gitfree.txt has %d entries, want at least %d: the list only grows", len(listed), minGitFree)
	}
	dirs, err := PackageDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := CheckGitFree(root, dirs, listed)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
}
