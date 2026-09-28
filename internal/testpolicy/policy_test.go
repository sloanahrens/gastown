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

// maxUnconverted is the number of entries unconverted.txt held when this
// ratchet was added (Ruling R11). The list only shrinks: converting a
// package deletes its line from unconverted.txt AND lowers maxUnconverted
// here, in the same change. It must never grow.
const maxUnconverted = 65

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
	// originallyListed is a snapshot of unconverted.txt's membership, taken
	// before the loop below starts deleting from unconverted to find stale
	// entries. It is consulted after CheckContracts runs, to tell a stale
	// integration-name finding (a package the rollout hasn't reached yet)
	// from a real one.
	originallyListed := make(map[string]bool, len(unconverted))
	for rel := range unconverted {
		originallyListed[rel] = true
	}
	if !*seed && len(originallyListed) > maxUnconverted {
		t.Errorf("unconverted.txt has %d entries, want at most %d: the list only shrinks — converting a package deletes its line here AND lowers maxUnconverted in policy_test.go", len(originallyListed), maxUnconverted)
	}
	dirs, err := PackageDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	var dirty []string
	for _, dir := range dirs {
		rel := filepath.ToSlash(strings.TrimPrefix(dir, root+string(filepath.Separator)))
		vs, exemptions, err := ScanDirWithExemptions(dir)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if len(vs) > 0 {
			dirty = append(dirty, rel)
		}
		listed := unconverted[rel]
		delete(unconverted, rel)
		switch {
		case *seed:
		case listed && len(vs) == 0:
			t.Errorf("%s is in unconverted.txt but passes every rule: delete its line", rel)
		case !listed:
			for _, v := range vs {
				t.Error(v.String())
			}
			// The test logs every exemption (spec §4), but only for
			// packages the ratchet already covers: an unconverted
			// package's allow comments (if any) aren't policy yet.
			for _, e := range exemptions {
				t.Logf("exemption %s:%d %s — %s", e.Pos.Filename, e.Pos.Line, e.Rule, e.Reason)
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
	// CheckContracts runs over every package: a fake-contract finding, or a
	// call to a contract from another package, can only be judged correctly
	// with the whole picture. Only its integration-name findings are then
	// filtered, one at a time, against the ratchet: a package that hasn't
	// been through the rollout yet (docs/testing.md, tmux pilot, then the
	// rest of internal/*) is exempt from the naming convention alone, the
	// same way it's exempt from ScanDir's rules. fake-contract findings are
	// never filtered.
	cvs, err := CheckContracts(root, dirs)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range cvs {
		if v.Rule == RuleIntegrationName {
			pkgDir := filepath.ToSlash(strings.TrimPrefix(filepath.Dir(v.Pos.Filename), root+string(filepath.Separator)))
			if originallyListed[pkgDir] {
				continue
			}
		}
		t.Error(v.String())
	}
}
