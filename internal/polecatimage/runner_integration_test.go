//go:build integration

package polecatimage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// execRunHelperArg marks this test binary as the program the runner test
// starts; the value after the colon is how that program should end.
const execRunHelperArg = "polecat-image-exec-run-helper:"

// TestIntegrationExecRunMapsExitStatus runs the real runner over real
// processes, which the unit tier's seam never starts: a status of the program's
// own passes through, and a program that never ran or that a signal killed
// reports 127, so a probe that could not answer defers instead of reading as
// "nothing there".
func TestIntegrationExecRunMapsExitStatus(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("finding the test binary: %v", err)
	}
	helper := func(mode string) command {
		return command{
			name: self,
			args: []string{"-test.run=^TestIntegrationExecRunHelper$", execRunHelperArg + mode},
		}
	}
	cases := []struct {
		name   string
		cmd    command
		exit   int
		output bool
	}{
		{"success", helper("ok"), 0, true},
		{"a status of its own", helper("exit3"), 3, true},
		{"killed by a signal", helper("signal"), 127, true},
		{"a program that is not there", command{name: filepath.Join(t.TempDir(), "missing")}, 127, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := execRun(tc.cmd)
			if res.exit != tc.exit {
				t.Errorf("exit = %d, want %d", res.exit, tc.exit)
			}
			if got := strings.Contains(res.stdout, "helper stdout"); got != tc.output {
				t.Errorf("stdout = %q, want the helper's marker: %v", res.stdout, tc.output)
			}
			if got := strings.Contains(res.stderr, "helper stderr"); got != tc.output {
				t.Errorf("stderr = %q, want the helper's marker: %v", res.stderr, tc.output)
			}
		})
	}
}

// TestIntegrationExecRunHelper is the program TestIntegrationExecRunMapsExitStatus
// starts: this binary, told by its last argument how to end.
func TestIntegrationExecRunHelper(t *testing.T) {
	last := os.Args[len(os.Args)-1]
	mode, ok := strings.CutPrefix(last, execRunHelperArg)
	if !ok {
		t.Skip("only runs as the re-exec from TestIntegrationExecRunMapsExitStatus")
	}
	fmt.Println("helper stdout")
	fmt.Fprintln(os.Stderr, "helper stderr")
	switch mode {
	case "ok":
		os.Exit(0)
	case "exit3":
		os.Exit(3)
	case "signal":
		// SIGKILL cannot be caught or ignored, so the process always ends the
		// way the case needs it to; if the call fails, a status the runner
		// passes through still fails the test rather than hanging it.
		if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
			t.Fatalf("signalling the helper: %v", err)
		}
		os.Exit(99)
	}
	t.Fatalf("unknown helper mode %q", mode)
}
