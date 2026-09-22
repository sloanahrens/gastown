package beads

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestReadyDispatchable_ExcludesMailServerSide is the gt-b9wq rework's
// regression test for the om-editorial finding: the Ready panel's mail
// exclusion must not depend on labels surviving into `bd ready --json`'s
// response. The stub bd here mirrors that exact gap — the issue it returns
// after filtering carries no "labels" field at all, the shape a label-only
// client-side check cannot see — and it only omits the mail-shaped issue
// when it observes the --exclude-label/--exclude-type args ReadyDispatchable
// is required to send, the same way a real bd applies the exclusion before
// it ever serializes a response.
func TestReadyDispatchable_ExcludesMailServerSide(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}

	ResetBdAllowStaleCacheForTest()
	t.Cleanup(ResetBdAllowStaleCacheForTest)

	stubDir := t.TempDir()
	logPath := filepath.Join(stubDir, "bd.log")
	stubScript := `#!/bin/sh
printf '%s\n' "$*" >> "$MOCK_BD_LOG"
case "$1" in
  --allow-stale) exit 0 ;;
esac
case "$*" in
  *"--exclude-label"*"gt:message"*"--exclude-type"*"message"*)
    printf '[{"id":"gt-real","title":"Fix the flaky slot test","status":"open","priority":2,"issue_type":"task"}]\n'
    ;;
  *)
    printf '[{"id":"gt-real","title":"Fix the flaky slot test","status":"open","priority":2,"issue_type":"task"},{"id":"hq-mail","title":"Re: something","status":"open","priority":2,"issue_type":"task"}]\n'
    ;;
esac
exit 0
`
	stubPath := filepath.Join(stubDir, "bd")
	if err := os.WriteFile(stubPath, []byte(stubScript), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_BD_LOG", logPath)

	b := NewIsolated(t.TempDir())
	issues, err := b.ReadyDispatchable()
	if err != nil {
		t.Fatalf("ReadyDispatchable: %v", err)
	}

	if len(issues) != 1 || issues[0].ID != "gt-real" {
		ids := make([]string, len(issues))
		for i, issue := range issues {
			ids[i] = issue.ID
		}
		t.Fatalf("ReadyDispatchable = %v, want only [gt-real]", ids)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read mock bd log: %v", err)
	}
	logOutput := string(logData)
	if !strings.Contains(logOutput, "--exclude-label") || !strings.Contains(logOutput, "gt:message") {
		t.Fatalf("ReadyDispatchable did not send --exclude-label with gt:message:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "--exclude-type") || !strings.Contains(logOutput, "message") {
		t.Fatalf("ReadyDispatchable did not send --exclude-type with message:\n%s", logOutput)
	}
}

// TestReadyDispatchable_FallsBackOnUnknownFlag guards an older bd that
// rejects --exclude-label/--exclude-type as unknown: ReadyDispatchable must
// still return the unfiltered Ready() result rather than error out, leaving
// the caller's own IsNonDispatchableBead pass as the only filter (same as
// before this method existed).
func TestReadyDispatchable_FallsBackOnUnknownFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}

	ResetBdAllowStaleCacheForTest()
	t.Cleanup(ResetBdAllowStaleCacheForTest)

	stubDir := t.TempDir()
	stubScript := `#!/bin/sh
case "$1" in
  --allow-stale) exit 0 ;;
esac
case "$*" in
  *"--exclude-label"*)
    echo "Error: unknown flag: --exclude-label" 1>&2
    exit 1
    ;;
  *)
    printf '[{"id":"gt-real","title":"Fix the flaky slot test","status":"open","priority":2,"issue_type":"task"}]\n'
    exit 0
    ;;
esac
`
	stubPath := filepath.Join(stubDir, "bd")
	if err := os.WriteFile(stubPath, []byte(stubScript), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	b := NewIsolated(t.TempDir())
	issues, err := b.ReadyDispatchable()
	if err != nil {
		t.Fatalf("ReadyDispatchable: %v", err)
	}
	if len(issues) != 1 || issues[0].ID != "gt-real" {
		t.Fatalf("ReadyDispatchable fallback = %v, want [gt-real]", issues)
	}
}
