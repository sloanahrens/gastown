package testutil

import (
	"slices"
	"testing"
)

func TestCleanGTEnv_PreservesDoltPort(t *testing.T) {
	t.Parallel()
	env := cleanGTEnv([]string{"GT_DOLT_PORT=13307", "GT_TOWN_ROOT=/some/town", "BD_ACTOR=polecat/test", "PATH=/usr/bin"})

	if !slices.Contains(env, "GT_DOLT_PORT=13307") {
		t.Error("CleanGTEnv stripped GT_DOLT_PORT — must preserve it")
	}
	if slices.Contains(env, "GT_TOWN_ROOT=/some/town") {
		t.Error("CleanGTEnv preserved GT_TOWN_ROOT — must strip it")
	}
	if slices.Contains(env, "BD_ACTOR=polecat/test") {
		t.Error("CleanGTEnv preserved BD_ACTOR — must strip it")
	}
	if !slices.Contains(env, "PATH=/usr/bin") {
		t.Error("CleanGTEnv stripped PATH")
	}
}

func TestCleanGTEnv_PreservesBeadsDoltPort(t *testing.T) {
	t.Parallel()
	env := cleanGTEnv([]string{"BEADS_DOLT_PORT=13307", "BD_DEBUG=1"})

	if !slices.Contains(env, "BEADS_DOLT_PORT=13307") {
		t.Error("CleanGTEnv stripped BEADS_DOLT_PORT — must preserve it")
	}
	if slices.Contains(env, "BD_DEBUG=1") {
		t.Error("CleanGTEnv preserved BD_DEBUG — must strip it")
	}
}

// The hermetic markers, the Dolt passthroughs and bd's telemetry switches
// reach subprocesses; other GT_*/BD_* context does not.
func TestCleanGTEnv_KeepsHarnessMarkers(t *testing.T) {
	t.Parallel()
	keep := []string{"GT_DOLT_HOST=h", "GT_TEST_EXTERNAL_DOLT=1", "GT_TEST_HERMETIC=1", "GT_TEST_FORBIDDEN_TOWN_ROOT=/live", "BD_DISABLE_METRICS=1", "BD_DISABLE_EVENT_FLUSH=1"}
	env := cleanGTEnv(append([]string{"GT_ROLE=x", "BD_ACTOR=y"}, keep...))
	if !slices.Equal(env, keep) {
		t.Errorf("CleanGTEnv = %q, want %q", env, keep)
	}
}

func TestCleanGTEnv_ExtraEnv(t *testing.T) {
	t.Parallel()
	env := cleanGTEnv(nil, "HOME=/tmp/test", "FOO=bar")
	if !slices.Equal(env, []string{"HOME=/tmp/test", "FOO=bar"}) {
		t.Errorf("CleanGTEnv extras = %q, want them appended", env)
	}
}

func TestNewBDCommand_InheritsEnv(t *testing.T) {
	t.Parallel()
	cmd := NewBDCommand("version")
	// cmd.Env should be nil (inherits process env)
	if cmd.Env != nil {
		t.Error("NewBDCommand should not set cmd.Env (nil inherits process env)")
	}
	if !slices.Equal(cmd.Args, []string{"bd", "version"}) {
		t.Errorf("Args = %q", cmd.Args)
	}
}

// The isolated commands carry CleanGTEnv of the process environment.
func TestNewIsolatedCommands_SetEnv(t *testing.T) {
	t.Parallel()
	for name, cmd := range map[string]interface {
		env() []string
		args() []string
	}{
		"bd": isolated{NewIsolatedBDCommand("version").Env, NewIsolatedBDCommand("version").Args},
		"gt": isolated{NewIsolatedGTCommand("version").Env, NewIsolatedGTCommand("version").Args},
	} {
		if !slices.Equal(cmd.env(), CleanGTEnv()) {
			t.Errorf("%s: Env is not CleanGTEnv()", name)
		}
		if !slices.Equal(cmd.args(), []string{name, "version"}) {
			t.Errorf("%s: Args = %q", name, cmd.args())
		}
	}
}

type isolated struct{ e, a []string }

func (i isolated) env() []string  { return i.e }
func (i isolated) args() []string { return i.a }
