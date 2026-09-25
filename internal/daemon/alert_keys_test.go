package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeGtRecordingArgs installs a fake `gt` that appends its arguments to a log
// file, one invocation per line, and returns the log path.
func fakeGtRecordingArgs(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for gt")
	}
	logPath := filepath.Join(t.TempDir(), "gt-calls.log")
	script := `#!/usr/bin/env bash
printf '%s\n' "$*" >> "` + logPath + `"
cat > /dev/null
exit 0
`
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gt: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func daemonShellingOutToGt(t *testing.T) *Daemon {
	t.Helper()
	return &Daemon{
		logger: discardLogger,
		config: &Config{TownRoot: t.TempDir()},
	}
}

func readGtCalls(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read gt call log: %v", err)
	}
	return string(data)
}

// TestEscalateAlert_SendsTheStableKey pins the contract the dedupe depends on:
// the daemon must hand gt escalate the same key every time a condition fires,
// or a persisting condition mints a new bead per patrol cycle (gt-vwry).
func TestEscalateAlert_SendsTheStableKey(t *testing.T) {
	logPath := fakeGtRecordingArgs(t)
	d := daemonShellingOutToGt(t)

	d.escalateAlert(alertKeyJSONLSpike, "jsonl_git_backup", "spike detected:\nhq 1952 -> 932")

	calls := readGtCalls(t, logPath)
	if !strings.Contains(calls, "--fingerprint "+alertKeyJSONLSpike) {
		t.Errorf("expected the stable alert key on the gt escalate call, got:\n%s", calls)
	}
	if !strings.Contains(calls, "escalate") {
		t.Errorf("expected a gt escalate call, got:\n%s", calls)
	}
}

// TestEscalate_DerivesKeyFromTitleForUnkeyedProducers covers the producers that
// hand over a message whose first line already names the condition: the key
// falls out of the title, so they dedupe without naming a class.
func TestEscalate_DerivesKeyFromTitleForUnkeyedProducers(t *testing.T) {
	logPath := fakeGtRecordingArgs(t)
	d := daemonShellingOutToGt(t)

	d.escalate("compactor_dog", "compact hq: 900 commits")

	calls := readGtCalls(t, logPath)
	want := "--fingerprint " + escalationTitle("compactor_dog", "compact hq: 900 commits")
	if !strings.Contains(calls, want) {
		t.Errorf("expected %q on the gt escalate call, got:\n%s", want, calls)
	}
}

// TestClearAlerts_OneInvocationCarriesEveryKey: a producer clears its whole
// owned key set on a healthy cycle, and that must cost one gt invocation, not
// one per key.
func TestClearAlerts_OneInvocationCarriesEveryKey(t *testing.T) {
	logPath := fakeGtRecordingArgs(t)
	d := daemonShellingOutToGt(t)

	d.clearAlerts("backup cycle clean", alertKeyJSONLSpike, alertKeyJSONLPush)

	calls := readGtCalls(t, logPath)
	lines := strings.Split(strings.TrimSpace(calls), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly one gt invocation, got %d:\n%s", len(lines), calls)
	}
	for _, key := range []string{alertKeyJSONLSpike, alertKeyJSONLPush} {
		if !strings.Contains(lines[0], "--fingerprint "+key) {
			t.Errorf("expected %q on the clear call, got:\n%s", key, lines[0])
		}
	}
	if !strings.HasPrefix(lines[0], "escalate clear") {
		t.Errorf("expected an `escalate clear` invocation, got:\n%s", lines[0])
	}
}

func TestClearAlerts_NoKeysIsANoOp(t *testing.T) {
	logPath := fakeGtRecordingArgs(t)
	d := daemonShellingOutToGt(t)

	d.clearAlerts("nothing to clear")

	if calls := readGtCalls(t, logPath); calls != "" {
		t.Errorf("expected no gt invocation when there are no keys, got:\n%s", calls)
	}
}

// TestAlertKeysAreDistinctAndNonEmpty pins the keys themselves. A key that
// collides with another condition's, or one that drifts, silently breaks one of
// the two halves of the lifecycle — dedupe would merge unrelated alerts, or
// auto-close would stop finding the alert it is meant to close.
func TestAlertKeysAreDistinctAndNonEmpty(t *testing.T) {
	keys := map[string]string{
		"alertKeyMainBranchTest": alertKeyMainBranchTest,
		"alertKeyJSONLInit":      alertKeyJSONLInit,
		"alertKeyJSONLNoDBs":     alertKeyJSONLNoDBs,
		"alertKeyJSONLScrub":     alertKeyJSONLScrub,
		"alertKeyJSONLSpike":     alertKeyJSONLSpike,
		"alertKeyJSONLPush":      alertKeyJSONLPush,
	}

	seen := make(map[string]string, len(keys))
	for name, key := range keys {
		if key == "" {
			t.Errorf("%s is empty", name)
			continue
		}
		if prev, ok := seen[key]; ok {
			t.Errorf("%s and %s share the key %q; their alerts would merge", prev, name, key)
			continue
		}
		seen[key] = name
	}
}
