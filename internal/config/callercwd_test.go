package config

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/callercwd"
)

// TestCallerCwdDoesNotSteer pins that agent resolution's host reads the real
// process directory: with GT_CALLER_CWD naming another directory, processHost
// still answers this process's directory, so the variable cannot redirect a
// spawn's config and prompt lookup.
//
//testpolicy:allow parallel — GT_CALLER_CWD is the input under test, which t.Setenv shares with the whole process
func TestCallerCwdDoesNotSteer(t *testing.T) {
	processDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("process directory: %v", err)
	}
	if processDir == "" {
		t.Fatalf("the process directory is empty; the assertion would be vacuous")
	}
	callerPath := t.TempDir()
	if callerPath == processDir {
		t.Fatalf("the caller's directory %q equals the process directory; the assertion would be vacuous", callerPath)
	}
	//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
	t.Setenv(callercwd.EnvCallerCwd, callerPath)

	got, err := processHost.getwd()
	if err != nil || got != processDir {
		t.Fatalf("agent resolution directory = %q, %v; want the process directory %q", got, err, processDir)
	}
}
