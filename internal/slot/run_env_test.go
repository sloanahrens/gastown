package slot

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestAcquiredFormat pins the acquire line. Callers outside Go detect a
// successful acquire by that string — the rebuild plugin greps for it to tell
// a command that never ran (defer, retry next heartbeat) from one that ran and
// failed (record a failure, escalate) — so rewording it breaks that plugin
// silently, on every heartbeat, with this suite still green (gt-kox0).
func TestAcquiredFormat(t *testing.T) {
	t.Parallel()
	line := fmt.Sprintf(acquiredFormat, "gastown/rebuild-gt", 2*time.Second, 0, 2)
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
		gotEnv, gotCmd := SplitEnvPrefix(c.in)
		if strings.Join(gotEnv, " ") != strings.Join(c.wantEnv, " ") || strings.Join(gotCmd, " ") != strings.Join(c.wantCmd, " ") {
			t.Errorf("SplitEnvPrefix(%v) = (%v, %v), want (%v, %v)", c.in, gotEnv, gotCmd, c.wantEnv, c.wantCmd)
		}
	}
}

// envValues returns every value the slice carries for key, so a test sees a
// duplicate the way a reader walking the environment would.
func envValues(env []string, key string) []string {
	var values []string
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			values = append(values, v)
		}
	}
	return values
}

// TestRunEnv_AlreadySetKeyIsOverriddenNotDuplicated drives the branch
// gt-g7ym is about: an assignment for a key this process already carries.
//
// Asserting on the child's own environment would not catch it — os/exec dedups
// a Cmd.Env on the way to execve, so the child reads the operator's value from
// either construction. The duplicate is visible in the slice, which is also
// where it would matter to a reader that takes the first match of a repeated
// key rather than the last (gt-g7ym), so the assertions are on the slice.
func TestRunEnv_AlreadySetKeyIsOverriddenNotDuplicated(t *testing.T) {
	t.Parallel()
	environ := []string{"PATH=/ambient/bin", "GT_SLOT_RUN_PROBE=inherited", "GT_SLOT_RUN_KEEP=kept"}

	env := runEnv(environ, []string{"GT_SLOT_RUN_PROBE=bar"})
	if got := envValues(env, "GT_SLOT_RUN_PROBE"); len(got) != 1 || got[0] != "bar" {
		t.Errorf("child env carries GT_SLOT_RUN_PROBE %v, want exactly [bar]: a second entry leaves the override to the reader's rule (gt-g7ym)", got)
	}
	if got := envValues(env, "GT_SLOT_RUN_KEEP"); len(got) != 1 || got[0] != "kept" {
		t.Errorf("child env carries GT_SLOT_RUN_KEEP %v, want the inherited [kept]: the replacement is per assigned key, not a fresh environment", got)
	}

	// PATH is the key the program is resolved against, so a child searching an
	// ambient PATH would run a different binary than the one validated.
	env = runEnv(environ, []string{"PATH=/slot/bin"})
	if got := envValues(env, "PATH"); len(got) != 1 || got[0] != "/slot/bin" {
		t.Errorf("child env carries PATH %v, want exactly [/slot/bin]", got)
	}

	// A key assigned twice settles on the last assignment, as env(1) leaves it.
	env = runEnv(environ, []string{"GT_SLOT_RUN_PROBE=bar", "GT_SLOT_RUN_PROBE=baz"})
	if got := envValues(env, "GT_SLOT_RUN_PROBE"); len(got) != 1 || got[0] != "baz" {
		t.Errorf("child env carries GT_SLOT_RUN_PROBE %v, want exactly [baz]", got)
	}
}
