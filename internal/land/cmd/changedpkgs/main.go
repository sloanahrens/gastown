// Command changedpkgs prints the Go packages a branch changed, one "./dir"
// per line, for `make presubmit` (gt-ssyxd). It reads
// `git diff --name-status -M <base>...HEAD`, so a branch is judged by its own
// commits, never by what landed on the base after it branched.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"

	"github.com/steveyegge/gastown/internal/land"
)

func main() {
	base := flag.String("base", "origin/main", "ref the branch is compared against")
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

	for _, dir := range land.ChangedPackages(string(out), func(d string) bool { return land.PackageDirHasGo(repoRoot, d) }) {
		fmt.Println("./" + dir)
	}
}
