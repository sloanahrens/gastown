package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestAgentResumeDeaconPausedFileNotClobbered is the gt-aj4m regression:
// `gt agent resume deacon` with no agent-pause marker but a stale bead
// mirror (agent_state=paused) used to rewrite the mirror to idle
// unconditionally. For the deacon, a paused bead state is often the
// DISPLAY mirror of the separate `gt deacon pause` file
// (.runtime/deacon/paused.json, which gates its heartbeats): clearing the
// mirror made the deacon resume patrols while its pause file still held.
// A live pause file means the mirror is live state, not a stale race
// artifact — resume must leave it alone.
func TestAgentResumeDeaconPausedFileNotClobbered(t *testing.T) {
	town, argsFile := setupResumeFixture(t, true)
	out, _ := captureStdio(t, func() { _ = runAgentResume(bareResumeCmd(), []string{"deacon"}) })

	// No bd update may have run: the mirror must not be rewritten.
	if hasUpdateCall(t, argsFile) {
		t.Errorf("bd update recorded despite a live deacon pause file; the mirror would have been clobbered")
	}
	if !strings.Contains(out, "LIVE") || !strings.Contains(out, "left untouched") {
		t.Errorf("expected live-pause-file notice, got: %q", out)
	}
	// The bead state on disk is only the pause file — and it must survive.
	if _, err := os.Stat(filepath.Join(town, ".runtime/deacon/paused.json")); err != nil {
		t.Errorf("deacon pause file was removed by resume: %v", err)
	}
}

// TestAgentResumeStaleMirrorCleared verifies the original stale-mirror
// repair still works when no separate pause file is live: the mirror is
// a stale race artifact and resume clears it to idle.
func TestAgentResumeStaleMirrorCleared(t *testing.T) {
	_, argsFile := setupResumeFixture(t, false)
	out, _ := captureStdio(t, func() { _ = runAgentResume(bareResumeCmd(), []string{"deacon"}) })

	if !hasUpdateCall(t, argsFile) {
		t.Errorf("expected the stale mirror to be cleared via bd update, but no update was recorded")
	}
	if !strings.Contains(out, "cleared to idle") {
		t.Errorf("expected stale-mirror-clear notice, got: %q", out)
	}
}

// TestAgentResumeDeaconPausedFileNotPaused verifies a non-paused (false)
// deacon pause file does not block the stale-mirror repair: the file is
// not governing anything, so the mirror is still just a stale artifact.
func TestAgentResumeDeaconPausedFileNotPaused(t *testing.T) {
	town, argsFile := setupResumeFixture(t, false)
	writePauseFile(t, town, `{"paused": false, "reason": "cleared by an older resume", "paused_at": "2026-01-01T00:00:00Z", "paused_by": "human"}`)
	out, _ := captureStdio(t, func() { _ = runAgentResume(bareResumeCmd(), []string{"deacon"}) })

	if !hasUpdateCall(t, argsFile) {
		t.Errorf("expected the stale mirror to be cleared despite a non-paused pause file, but no update was recorded")
	}
	if !strings.Contains(out, "cleared to idle") {
		t.Errorf("expected stale-mirror-clear notice, got: %q", out)
	}
}

// bareResumeCmd builds a command object whose RunE is runAgentResume, so
// the handler can be driven without the full root command tree.
func bareResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:  "resume",
		Args: cobra.ExactArgs(1),
		RunE: runAgentResume,
	}
}

