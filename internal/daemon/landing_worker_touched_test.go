package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
)

// touchedRawCommit is one commit of a touchedRawLog: its full sha and the
// paths it changed.
type touchedRawCommit struct {
	sha   string
	paths []string
}

// touchedRawLog renders the stream (*git.Git).CommitFileChanges parses: a
// "C <sha> <parent>" header for each commit, then a raw meta token and its
// path token for each file the commit changed. Commits are listed in the
// order git log prints them, newest first.
func touchedRawLog(commits ...touchedRawCommit) string {
	var b strings.Builder
	for _, c := range commits {
		b.WriteString("C " + c.sha + " " + strings.Repeat("f", 40) + "\x00")
		for _, path := range c.paths {
			b.WriteString("\n:100644 100644 " + strings.Repeat("a", 40) + " " + strings.Repeat("b", 40) + " M\x00" + path + "\x00")
		}
	}
	return b.String()
}

// pathTouchedWorker builds the daemon's landing worker for a rig whose
// follow-up feed is on, with the landing worker's git handle answered by run.
// It is the production construction path (newRigLandingWorker), so the test
// drives the wiring rather than a helper.
func pathTouchedWorker(t *testing.T, run git.ExecFunc) *land.Lander {
	t.Helper()
	townRoot := t.TempDir()
	const rigName = "gastown" // the rig newForgejoPromoteDaemon gives its fake remote
	rigPath := promoteRigConfig(t, townRoot, rigName, filepath.Join(t.TempDir(), "promote.key"))
	if err := os.MkdirAll(filepath.Join(rigPath, "settings"), 0o755); err != nil {
		t.Fatalf("mkdir settings: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, "settings", "config.json"),
		[]byte(`{"type":"rig-settings","version":1,"landing":{"followups":true}}`), 0o644); err != nil {
		t.Fatalf("write rig settings: %v", err)
	}
	d := newForgejoPromoteDaemon(t, townRoot, "")
	d.landingRemoteGitFn = func(dir string) *git.Git { return git.NewGitWithExec(dir, run) }

	w, err := d.newRigLandingWorker(rigName)
	if err != nil {
		t.Fatalf("newRigLandingWorker: %v", err)
	}
	lander, ok := w.Lander.(*land.Lander)
	if !ok {
		t.Fatalf("Lander = %T, want *land.Lander", w.Lander)
	}
	return lander
}

// TestLandingWorkerPathTouched is gt-h80mxo: a rig with the follow-up feed on
// wires Lander.PathTouched to the commits that touched a finding's file
// between the landing the finding is queued from and the current base, read
// from the rig repository through the handle the landing worker's other reads
// use. The feed marks the finding with at most five short shas, newest first;
// a git failure leaves it unmarked instead of failing the landing.
func TestLandingWorkerPathTouched(t *testing.T) {
	t.Parallel()
	const (
		landed = "landed111"
		base   = "base222"
		path   = "internal/land/note.go"
	)
	sha := func(c byte) string { return strings.Repeat(string(c), 40) }

	t.Run("reads the commits that touched the path", func(t *testing.T) {
		t.Parallel()
		var calls [][]string
		lander := pathTouchedWorker(t, func(c git.ExecCall) (string, string, error) {
			calls = append(calls, c.Args())
			return touchedRawLog(
				touchedRawCommit{sha: sha('a'), paths: []string{"internal/land/other.go"}}, // did not touch path
				touchedRawCommit{sha: sha('b'), paths: []string{path}},
				touchedRawCommit{sha: sha('c'), paths: []string{"elsewhere.go", path}},
				touchedRawCommit{sha: sha('d'), paths: []string{"internal/land/other.go"}}, // did not touch path
				touchedRawCommit{sha: sha('e'), paths: []string{path}},
				touchedRawCommit{sha: sha('f'), paths: []string{path}},
				touchedRawCommit{sha: sha('g'), paths: []string{path}},
				touchedRawCommit{sha: sha('h'), paths: []string{path}}, // past the five-short-sha cap
			), "", nil
		})
		if lander.PathTouched == nil {
			t.Fatal("PathTouched is nil; a landing feeds the replay with no commit to mark the finding")
		}
		got := lander.PathTouched(landed, base, path)
		want := []string{
			strings.Repeat("b", 8), strings.Repeat("c", 8), strings.Repeat("e", 8),
			strings.Repeat("f", 8), strings.Repeat("g", 8),
		}
		if !slices.Equal(got, want) {
			t.Errorf("PathTouched(%q, %q, %q) = %q, want %q: the shas that touched the file, newest first, at most five, shortened", landed, base, path, got, want)
		}
		if len(calls) != 1 {
			t.Fatalf("git was called %d times, want once", len(calls))
		}
		if !slices.Contains(calls[0], landed+".."+base) {
			t.Errorf("git args = %q, want the range %q", calls[0], landed+".."+base)
		}
	})

	t.Run("a git error marks nothing and the Lander is still built", func(t *testing.T) {
		t.Parallel()
		lander := pathTouchedWorker(t, func(git.ExecCall) (string, string, error) {
			return "", "fatal: not a git repository", errors.New("git failed")
		})
		if lander.PathTouched == nil {
			t.Fatal("PathTouched is nil after a git failure; the landing worker built a Lander with no hook")
		}
		if got := lander.PathTouched(landed, base, path); got != nil {
			t.Errorf("PathTouched on a git error = %q, want nil so the feed marks nothing", got)
		}
	})

	t.Run("a degenerate call asks git nothing", func(t *testing.T) {
		t.Parallel()
		var calls int
		lander := pathTouchedWorker(t, func(git.ExecCall) (string, string, error) {
			calls++
			return "", "", nil
		})
		if lander.PathTouched == nil {
			t.Fatal("PathTouched is nil; the rig's feed has no hook")
		}
		for _, call := range [][3]string{
			{"", base, path},
			{landed, "", path},
			{landed, base, "  "},
			{"-x", base, path}, // an end git would read as an option
			{landed, "-y", path},
		} {
			if got := lander.PathTouched(call[0], call[1], call[2]); got != nil {
				t.Errorf("PathTouched(%q, %q, %q) = %q, want nil", call[0], call[1], call[2], got)
			}
		}
		if calls != 0 {
			t.Errorf("git was called %d times for an empty range or path, want never", calls)
		}
	})
}
