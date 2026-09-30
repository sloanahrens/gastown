package doltserver

import (
	"strings"
	"testing"
)

// TestDefaultConfig_WaitTimeoutDefault verifies that the default config
// applies the gh-3623 idle-session timeout.
func TestDefaultConfig_WaitTimeoutDefault(t *testing.T) {
	t.Parallel()
	config := newFakeHost().setenv("GT_DOLT_WAIT_TIMEOUT", "").host().DefaultConfig(t.TempDir())

	if config.WaitTimeoutSec != DefaultWaitTimeoutSec {
		t.Errorf("WaitTimeoutSec = %d, want %d", config.WaitTimeoutSec, DefaultWaitTimeoutSec)
	}
}

// TestDefaultConfig_WaitTimeoutEnvOverride verifies the GT_DOLT_WAIT_TIMEOUT
// env var raises or lowers the configured timeout.
func TestDefaultConfig_WaitTimeoutEnvOverride(t *testing.T) {
	t.Parallel()
	config := newFakeHost().setenv("GT_DOLT_WAIT_TIMEOUT", "120").host().DefaultConfig(t.TempDir())

	if config.WaitTimeoutSec != 120 {
		t.Errorf("WaitTimeoutSec = %d, want 120", config.WaitTimeoutSec)
	}
}

// TestDefaultConfig_WaitTimeoutNegativeDisables verifies that a negative
// value opts out of the override, leaving Dolt's default in place.
func TestDefaultConfig_WaitTimeoutNegativeDisables(t *testing.T) {
	t.Parallel()
	config := newFakeHost().setenv("GT_DOLT_WAIT_TIMEOUT", "-1").host().DefaultConfig(t.TempDir())

	if config.WaitTimeoutSec != 0 {
		t.Errorf("WaitTimeoutSec = %d, want 0 (disabled)", config.WaitTimeoutSec)
	}
}

// TestDefaultConfig_WaitTimeoutInvalidIgnored verifies that a non-numeric
// env value falls back to the default rather than zeroing the timeout.
func TestDefaultConfig_WaitTimeoutInvalidIgnored(t *testing.T) {
	t.Parallel()
	config := newFakeHost().setenv("GT_DOLT_WAIT_TIMEOUT", "not-a-number").host().DefaultConfig(t.TempDir())

	if config.WaitTimeoutSec != DefaultWaitTimeoutSec {
		t.Errorf("WaitTimeoutSec = %d, want default %d when env var is invalid", config.WaitTimeoutSec, DefaultWaitTimeoutSec)
	}
}

// TestBuildWaitTimeoutQuery verifies the exact SQL emitted by applyWaitTimeout.
func TestBuildWaitTimeoutQuery(t *testing.T) {
	t.Parallel()
	got := buildWaitTimeoutQuery(30)
	want := "SET GLOBAL wait_timeout = 30"
	if got != want {
		t.Errorf("buildWaitTimeoutQuery(30) = %q, want %q", got, want)
	}
}

// TestApplyWaitTimeout_DisabledShortCircuits verifies that the override is opted out of: no SET
// GLOBAL reaches dolt.
func TestApplyWaitTimeout_DisabledShortCircuits(t *testing.T) {
	t.Parallel()
	f := newFakeHost()
	for _, v := range []int{0, -1, -3600} {
		f.host().applyWaitTimeout(t.TempDir(), &Config{WaitTimeoutSec: v})
	}
	if ran := f.ranMatching("SET GLOBAL"); len(ran) != 0 {
		t.Errorf("SET GLOBAL sent for an opted-out value: %q", ran)
	}
}

// TestApplyWaitTimeout_DispatchesQuery verifies that a set value sends the expected
// SET GLOBAL statement to the town's running server.
func TestApplyWaitTimeout_DispatchesQuery(t *testing.T) {
	t.Parallel()
	f := newFakeHost().townPort(4520).on("dolt *", fakeReply{})
	f.host().applyWaitTimeout(t.TempDir(), &Config{WaitTimeoutSec: 45})

	ran := f.ranMatching("SET GLOBAL")
	if len(ran) != 1 || !strings.HasSuffix(ran[0], "sql -q SET GLOBAL wait_timeout = 45") || !strings.Contains(ran[0], "--port 4520") {
		t.Errorf("dolt calls = %q, want one %q against port 4520", ran, "SET GLOBAL wait_timeout = 45")
	}
}

// TestApplyWaitTimeout_ErrorIsBestEffort verifies that a SQL failure does not panic or
// propagate: the server is already up, and failing here would fail the start.
func TestApplyWaitTimeout_ErrorIsBestEffort(t *testing.T) {
	t.Parallel()
	f := newFakeHost().on("dolt *", fakeReply{stderr: "simulated SQL failure", code: 1})
	f.host().applyWaitTimeout(t.TempDir(), &Config{WaitTimeoutSec: 45})
	if len(f.ranMatching("SET GLOBAL")) != 1 {
		t.Errorf("the statement was not attempted: %q", f.commands())
	}
}
