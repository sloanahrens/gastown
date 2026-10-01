package testpolicy

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestUnitTierMainFixtures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		hasTests, runs bool
	}{
		{"harness", true, true},           // testutil.HermeticMain under an import alias
		{"unittier", true, true},          // unittier.Main
		{"inpkg", true, true},             // Main unqualified inside package unittier
		{"none", true, false},             // a local Main is not unittier.Main
		{"integration_only", true, false}, // the only TestMain is integration-tagged
		{"notests", false, false},
	} {
		hasTests, runs, err := UnitTierMain(filepath.Join("testdata", "tiermain", tc.name))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if hasTests != tc.hasTests || runs != tc.runs {
			t.Errorf("%s: hasTests %v runs %v, want %v %v", tc.name, hasTests, runs, tc.hasTests, tc.runs)
		}
	}
}

func TestCheckUnitTierMainsFixtures(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("testdata", "tiermain"))
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, name := range []string{"harness", "unittier", "none", "integration_only", "notests"} {
		dirs = append(dirs, filepath.Join(root, name))
	}
	got, err := CheckUnitTierMains(root, dirs)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	if len(got) != 2 || !strings.HasPrefix(got[0], "none: its unit tier has no TestMain") || !strings.HasPrefix(got[1], "integration_only: ") {
		t.Errorf("findings = %q, want none and integration_only", joined)
	}
}

// TestUnitTierMain holds every package's unit tier to a TestMain that runs
// testutil.HermeticMain or unittier.Main, which fail the run on a goroutine
// that outlives the tests (docs/testing.md, "The rules").
func TestUnitTierMain(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := PackageDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := CheckUnitTierMains(root, dirs)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
}

func TestAllowedToolsFixture(t *testing.T) {
	t.Parallel()
	names, vs, err := AllowedTools(filepath.Join("testdata", "allowtools"))
	if err != nil {
		t.Fatal(err)
	}
	// "dolt" is in an integration-tagged file, which is not the unit tier.
	if got := strings.Join(names, ","); got != "ps,bd,sh" {
		t.Errorf("names = %s, want ps,bd,sh", got)
	}
	if len(vs) != 1 || vs[0].Rule != RuleAllowTools || vs[0].Pos.Line != 15 {
		t.Errorf("violations = %v, want one for the variable on line 15", vs)
	}
}

// maxAllowedTools is the number of tool names passed to AllowTools across
// the tree: the external tools unit tiers still start. It only shrinks:
// seaming a tool out of a package's unit tier deletes its name from that
// package's TestMain AND lowers this, in the same change.
const maxAllowedTools = 8

// TestAllowedTools holds the AllowTools baseline to maxAllowedTools. Run it
// with -v to see what each package still starts.
func TestAllowedTools(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := PackageDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, dir := range dirs {
		names, vs, err := AllowedTools(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range vs {
			t.Error(v)
		}
		if len(names) > 0 {
			t.Logf("%s: %s", relPath(root, dir), strings.Join(names, " "))
		}
		total += len(names)
	}
	if total > maxAllowedTools {
		t.Errorf("AllowTools names %d tools across the tree, want at most %d: the baseline only shrinks; answer the new tool through a seam instead", total, maxAllowedTools)
	}
	if total < maxAllowedTools {
		t.Errorf("AllowTools names %d tools across the tree, but maxAllowedTools is %d: lower it to %d", total, maxAllowedTools, total)
	}
}
