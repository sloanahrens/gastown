// Command changedpkgs prints the Go packages a branch changed, one "./dir"
// per line, for `make presubmit` (gt-ssyxd). It reads
// `git diff --name-status -M <base>...HEAD`, so a branch is judged by its own
// commits, never by what landed on the base after it branched.
//
// -guards prints instead the arguments for a second `go test` run over the
// tree-wide guard tests the branch's changed paths trigger (gt-ydzwb),
// "-run <union of test names> ./pkg ...", and nothing when it triggers none.
// The guard tests run under -run so presubmit does not pay for the whole
// suite of the package that hosts one: internal/cmd and internal/polecat take
// minutes each, and the guard test inside them takes a second.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/land"
)

func main() {
	base := flag.String("base", "origin/main", "ref the branch is compared against")
	guards := flag.Bool("guards", false, "print the go test arguments for the tree-wide guard tests the branch triggers")
	flag.Parse()

	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "changedpkgs: not in a git work tree:", err)
		os.Exit(1)
	}
	repoRoot := string(root[:len(root)-1])

	diff := exec.Command("git", "diff", "--name-status", "-M", *base+"...HEAD")
	diff.Stderr = os.Stderr
	out, err := diff.Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "changedpkgs: git diff %s...HEAD failed: %v\n", *base, err)
		os.Exit(1)
	}
	hasGo := func(d string) bool { return land.PackageDirHasGo(repoRoot, d) }

	if *guards {
		printGuards(land.GuardSelects(string(out), hasGo))
		return
	}
	for _, dir := range land.ChangedPackages(string(out), hasGo) {
		fmt.Println("./" + dir)
	}
}

// printGuards writes one line of `go test` arguments, or nothing when the
// branch triggers no guard. The union of the test names is safe to expand
// unquoted: a Go test name holds no space and no shell metacharacter.
func printGuards(sel []land.GuardSelect) {
	if len(sel) == 0 {
		return
	}
	names := map[string]bool{}
	pkgs := make([]string, 0, len(sel))
	for _, s := range sel {
		pkgs = append(pkgs, "./"+s.Package)
		for _, name := range s.Tests {
			names[name] = true
		}
	}
	union := make([]string, 0, len(names))
	for name := range names {
		union = append(union, name)
	}
	sort.Strings(union)
	sort.Strings(pkgs)
	fmt.Printf("-run ^(%s)$ %s\n", strings.Join(union, "|"), strings.Join(pkgs, " "))
}
