package web

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// detailsBatchFetcher builds a fetcher whose bd is a shell script standing in
// for the real binary, so an error has to survive the actual subprocess path
// (gt-80o pinned these queries to the town root, which fetcherRunCmd cannot
// express). The script path is returned so a test can have the script record
// what it saw next to itself.
func detailsBatchFetcher(t *testing.T, script string) (*LiveConvoyFetcher, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}

	bdPath := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	f := &LiveConvoyFetcher{townRoot: t.TempDir(), cmdTimeout: 5 * time.Second, bdBin: bdPath}
	return f, bdPath
}

func TestGetIssueDetailsBatch_ReturnsStructuredErrorOnCommandFailure(t *testing.T) {
	f, _ := detailsBatchFetcher(t, `#!/bin/sh
echo "boom" >&2
exit 1
`)

	_, err := f.getIssueDetailsBatch([]string{"gt-1", "gt-2"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "bd show failed") {
		t.Fatalf("expected structured failure context, got: %v", err)
	}
	if !strings.Contains(err.Error(), "issue_count=2") {
		t.Fatalf("expected issue count in error, got: %v", err)
	}
}

func TestGetIssueDetailsBatch_ReturnsStructuredErrorOnInvalidJSON(t *testing.T) {
	f, _ := detailsBatchFetcher(t, `#!/bin/sh
printf '{invalid'
exit 0
`)

	_, err := f.getIssueDetailsBatch([]string{"gt-9"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("expected parse context, got: %v", err)
	}
	if !strings.Contains(err.Error(), "issue_count=1") {
		t.Fatalf("expected issue count in error, got: %v", err)
	}
}

func TestGetIssueDetailsBatch_ParsesIssueDetails(t *testing.T) {
	f, _ := detailsBatchFetcher(t, `#!/bin/sh
echo '[{"id":"gt-1","title":"One","status":"open","assignee":"rig/polecats/a","updated_at":"2026-02-01T12:00:00Z"},{"id":"gt-2","title":"Two","status":"closed","assignee":"","updated_at":"2026-02-01T12:01:00Z"}]'
`)

	details, err := f.getIssueDetailsBatch([]string{"gt-1", "gt-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(details) != 2 {
		t.Fatalf("expected 2 details, got %d", len(details))
	}
	if details["gt-1"] == nil || details["gt-1"].Title != "One" {
		t.Fatalf("unexpected parsed details for gt-1: %#v", details["gt-1"])
	}
	if details["gt-2"] == nil || details["gt-2"].Status != "closed" {
		t.Fatalf("unexpected parsed details for gt-2: %#v", details["gt-2"])
	}
}

// TestGetIssueDetailsBatch_RunsFromTownRoot pins the working directory bd is
// invoked in. bd resolves its database relative to cwd, so a dashboard started
// outside the town resolves nothing and every tracked issue renders as
// "(external)"/unknown — cross-rig IDs like om-59p are exactly the ones that
// need routes.jsonl from the town root (gt-44z1).
func TestGetIssueDetailsBatch_RunsFromTownRoot(t *testing.T) {
	f, bdPath := detailsBatchFetcher(t, `#!/bin/sh
printf '%s' "$PWD" > "$0.pwd"
echo '[]'
`)

	if _, err := f.getIssueDetailsBatch([]string{"om-59p"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(bdPath + ".pwd")
	if err != nil {
		t.Fatalf("read recorded cwd: %v", err)
	}
	// t.TempDir() is symlinked on macOS (/var vs /private/var), and the shell
	// reports $PWD unresolved, so compare resolved paths rather than strings.
	want, err := filepath.EvalSymlinks(f.townRoot)
	if err != nil {
		t.Fatalf("resolve town root: %v", err)
	}
	gotResolved, err := filepath.EvalSymlinks(strings.TrimSpace(string(got)))
	if err != nil {
		t.Fatalf("resolve recorded cwd %q: %v", got, err)
	}
	if gotResolved != want {
		t.Fatalf("bd show ran in %q, want the town root %q", gotResolved, want)
	}
}
