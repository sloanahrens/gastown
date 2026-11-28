// Command redbase runs the red-on-base check of the landing gate (gt-qzox4o). The gate runs it in
// the checkout of the branch being landed: it judges the tests the branch adds or changes against
// the merge base, and exits with the code presubmit.RedBaseExit returns. All decisions live in
// internal/presubmit; this file only wires the real operations into it.
package main

import (
	"io"
	"os"
	"os/exec"

	"github.com/steveyegge/gastown/internal/presubmit"
)

func main() {
	adapters := presubmit.RedBaseAdapters{
		Git:    gitOutput,
		Read:   os.ReadFile,
		Mkdir:  func(dir string) error { return os.MkdirAll(dir, 0o750) },
		Write:  func(path string, data []byte) error { return os.WriteFile(path, data, 0o600) },
		GoTest: goOutput,
	}
	check := func(base, dir string, out io.Writer) (bool, error) {
		return presubmit.RedBaseRunCheck(adapters, base, dir, out)
	}
	os.Exit(presubmit.RedBaseExit(os.Args[1:], os.Stdout, os.Stderr, os.MkdirTemp, os.RemoveAll, check))
}

// gitOutput runs git in the working directory. Standard error reaches the terminal; only standard
// output is returned, never echoed into an error, since captured output can hold a secret.
func gitOutput(args []string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

// goOutput runs the go command in dir. Standard error reaches the terminal, and its output and error
// go through presubmit.RedBaseGoResult, which tells a run that failed its tests from one that did
// not run.
func goOutput(dir string, args []string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	return presubmit.RedBaseGoResult(out, err)
}
