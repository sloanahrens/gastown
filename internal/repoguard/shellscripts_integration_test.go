//go:build integration

package repoguard

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestIntegrationNoNewShellScripts holds every tracked .sh file to
// shell-scripts.txt. The listing runs real git, so the test lives in the
// integration tier: the unit tier runs no git (internal/testpolicy/realgit.txt,
// docs/testing.md "Seams for external tools"). The allowlist is the single
// source of truth, compared in both directions, so a new script has no line
// and a line whose script is gone fails.
func TestIntegrationNoNewShellScripts(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	allowed, err := ReadAllowlist("shell-scripts.txt")
	if err != nil {
		t.Fatal(err)
	}
	findings, err := CheckShellScripts(func() ([]string, error) { return trackedShellScripts(root) }, allowed)
	if err != nil {
		t.Fatalf("listing the tracked .sh files: %v", err)
	}
	for _, f := range findings {
		t.Error(f)
	}
}

// TestIntegrationAllowlistRejectsRealTree is the end-to-end control for the
// clean run above: the same real listing and the same checked-in allowlist
// text, with one line mutated at a time. A guard that passed the clean tree
// but never failed a broken line would prove nothing, so each shape a bad line
// takes must fail the guard here, over the tree itself (gt-tpg0p.15).
func TestIntegrationAllowlistRejectsRealTree(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	text, err := os.ReadFile(filepath.Join(root, "internal", "repoguard", "shell-scripts.txt"))
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := trackedShellScripts(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracked) == 0 {
		t.Fatal("the tree lists no .sh files, so the control has nothing to mutate")
	}
	target := tracked[0]
	list := func() ([]string, error) { return tracked, nil }

	clean, err := ParseAllowlist(bytes.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if findings, err := CheckShellScripts(list, clean); err != nil {
		t.Fatal(err)
	} else if len(findings) > 0 {
		t.Fatalf("the checked-in allowlist does not hold the tree: %q", findings)
	}

	tests := []struct {
		name  string
		path  string
		line  string
		alarm string
	}{
		{
			name:  "a line that lost its reason",
			path:  target,
			line:  target + "\t",
			alarm: allowlistNoReasonAlarm,
		},
		{
			name:  "a line deleted for a tracked script",
			path:  target,
			line:  "",
			alarm: newScriptAlarm,
		},
		{
			name:  "a platform reason on a path it does not name",
			path:  "docker-entrypoint.sh",
			line:  "docker-entrypoint.sh\tplatform: git hook",
			alarm: fmt.Sprintf(badFitsAlarm, "platform: git hook"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			allowed, err := ParseAllowlist(bytes.NewReader(replaceLine(t, text, tc.path, tc.line)))
			if err != nil {
				t.Fatal(err)
			}
			findings, err := CheckShellScripts(list, allowed)
			if err != nil {
				t.Fatal(err)
			}
			if !namesLine(findings, tc.path, tc.alarm) {
				t.Errorf("findings = %q, want one naming %s with %q", findings, tc.path, tc.alarm)
			}
		})
	}

	// A line whose script git does not track is stale, and the stale check is
	// the other direction of the comparison the guard makes.
	extra := append(append([]byte(nil), text...), []byte("scripts/guard-control-gone.sh\tport: gt-tpg0p\n")...)
	allowed, err := ParseAllowlist(bytes.NewReader(extra))
	if err != nil {
		t.Fatal(err)
	}
	findings, err := CheckShellScripts(list, allowed)
	if err != nil {
		t.Fatal(err)
	}
	if !namesLine(findings, "scripts/guard-control-gone.sh", removedScriptAlarm) {
		t.Errorf("findings = %q, want the stale line for scripts/guard-control-gone.sh", findings)
	}
}

// replaceLine returns the allowlist text with the line naming path rewritten to
// line, or removed when line is empty. A control that mutated nothing would
// pass as a guard, so a missing line is fatal.
func replaceLine(t *testing.T, text []byte, path, line string) []byte {
	t.Helper()
	lines := strings.Split(string(text), "\n")
	for i, have := range lines {
		if !strings.HasPrefix(have, path+"\t") {
			continue
		}
		if line == "" {
			return []byte(strings.Join(append(lines[:i], lines[i+1:]...), "\n"))
		}
		lines[i] = line
		return []byte(strings.Join(lines, "\n"))
	}
	t.Fatalf("no allowlist line names %s", path)
	return nil
}

// namesLine reports whether findings holds one that names path and holds alarm.
func namesLine(findings []string, path, alarm string) bool {
	for _, f := range findings {
		if strings.Contains(f, path) && strings.Contains(f, alarm) {
			return true
		}
	}
	return false
}

// TestIntegrationTrackedShellScriptsFailsOutsideGit: a listing that cannot see
// the tree is an error, never an empty tree, so the guard cannot pass by
// failing to read git.
func TestIntegrationTrackedShellScriptsFailsOutsideGit(t *testing.T) {
	t.Parallel()
	if got, err := trackedShellScripts(t.TempDir()); err == nil {
		t.Fatalf("trackedShellScripts(non-repo) = %q, want an error", got)
	}
}

// trackedShellScripts is the real lister: every .sh file git tracks under
// root, sorted. It lives in this integration-tagged file because the unit tier
// runs no git; the guard itself takes a Lister so its unit tests inject a fake.
func trackedShellScripts(root string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z", "--", "*.sh").Output() //nolint:gosec // G204: fixed argv; root is the repository under test
	if err != nil {
		return nil, fmt.Errorf("git ls-files '*.sh' in %s: %w", root, err)
	}
	var files []string
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			files = append(files, string(f))
		}
	}
	sort.Strings(files)
	return files, nil
}

// repoRoot is the gastown checkout this package sits in. The test reads the
// checked-in allowlist and the tree's tracked scripts, both relative to it.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}
