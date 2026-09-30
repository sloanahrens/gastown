//go:build integration

package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// wispTree and closeStaleWisp read and close through internal/beads, which
// runs bd itself, so their bodies are pinned here against a stub bd on PATH:
// the gt-da2x finding was that they ran without the daemon's routing env
// (gh#3596 connection churn), and the env a real bd sees is the assertion.

// writeFakeBdForHandler installs a mock `bd` in binDir that appends its argv
// and its bd read/write mode env to logPath before running body. Logging the
// mode is the point: gt-da2x's first finding was that the wisp reads and
// closes reintroduced gh#3596 connection churn by running without the daemon's
// routing env, so the tests assert on the mode rather than trusting it.
func writeFakeBdForHandler(t *testing.T, binDir, logPath, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd is a shell script; skipping on Windows")
	}
	script := "#!/bin/sh\n" +
		"printf '%s|BD_DOLT_AUTO_COMMIT=%s|BD_READONLY=%s\\n' \"$*\" \"$BD_DOLT_AUTO_COMMIT\" \"$BD_READONLY\" >> \"" + logPath + "\"\n" +
		body
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// fakeBdCallsForHandler returns the argv and bd mode of each recorded call.
func fakeBdCallsForHandler(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading fake bd log: %v", err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

const fakeWispTreeScriptBody = `if [ "$1" = "show" ]; then
  case "$2" in
    wisp-alpha)
      echo '{"wisp-alpha":[{"id":"wisp-alpha.1","status":"open"},{"id":"wisp-alpha.2","status":"open"}],"schema_version":1}'
      exit 0
      ;;
  esac
  echo '{"schema_version":1}'
  exit 0
fi
exit 0
`

// TestIntegrationWispTree_RealBodyReadsChildrenReadOnly covers wispTree's body: it must
// find ephemeral wisp steps (via `bd show --children`, which sees the
// wisp_dependencies table that `bd children` misses) and it must do so with
// BD_DOLT_AUTO_COMMIT=off. That read runs once per idle dog per dispatch tick;
// with auto-commit on it opens a connection attempting a no-op commit every
// time (gh#3596).
func TestIntegrationWispTree_RealBodyReadsChildrenReadOnly(t *testing.T) {
	townRoot := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeFakeBdForHandler(t, t.TempDir(), logPath, fakeWispTreeScriptBody)

	tree, err := wispTree(townRoot, "wisp-alpha")
	if err != nil {
		t.Fatalf("wispTree: %v", err)
	}
	want := []beads.WispStep{
		{ID: "wisp-alpha"},
		{ID: "wisp-alpha.1", Status: "open"},
		{ID: "wisp-alpha.2", Status: "open"},
	}
	if len(tree) != len(want) {
		t.Fatalf("wispTree() = %+v, want %+v", tree, want)
	}
	for i := range want {
		if tree[i] != want[i] {
			t.Fatalf("wispTree()[%d] = %+v, want %+v", i, tree[i], want[i])
		}
	}

	calls := fakeBdCallsForHandler(t, logPath)
	if len(calls) == 0 {
		t.Fatal("fake bd was never invoked")
	}
	for _, call := range calls {
		if !strings.HasPrefix(call, "show wisp-alpha") || !strings.Contains(call, "--children --json") {
			t.Errorf("call = %q, want a `bd show <id> --children --json` read", call)
		}
		if !strings.Contains(call, "BD_DOLT_AUTO_COMMIT=off") || !strings.Contains(call, "BD_READONLY=true") {
			t.Errorf("call = %q, want the daemon's read-only routing env (BD_DOLT_AUTO_COMMIT=off, BD_READONLY=true)", call)
		}
	}
}

// TestIntegrationCloseStaleWisp_RealBodyClosesDeepestFirstWithMutationEnv covers
// closeStaleWisp's body: children are closed before the root (a parent closed
// while its child survives strands the child — gt-7lx3), already-closed steps
// are skipped, and the write runs with auto-commit on so the closes are not
// stranded in a daemon subprocess context.
func TestIntegrationCloseStaleWisp_RealBodyClosesDeepestFirstWithMutationEnv(t *testing.T) {
	townRoot := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeFakeBdForHandler(t, t.TempDir(), logPath, "if [ \"$1\" = \"close\" ]; then\n  exit 0\nfi\nexit 0\n")

	tree := []beads.WispStep{
		{ID: "wisp-alpha"},
		{ID: "wisp-alpha.1", Status: "open"},
		{ID: "wisp-alpha.2", Status: "closed"},
		{ID: "wisp-alpha.1.1", Status: "open"},
	}
	closed, err := closeStaleWisp(townRoot, "wisp-alpha", tree)
	if err != nil {
		t.Fatalf("closeStaleWisp: %v", err)
	}
	if closed != 3 {
		t.Errorf("closeStaleWisp() closed = %d, want 3 (the root and the two open steps)", closed)
	}

	calls := fakeBdCallsForHandler(t, logPath)
	want := "close wisp-alpha.1.1 wisp-alpha.1 wisp-alpha --force --reason " + wispAbandonedReason +
		"|BD_DOLT_AUTO_COMMIT=on|BD_READONLY="
	if len(calls) != 1 || calls[0] != want {
		t.Errorf("fake bd calls = %q, want exactly [%q]", calls, want)
	}
}

// TestIntegrationCloseStaleWisp_RealBodySurfacesCloseFailure pins the return
// path the handler logs on: a failed bd close must not be reported as a
// successful reap.
func TestIntegrationCloseStaleWisp_RealBodySurfacesCloseFailure(t *testing.T) {
	townRoot := t.TempDir()
	writeFakeBdForHandler(t, t.TempDir(), filepath.Join(t.TempDir(), "bd.log"), "echo 'dolt unreachable' >&2\nexit 1\n")

	closed, err := closeStaleWisp(townRoot, "wisp-alpha", []beads.WispStep{{ID: "wisp-alpha"}})
	if err == nil {
		t.Fatal("closeStaleWisp() error = nil, want an error when bd close fails")
	}
	if closed != 0 {
		t.Errorf("closeStaleWisp() closed = %d, want 0 on failure", closed)
	}
	if !strings.Contains(err.Error(), "dolt unreachable") {
		t.Errorf("closeStaleWisp() error = %v, want it to carry bd's stderr", err)
	}
}
