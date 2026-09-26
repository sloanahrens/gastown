package beads

import (
	"errors"
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

// TestReady_ExcludesBookkeepingServerSide is the gt-0q80 regression test:
// Ready() must send the same --exclude-label/--exclude-type flags
// ReadyDispatchable sends, so the in-process store path and the CLI path
// answer the same question. The stub mirrors the gt-b9wq gap — it only
// omits the mail-shaped issue when it observes the exclusion flags.
func TestReady_ExcludesBookkeepingServerSide(t *testing.T) {
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
	issues, err := b.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}

	if len(issues) != 1 || issues[0].ID != "gt-real" {
		ids := make([]string, len(issues))
		for i, issue := range issues {
			ids[i] = issue.ID
		}
		t.Fatalf("Ready = %v, want only [gt-real]", ids)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read mock bd log: %v", err)
	}
	logOutput := string(logData)
	if !strings.Contains(logOutput, "--exclude-label") || !strings.Contains(logOutput, "gt:message") {
		t.Fatalf("Ready did not send --exclude-label with gt:message:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "--exclude-type") || !strings.Contains(logOutput, "message") {
		t.Fatalf("Ready did not send --exclude-type with message:\n%s", logOutput)
	}
}

// readyPageID returns the ID of the first ready issue, or "" for a nil slice.
func readyPageID(issues []*Issue) string {
	if len(issues) == 0 {
		return ""
	}
	return issues[0].ID
}

// TestReady_CappedEnvelopeReturnsSentinel is the gt-m7pq regression test for
// the CLI path: when bd reports its ready page capped (the JSON envelope's
// pagination carries truncated=true), Ready returns the page AND an
// ErrReadyTruncated sentinel — the same "full page is a loud answer" contract
// the in-process store path got from the one-shot probe.
func TestReady_CappedEnvelopeReturnsSentinel(t *testing.T) {
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
printf '{"schema_version":1,"data":[{"id":"gt-page-0","title":"one","status":"open","priority":2,"issue_type":"task"}],"pagination":{"returned":1,"total":373,"truncated":true}}\n'
exit 0
`
	stubPath := filepath.Join(stubDir, "bd")
	if err := os.WriteFile(stubPath, []byte(stubScript), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	b := NewIsolated(t.TempDir())
	issues, err := b.Ready()
	if err == nil {
		t.Fatal("expected ErrReadyTruncated (bd reported the page capped)")
	}
	var capped *ErrReadyTruncated
	if !errors.As(err, &capped) {
		t.Fatalf("expected ErrReadyTruncated, got %v", err)
	}

	if len(issues) != 1 || readyPageID(issues) != "gt-page-0" {
		t.Fatalf("Ready returned %v, want the capped page (gt-page-0)", readyPageID(issues))
	}
	if capped.Found != 1 || capped.TrueCount != 373 {
		t.Errorf("capped = {Found: %d, TrueCount: %d}, want {1, 373}", capped.Found, capped.TrueCount)
	}
}

// TestReady_ReadyDispatchableCappedEnvelopeReturnsSentinel pins the same
// contract on ReadyDispatchable's CLI path — the method the Ready panel and
// the dispatch patrol use — so a capped page is flagged there too, not just
// on Ready.
func TestReady_ReadyDispatchableCappedEnvelopeReturnsSentinel(t *testing.T) {
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
printf '{"schema_version":1,"data":[{"id":"gt-real","title":"one","status":"open","priority":2,"issue_type":"task"}],"pagination":{"returned":1,"total":373,"truncated":true}}\n'
exit 0
`
	stubPath := filepath.Join(stubDir, "bd")
	if err := os.WriteFile(stubPath, []byte(stubScript), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	b := NewIsolated(t.TempDir())
	issues, err := b.ReadyDispatchable()
	if err == nil {
		t.Fatal("expected ErrReadyTruncated (bd reported the page capped)")
	}
	var capped *ErrReadyTruncated
	if !errors.As(err, &capped) {
		t.Fatalf("expected ErrReadyTruncated, got %v", err)
	}
	if len(issues) != 1 || readyPageID(issues) != "gt-real" {
		t.Fatalf("ReadyDispatchable returned %v, want the capped page (gt-real)", readyPageID(issues))
	}
	if capped.Found != 1 || capped.TrueCount != 373 {
		t.Errorf("capped = {Found: %d, TrueCount: %d}, want {1, 373}", capped.Found, capped.TrueCount)
	}
}

// TestReady_EnvelopeWithoutPaginationIsNotCapped pins the envelope's other
// branch: bd emits the same {"schema_version":1,"data":...} shape for a ready
// page that was NOT capped (no pagination key, or truncated=false) — that is
// the whole board, not a page, so Ready must return it without a sentinel.
func TestReady_EnvelopeWithoutPaginationIsNotCapped(t *testing.T) {
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
printf '{"schema_version":1,"data":[{"id":"gt-entire-board","title":"one","status":"open","priority":2,"issue_type":"task"}]}\n'
exit 0
`
	stubPath := filepath.Join(stubDir, "bd")
	if err := os.WriteFile(stubPath, []byte(stubScript), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	b := NewIsolated(t.TempDir())
	issues, err := b.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	var capped *ErrReadyTruncated
	if errors.As(err, &capped) {
		t.Fatalf("unexpected ErrReadyTruncated for an uncapped envelope: %v", err)
	}
	if len(issues) != 1 || readyPageID(issues) != "gt-entire-board" {
		t.Fatalf("Ready returned %v, want [gt-entire-board]", readyPageID(issues))
	}
}

// TestReady_PlainArrayOutputIsNotCapped pins graceful degradation against an
// old bd that ignores BD_JSON_ENVELOPE and still prints a bare JSON array:
// that shape carries no pagination, so Ready cannot know the page was capped
// and reports no sentinel — today's (silent) behavior, not worse.
func TestReady_PlainArrayOutputIsNotCapped(t *testing.T) {
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
printf '[{"id":"gt-legacy","title":"one","status":"open","priority":2,"issue_type":"task"}]\n'
exit 0
`
	stubPath := filepath.Join(stubDir, "bd")
	if err := os.WriteFile(stubPath, []byte(stubScript), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	b := NewIsolated(t.TempDir())
	issues, err := b.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	var capped *ErrReadyTruncated
	if errors.As(err, &capped) {
		t.Fatalf("unexpected ErrReadyTruncated for legacy plain-array output: %v", err)
	}
	if len(issues) != 1 || readyPageID(issues) != "gt-legacy" {
		t.Fatalf("Ready returned %v, want [gt-legacy]", readyPageID(issues))
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
