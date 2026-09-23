package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestSlotAcquiredFormat pins the acquire line. Callers outside Go detect a
// successful acquire by that string — the rebuild plugin greps for it to tell
// a command that never ran (defer, retry next heartbeat) from one that ran and
// failed (record a failure, escalate) — so rewording it breaks that plugin
// silently, on every heartbeat, with this suite still green (gt-kox0).
func TestSlotAcquiredFormat(t *testing.T) {
	t.Parallel()
	line := fmt.Sprintf(slotAcquiredFormat, "gastown/rebuild-gt", 2*time.Second, 0, 2)
	if !strings.HasPrefix(line, "Container-gate slot acquired ") {
		t.Fatalf("the acquire line no longer starts with the marker script plugins grep for: %q", line)
	}
	if !strings.Contains(line, "role=gastown/rebuild-gt") {
		t.Errorf("the acquire line lost the role: %q", line)
	}
}

// TestSplitEnvPrefix covers gt-18nx: gt slot run must treat leading VAR=value
// tokens as environment (env(1) semantics) so the polecat formula's verbatim
// wrap of an env-prefixed test_command works.
func TestSplitEnvPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      []string
		wantEnv []string
		wantCmd []string
	}{
		{[]string{"make", "test"}, nil, []string{"make", "test"}},
		{[]string{"GOFLAGS=-p=8", "make", "test"}, []string{"GOFLAGS=-p=8"}, []string{"make", "test"}},
		{[]string{"A=1", "B=", "go", "test", "./..."}, []string{"A=1", "B="}, []string{"go", "test", "./..."}},
		{[]string{"go", "test", "-run=X"}, nil, []string{"go", "test", "-run=X"}},
		{[]string{"--flag=value", "cmd"}, nil, []string{"--flag=value", "cmd"}},
		{[]string{"1BAD=x", "cmd"}, nil, []string{"1BAD=x", "cmd"}},
		{[]string{"ONLY=env"}, []string{"ONLY=env"}, nil},
	}
	for _, c := range cases {
		gotEnv, gotCmd := splitEnvPrefix(c.in)
		if strings.Join(gotEnv, " ") != strings.Join(c.wantEnv, " ") || strings.Join(gotCmd, " ") != strings.Join(c.wantCmd, " ") {
			t.Errorf("splitEnvPrefix(%v) = (%v, %v), want (%v, %v)", c.in, gotEnv, gotCmd, c.wantEnv, c.wantCmd)
		}
	}
}

// TestSplitEnvPrefix_ChildSeesVariable proves the split is enough for the
// child to observe the assignment when applied the way runSlotRun applies it.
func TestSplitEnvPrefix_ChildSeesVariable(t *testing.T) {
	t.Parallel()
	envAssigns, cmdArgs := splitEnvPrefix([]string{"GT_SLOT_RUN_PROBE=bar", "sh", "-c", "printf %s \"$GT_SLOT_RUN_PROBE\""})
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...) //nolint:gosec // G204: fixed test args
	cmd.Env = append(os.Environ(), envAssigns...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	if string(out) != "bar" {
		t.Fatalf("child saw %q, want %q", out, "bar")
	}
}