// setupResumeFixture builds a fake Gas Town root wired to a fake bd and a
// fake tmux, so runAgentResume's bead reads/writes and session checks are
// hermetic:
//
//   - the fake bd logs every invocation's argv (one per line) to argsFile,
//     serves `show hq-deacon --json` with a paused agent bead, and
//     accepts updates without touching Dolt. A fresh PATH means the
//     allow-stale support probe hits the same shim.
//   - the fake tmux answers every subcommand with "no server running",
//     which the wrapper maps to "session does not exist" — no signal is
//     sent, no error is returned.
//
// paused=true writes a live .runtime/deacon/paused.json (the `gt deacon
// pause` file) beside the paused bead mirror.
func setupResumeFixture(t *testing.T, paused bool) (town, argsFile string) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("PATH shims are POSIX-only")
	}

	town = t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatalf("create mayor marker dir: %v", err)
	}
	// town.json is required, not just nice: workspace.IsWorkspace accepts the
	// mayor/ directory alone, but beads.FindTownRoot — the only town-root
	// resolver the in-process beads client (beads.New(town).ForAgentBead())
	// has — requires mayor/town.json. Without it the beads client sees no
	// town root and the fake-bd subprocess never runs ("no beads database
	// found"), so a hermetic fixture must carry the full marker.
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write fixture town.json: %v", err)
	}
	// GT_TOWN_ROOT is accepted by workspace.FindFromCwdOrError (verified by
	// IsWorkspace) and, through the same convention, by beads'
	// FindTownRoot — the in-process beads client and the deacon pause-file
	// path both resolve the fixture rather than the live town this test
	// happens to run inside, with no live-town file to walk up to.
	t.Setenv("GT_TOWN_ROOT", town)
	// Belt for tmux.NewTmux's live-socket guard: the guard only fires when
	// the package socket is unset, and this test never initializes a
	// registry — a fake socket keeps the shim from being reached with a
	// live one even if ambient env changes.
	t.Setenv("GT_TOWN_SOCKET", "gt-test-agent-resume")

	// cwd walk-up: resolve the fixture, not the live town this test
	// happens to run inside.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	os.Chdir(os.TempDir())
	t.Cleanup(func() { _ = os.Chdir(wd) })

	// Cleanups run in REVERSE registration order. Register the binDir/argsFile
	// deletion BEFORE the town dir is used, so it runs LAST and the shim's
	// on-disk payload (bd-show-*.json lives in binDir) outlives the town
	// fixture. A later-registered cleanup would delete binDir while the test
	// is still running its show calls.
	binDir := t.TempDir()
	t.Cleanup(func() { _ = os.RemoveAll(binDir) })
	argsDir := t.TempDir()
	t.Cleanup(func() { _ = os.RemoveAll(argsDir) })
	argsFile = filepath.Join(argsDir, "bd-args.log")
	if paused {
		writePauseFile(t, town, `{"paused": true, "reason": "operator freeze", "paused_at": "2026-01-01T00:00:00Z", "paused_by": "human"}`)
	}
	// The capability probe (beads.BdSupportsAllowStaleWithEnv) execs the
	// same shim as `bd --allow-stale version` and caches per resolved
	// path. The shim must therefore dispatch on a substring of the WHOLE
	// argv (`"$*"`), not on `$1`, because the flag position varies: the
	// probe itself arrives as `--allow-stale version` ($1 is the flag), and
	// a later call may or may not carry the prepended flag — both the bare
	// `show hq-deacon --json` and the prefixed form were observed,
	// depending on whether the path cache was already warm when the call
	// went out. Matching on "' show hq-deacon'" — with a leading space, as
	// an earlier version of this fixture did — misses that bare form and
	// serves nothing, which silently skips the
	// stale-mirror path the test exists to cover; match without anchoring
	// on a leading space. A single --allow-stale arg satisfies the probe
	// (it reads argv[1] and exits 0 before touching anything). `show
	// hq-deacon --json` is served from a static paused agent bead — a
	// literal file avoids shell-escaping the multi-line description; the
	// shim's own heredoc would break on the newlines in the description.
	// t.TempDir() directories are removed when their t.Cleanups run (which
	// precedes the next test), so the payload must live in binDir —
	// outlives the test like the shim that reads it.
	showJSON := `[{"id":"hq-deacon","title":"Deacon","issue_type":"agent","status":"open","priority":1,"labels":["gt:agent"],"description":"Deacon\n\nrole_type: deacon\nrig: null\nagent_state: paused\nhook_bead: null\ncleanup_status: null\nactive_mr: null"}]`
	showFile := filepath.Join(binDir, "bd-show-hq-deacon.json")
	if err := os.WriteFile(showFile, []byte(showJSON), 0o644); err != nil {
		t.Fatalf("write fake show payload: %v", err)
	}
	bdScript := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + argsFile + "'\n" +
		"case \"$*\" in\n" +
		"  *'show hq-deacon'*) cat '" + showFile + "' && exit 0;;\n" +
		"  *'--allow-stale'*) echo fake-bd-version && exit 0;;\n" +
		"esac\n" +
		"cat > /dev/null\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(bdScript), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	tmuxScript := "#!/bin/sh\necho \"no server running on socket\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(tmuxScript), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return town, argsFile
}

// writePauseFile writes the deacon pause file with the given JSON body.
func writePauseFile(t *testing.T, town, body string) {
	t.Helper()
	dir := filepath.Join(town, ".runtime/deacon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create deacon pause dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "paused.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write deacon pause file: %v", err)
	}
}

// hasUpdateCall reports whether any recorded bd invocation mutated the
// hq-deacon bead (the stale-mirror rewrite).
func hasUpdateCall(t *testing.T, argsFile string) bool {
	t.Helper()
	data, err := os.ReadFile(argsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		t.Fatalf("read bd arg log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// The call may arrive as `update hq-deacon ...` or, when the
		// allow-stale probe succeeds, `--allow-stale update hq-deacon ...`:
		// match the substring, not argv[0].
		if strings.Contains(line, "update hq-deacon") {
			return true
		}
	}
	return false
}
