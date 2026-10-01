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
