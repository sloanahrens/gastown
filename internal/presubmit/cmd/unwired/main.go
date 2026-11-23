// Command unwired prints, without failing, each exported function the branch adds that no
// non-test file of the module names, so the "no in-tree caller" review finding arrives in the
// author's own presubmit (gt-46ezdn). It is advisory: the Makefile prints its warning and carries
// on, so a false positive costs a line of output rather than a landing. The Makefile hands it the
// branch's own diff against PRESUBMIT_BASE.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/presubmit"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr, gitOutput, os.ReadFile, nestedModuleIn(".")))
}

// run is main without the process: 0 whenever the branch was judged, whatever it found, 1 when the
// branch could not be read, 2 when the flags could not be read.
func run(args []string, w io.Writer, git presubmit.Runner, read presubmit.FileReader, nested func(string) bool) int {
	fs := flag.NewFlagSet("unwired", flag.ContinueOnError)
	fs.SetOutput(w)
	base := fs.String("base", "origin/main", "ref the branch is compared against")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *base == "" {
		fmt.Fprintln(w, "unwired: -base names the ref the branch is compared against")
		return 2
	}
	diff, err := presubmit.ReadDiff(git, *base)
	if err != nil {
		fmt.Fprintf(w, "unwired: cannot read the branch: %v\n", err)
		return 1
	}
	// The files are read from the working tree, which make presubmit runs on the branch checkout,
	// so the diff's HEAD paths and the files read agree while the tree is clean.
	res, err := presubmit.JudgeUnwired(bytes.NewReader(diff), moduleFiles(git), read, nested)
	if err != nil {
		fmt.Fprintf(w, "unwired: %v\n", err)
		return 1
	}
	// Both counts print on every run, so a branch that adds nothing to judge reads as that rather
	// than as a branch with no unwired function.
	fmt.Fprintf(w, "unwired: judged %d added exported function(s), %d with no caller\n", res.Judged, len(res.Findings))
	for _, f := range res.Findings {
		fmt.Fprintf(w, "%s:%d %s\n", f.Path, f.Line, f.Name)
	}
	return 0
}

// moduleFiles lists the Go files of the repository root, the tree the diff's paths are relative to.
func moduleFiles(git presubmit.Runner) presubmit.FileLister {
	return func() ([]string, error) {
		out, err := git([]string{"ls-files", "-z", "--full-name", "--", "*.go"})
		if err != nil {
			return nil, fmt.Errorf("git ls-files: %w", err)
		}
		var files []string
		for _, p := range strings.Split(string(out), "\x00") {
			if p != "" {
				files = append(files, p)
			}
		}
		return files, nil
	}
}

// nestedModuleIn reports whether a path lies under a directory of its own module (a go.mod below
// root): such a file belongs to that module, and its callers are not this module's.
func nestedModuleIn(root string) func(string) bool {
	return func(p string) bool {
		for dir := filepath.Dir(filepath.FromSlash(p)); dir != "." && dir != "/" && dir != ""; dir = filepath.Dir(dir) {
			if _, err := os.Stat(filepath.Join(root, dir, "go.mod")); err == nil {
				return true
			}
		}
		return false
	}
}

// gitOutput runs git in the working directory. Standard error reaches the terminal so git's own
// message is visible; only standard output is returned, never echoed into an error, since captured
// output can hold a secret.
func gitOutput(args []string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}
