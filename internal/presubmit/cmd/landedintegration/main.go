// Command landedintegration runs the changed integration tests of a landing (gt-w0lu62). Run as
// "landedintegration -base HEAD^1" in the checkout of the landed change, it asks internal/presubmit
// which integration tests base...HEAD changed, runs them, and exits with the code
// presubmit.RunLandedIntegration returns. All decisions live in internal/presubmit; this file only
// wires the real git, file and go operations into it.
package main

import (
	"os"
	"os/exec"

	"github.com/steveyegge/gastown/internal/presubmit"
)

func main() {
	list := func(base string) ([]presubmit.RedBasePackage, []string, error) {
		return presubmit.ChangedIntegrationTests(gitOutput, os.ReadFile, base)
	}
	os.Exit(presubmit.RunLandedIntegration(os.Args[1:], os.Stdout, os.Stderr, list, goStream))
}

// gitOutput runs git in the working directory. Standard error reaches the terminal; only standard
// output is returned, never echoed into an error, since captured output can hold a secret.
func gitOutput(args []string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

// goStream runs the go command in the working directory. Its standard output and standard error
// pass through to this process's as they are written, and its error is returned for the caller.
func goStream(args []string) error {
	cmd := exec.Command("go", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
