package testpolicy

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var seed = flag.Bool("seed", false, "print the unconverted.txt a fresh checkout needs, instead of failing")

// TestPolicy applies the unit-test rules to every package not listed in
// unconverted.txt, and fails a listed package that already passes, so the
// list can only shrink.
func TestPolicy(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	unconverted, err := ReadList("unconverted.txt")
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := PackageDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	var dirty []string
	var converted []string
	for _, dir := range dirs {
		rel := filepath.ToSlash(strings.TrimPrefix(dir, root+string(filepath.Separator)))
		vs, err := ScanDir(dir)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if len(vs) > 0 {
			dirty = append(dirty, rel)
		}
		listed := unconverted[rel]
		delete(unconverted, rel)
		if !listed {
			converted = append(converted, dir)
		}
		switch {
		case *seed:
		case listed && len(vs) == 0:
			t.Errorf("%s is in unconverted.txt but passes every rule: delete its line", rel)
		case !listed:
			for _, v := range vs {
				t.Error(v.String())
			}
		}
	}
	if *seed {
		sort.Strings(dirty)
		fmt.Println(strings.Join(dirty, "\n"))
		return
	}
	for rel := range unconverted {
		t.Errorf("unconverted.txt lists %s, which is not a Go package directory", rel)
	}
	// CheckContracts holds converted packages to the contract and integration-
	// naming rules. An unconverted package hasn't been through the rollout yet
	// (docs/testing.md, tmux pilot, then the rest of internal/*), so its
	// pre-existing integration tests are exempt for the same reason ScanDir's
	// rules are: the ratchet in unconverted.txt is the single switch that
	// turns every new-testing-discipline rule on for a package, not just the
	// syntax rules ScanDir enforces.
	cvs, err := CheckContracts(root, converted)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range cvs {
		t.Error(v.String())
	}
}
