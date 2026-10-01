package testpolicy

import (
	"flag"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var seedRealGit = flag.Bool("seed-realgit", false, "print the realgit.txt the tree needs, instead of failing")

// maxRealGit is the number of entries realgit.txt holds: unit-tier test
// files outside gitfree.txt that still run real git. The list only shrinks:
// moving a file's git onto gitfake or into the integration tier deletes its
// line AND lowers this, in the same change.
const maxRealGit = 12

// TestRealGit holds every unit-tier test file outside gitfree.txt to the
// realgit.txt baseline, and logs the converted share of test lines, which
// counts a package with a baseline file as unconverted.
func TestRealGit(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	gitFree, err := ReadList("gitfree.txt")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := ReadList("realgit.txt")
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := PackageDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	found, err := RealGitFiles(root, dirs, gitFree)
	if err != nil {
		t.Fatal(err)
	}
	if *seedRealGit {
		fmt.Println(strings.Join(sortedKeys(found), "\n"))
		return
	}
	if len(baseline) > maxRealGit {
		t.Errorf("realgit.txt has %d entries, want at most %d: the list only shrinks", len(baseline), maxRealGit)
	}
	unconverted, err := ReadList("unconverted.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range CheckRealGit(root, found, baseline, unconverted) {
		t.Error(f)
	}
	perPkg := map[string]int{}
	for f := range found {
		perPkg[filepath.ToSlash(filepath.Dir(f))]++
	}
	pkgs := sortedKeys(perPkg)
	sort.SliceStable(pkgs, func(i, j int) bool { return perPkg[pkgs[i]] > perPkg[pkgs[j]] })
	for _, p := range pkgs {
		t.Logf("real git in the unit tier: %s %d files", p, perPkg[p])
	}
	converted, total, err := ConvertedShare(root, dirs, unconverted, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if total > 0 {
		t.Logf("converted share of test lines: %d/%d = %.1f%%", converted, total, 100*float64(converted)/float64(total))
	}
}

func TestRealGitFixtures(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("testdata", "realgit"))
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, name := range []string{"helpers", "listed", "clean"} {
		dirs = append(dirs, filepath.Join(root, name))
	}
	found, err := RealGitFiles(root, dirs, map[string]bool{"listed": true})
	if err != nil {
		t.Fatal(err)
	}
	// helpers: a_test.go defines runGit (exec git) and initRepo (calls
	// runGit); b_test.go only calls initRepo; c_test.go calls a local
	// variable named runGit, and setup, which runs git only in the external
	// test package helpers_test (d_test.go), so c_test.go is clean.
	want := []string{"helpers/a_test.go", "helpers/b_test.go", "helpers/d_test.go"}
	if got := sortedKeys(found); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("real-git files = %v, want %v (%v)", got, want, found)
	}
	if vs := found["helpers/b_test.go"]; len(vs) != 1 || !strings.Contains(vs[0].Msg, "calls initRepo, which runs git (a_test.go:") {
		t.Errorf("b_test.go violations = %v, want one call to initRepo", vs)
	}

	got := strings.Join(CheckRealGit(root, found, map[string]bool{
		"helpers/a_test.go": true, "clean/x_test.go": true, "gone/x_test.go": true,
	}, map[string]bool{"helpers": true, "gone": true}), "\n")
	for _, w := range []string{
		"helpers/b_test.go:9:2: [no-git] calls initRepo",
		"helpers/b_test.go runs real git in the unit tier",
		"realgit.txt lists clean/x_test.go, which no longer runs git",
		"realgit.txt lists gone/x_test.go, which does not exist",
		"realgit.txt lists clean/x_test.go, but unconverted.txt does not list clean",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("findings lack %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "helpers/a_test.go") || strings.Contains(got, "not list gone") {
		t.Errorf("a baselined file was reported:\n%s", got)
	}

	converted, total, err := ConvertedShare(root, dirs, map[string]bool{"listed": true}, map[string]bool{"helpers/a_test.go": true})
	if err != nil {
		t.Fatal(err)
	}
	if converted == 0 || converted >= total {
		t.Errorf("ConvertedShare = %d/%d, want only clean's lines converted", converted, total)
	}
}
