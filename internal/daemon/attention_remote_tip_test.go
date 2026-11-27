package daemon

import (
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// TestAttentionRemoteTip reads a rig's origin branch tip through the daemon's
// git seam: the injected gitfake answers with the tip it holds for the rig's
// bare repo, and no real git runs. The read is the one a direct-push landing
// compares against, so a unit test of the attention tick must not start git
// for it.
func TestAttentionRemoteTip(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	d := &Daemon{
		config:         &Config{TownRoot: town},
		logger:         log.New(io.Discard, "", 0),
		forgejoBaseURL: noForgejo,
	}
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	want := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})

	bare := constants.RigBareRepoPath(filepath.Join(town, attentionRig))
	f.InitBare(t, bare)
	f.AddRemote(t, bare, "origin", origin)

	s := d.attentionSources(time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC))
	got, err := s.remoteTip(attentionRig)
	if err != nil {
		t.Fatalf("remoteTip(%s): %v", attentionRig, err)
	}
	if got != want {
		t.Errorf("remoteTip(%s) = %q, want the gitfake origin tip %s", attentionRig, got, want)
	}
}
