//go:build integration

package style

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// printWarningChildEnv marks the re-executed test binary that calls
// PrintWarning against the real process streams.
const printWarningChildEnv = "GT_STYLE_PRINTWARNING_CHILD"

// TestIntegrationPrintWarningDoesNotWriteStdout guards against warnings
// contaminating JSON on stdout (sheriff PR #4568 / gt-pr-sheriff-4547): it
// re-executes this test binary, has the child call PrintWarning on the real
// os.Stdout and os.Stderr, and checks which stream the warning reached.
func TestIntegrationPrintWarningDoesNotWriteStdout(t *testing.T) {
	if os.Getenv(printWarningChildEnv) == "1" {
		PrintWarning("should go to stderr only")
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestIntegrationPrintWarningDoesNotWriteStdout$", "-test.count=1")
	cmd.Env = append(os.Environ(), printWarningChildEnv+"=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("child test binary failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}

	// The child's stdout carries only the testing framework's own verdict.
	if out := strings.TrimSpace(stdout.String()); out != "PASS" {
		t.Errorf("child stdout = %q, want only the test framework's PASS: PrintWarning must not write to stdout", out)
	}
	if !strings.Contains(stderr.String(), "should go to stderr only") {
		t.Errorf("child stderr = %q, want the warning", stderr.String())
	}
}
