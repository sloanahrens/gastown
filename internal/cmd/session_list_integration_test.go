//go:build integration

package cmd

// gt session list through the production session managers, against a tmux shim
// on PATH answering list-sessions with recorded session names.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

// A crew session carries the rig prefix, so listing it as polecat crew-sloan
// pointed `gt session stop` at a seat that does not exist. This holds the real
// polecat.SessionManager.ListPolecats filter to it, which the stub-backed unit
// test cannot.
func TestIntegrationSessionListSkipsCrewAndRigRoleSessions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux shim is POSIX-only; tmux itself does not run on Windows")
	}

	prefix := townRegistry().PrefixForRig("gastown")
	sessions := []string{
		prefix + "-amber",
		prefix + "-crew-sloan",
		prefix + "-witness",
		prefix + "-refinery",
		"other-onyx", // another rig's prefix
	}
	binDir := t.TempDir()
	script := "#!/bin/sh\ncat <<'EOF'\n" + strings.Join(sessions, "\n") + "\nEOF\n"
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tm := tmux.NewTmuxWithSocket("gt-test-session-list")
	got := listPolecatSessions([]*rig.Rig{{Name: "gastown"}}, polecatSessionManagers(tm))

	want := []SessionListItem{{Rig: "gastown", Polecat: "amber", SessionID: prefix + "-amber", Running: true}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("listPolecatSessions = %+v, want %+v", got, want)
	}
}
