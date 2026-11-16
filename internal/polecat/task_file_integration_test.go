//go:build integration

package polecat

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/testutil"
)

// taskFileLaunchCounter keeps each run's bead prefix apart across -count=N runs
// of this binary: every run allocates its own database in the container.
var taskFileLaunchCounter atomic.Int64

// countingBeads counts the Show calls the launch makes, so the test can tell a
// launch that reads the work bead once from one that reads it twice.
type countingBeads struct {
	beads.Client
	shows *int32
}

func (c countingBeads) Show(id string) (*beads.Issue, error) {
	atomic.AddInt32(c.shows, 1)
	return c.Client.Show(id)
}

// TestIntegrationLaunchBuildsTheTaskFileFromOneBeadRead drives the production
// launch against a real bd store: the bead travels from the container through
// the client the launch resolves for it into SpawnSpec.TaskFile, and the launch
// reads it once. The unit tier answers the bead read with a fake, so the store
// and the routing between them are only exercised here (gt-594jgj, gt-8w5v21).
func TestIntegrationLaunchBuildsTheTaskFileFromOneBeadRead(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not installed, skipping integration test")
	}
	testutil.RequireDoltContainer(t)

	n := taskFileLaunchCounter.Add(1)
	prefix := fmt.Sprintf("tf%d", n)
	town := t.TempDir()
	rigPath := filepath.Join(town, "gastown")
	mayorRigPath := filepath.Join(rigPath, "mayor", "rig")

	// The rig settings the sandbox spawn reads its Forgejo remote from.
	settings := filepath.Join(rigPath, "settings")
	if err := os.MkdirAll(settings, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{"remote_url":"http://forgejo:3000/owner/gastown.git"}}}`
	if err := os.WriteFile(filepath.Join(settings, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	// The town routes this run's prefix to the rig's database, which is where
	// the launch has to find the bead.
	townBeadsDir := filepath.Join(town, ".beads")
	if err := os.MkdirAll(townBeadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := beads.WriteRoutes(townBeadsDir, []beads.Route{
		{Prefix: "hq-", Path: "."},
		{Prefix: prefix + "-", Path: filepath.Join("gastown", "mayor", "rig")},
	}); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	// bd init runs in this directory, and a missing one fails the exec with
	// ENOENT under the binary's name.
	if err := os.MkdirAll(mayorRigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	initBeadsDBWithPrefix(t, mayorRigPath, prefix)

	db := beads.NewPinned(filepath.Join(mayorRigPath, ".beads"))
	issue, err := db.Create(beads.CreateOptions{
		Title:       "Render the spawn's task file",
		Description: "The seat reads its task from TASK.md.",
		Acceptance:  "## Acceptance\n\n- [x] the ticked box\n- [ ] the open box",
		Priority:    2,
	})
	if err != nil {
		t.Fatalf("create the work bead: %v", err)
	}
	for _, note := range []string{"first note", "second note"} {
		if err := db.Update(issue.ID, beads.UpdateOptions{AppendNotes: note}); err != nil {
			t.Fatalf("append note %q: %v", note, err)
		}
	}
	if err := db.AddCommentAs(issue.ID, "daemon", "a comment the seat reads"); err != nil {
		t.Fatalf("add comment: %v", err)
	}

	workDir, _, w := setupSessionBranchTestRepo(t)
	var shows int32
	var warnings []string
	m := &SessionManager{
		tmux: newFakeSessionTmux(),
		rig:  &rig.Rig{Name: "gastown", Path: rigPath},
		gits: w.opener(),
		warn: func(format string, args ...any) {
			warnings = append(warnings, fmt.Sprintf(format, args...))
		},
	}
	// The launch resolves the database for the bead and opens its client
	// there; the counter sits in front of that client so the reads are
	// visible without moving the read off the stored bead.
	m.beadsAt = func(beadsDir string) beads.Client {
		return countingBeads{Client: beads.NewPinned(beadsDir), shows: &shows}
	}
	backend := &recordingSandboxBackend{forgejo: "host.docker.internal", stopErr: errors.New("stop after prepare")}
	err = m.launch("Toast", m.SessionName("Toast"), SessionStartOptions{
		WorkDir:          workDir,
		Backend:          backend,
		Issue:            issue.ID,
		RuntimeConfigDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "stop after prepare") {
		t.Fatalf("launch = %v, want the backend's stop error after Prepare", err)
	}
	if backend.got == nil {
		t.Fatal("Prepare was never called, so no SpawnSpec was built")
	}
	text := string(backend.got.TaskFile)
	for _, want := range []string{
		"# TASK " + issue.ID + ": " + issue.Title,
		"The seat reads its task from TASK.md.",
		"- [x] the ticked box",
		"- [ ] the open box",
		"- first note",
		"- second note",
		"daemon: a comment the seat reads",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("SpawnSpec.TaskFile = %q, want it to carry %q", text, want)
		}
	}
	if got := atomic.LoadInt32(&shows); got != 1 {
		t.Errorf("Show calls = %d, want 1: the launch reads the work bead once", got)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %q, want none for a readable bead", warnings)
	}
}

// TestIntegrationLaunchCarriesARejectionIntoTheTaskFile drives the launch's
// rework path against a real bd store: a bead whose notes hold a MERGE
// REJECTION block, written by the production formatter, reaches
// SpawnSpec.TaskFile with that block's findings, and the unchanged fact is
// written only while the worktree still sits on the commit the refusal named.
// The unit tier answers the bead read with a fake and writes the note text by
// hand, so the store round-trip this path makes is exercised only here
// (gt-yx5nsp).
func TestIntegrationLaunchCarriesARejectionIntoTheTaskFile(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not installed, skipping integration test")
	}
	testutil.RequireDoltContainer(t)

	n := taskFileLaunchCounter.Add(1)
	prefix := fmt.Sprintf("tfr%d", n)
	town := t.TempDir()
	rigPath := filepath.Join(town, "gastown")
	mayorRigPath := filepath.Join(rigPath, "mayor", "rig")

	// The rig settings the sandbox spawn reads its Forgejo remote from.
	settings := filepath.Join(rigPath, "settings")
	if err := os.MkdirAll(settings, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{"remote_url":"http://forgejo:3000/owner/gastown.git"}}}`
	if err := os.WriteFile(filepath.Join(settings, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	// The town routes this run's prefix to the rig's database, which is where
	// the launch has to find the bead.
	townBeadsDir := filepath.Join(town, ".beads")
	if err := os.MkdirAll(townBeadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := beads.WriteRoutes(townBeadsDir, []beads.Route{
		{Prefix: "hq-", Path: "."},
		{Prefix: prefix + "-", Path: filepath.Join("gastown", "mayor", "rig")},
	}); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	if err := os.MkdirAll(mayorRigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	initBeadsDBWithPrefix(t, mayorRigPath, prefix)

	db := beads.NewPinned(filepath.Join(mayorRigPath, ".beads"))
	tests := []struct {
		name          string
		fixAfterwards bool
		wantUnchanged bool
	}{
		{name: "the branch still sits on the rejected commit", wantUnchanged: true},
		{name: "the branch moved past the rejected commit", fixAfterwards: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue, err := db.Create(beads.CreateOptions{
				Title:       "Rework the task file",
				Description: "The seat reads the refusal from TASK.md.",
				Priority:    2,
			})
			if err != nil {
				t.Fatalf("create the work bead: %v", err)
			}
			workDir, _, w := setupSessionBranchTestRepo(t)
			rejected := w.writeAndCommit(t, workDir, "the rejected attempt", map[string]string{"internal/x.go": "package x\n"})
			if tt.fixAfterwards {
				w.writeAndCommit(t, workDir, "the fix", map[string]string{"internal/x.go": "package x\n\n// fixed\n"})
			}
			// The note is the one Land writes, so the store round-trip and the
			// production format are both on trial: a reader that no longer
			// matches the writer leaves TASK.md without the refusal.
			note := land.FormatRejectionNote(land.RejectionNote{
				Attempt: 1,
				Kind:    string(land.RejectReview),
				Reason:  "om requested changes",
				Branch:  "polecat/rictus/" + issue.ID,
				Target:  "main",
				MR:      "gt-mr1",
				Findings: []land.Finding{{
					ID:       issue.ID + ".1",
					Severity: "major",
					Path:     "internal/x.go",
					Line:     12,
					Title:    "the retry never resets its counter",
				}},
				Head: rejected,
			})
			if err := db.Update(issue.ID, beads.UpdateOptions{AppendNotes: note}); err != nil {
				t.Fatalf("append the rejection note: %v", err)
			}

			var warnings []string
			m := &SessionManager{
				tmux: newFakeSessionTmux(),
				rig:  &rig.Rig{Name: "gastown", Path: rigPath},
				gits: w.opener(),
				warn: func(format string, args ...any) {
					warnings = append(warnings, fmt.Sprintf(format, args...))
				},
			}
			m.beadsAt = func(beadsDir string) beads.Client {
				return beads.NewPinned(beadsDir)
			}
			backend := &recordingSandboxBackend{forgejo: "host.docker.internal", stopErr: errors.New("stop after prepare")}
			err = m.launch("Toast", m.SessionName("Toast"), SessionStartOptions{
				WorkDir:          workDir,
				Backend:          backend,
				Issue:            issue.ID,
				RuntimeConfigDir: t.TempDir(),
			})
			if err == nil || !strings.Contains(err.Error(), "stop after prepare") {
				t.Fatalf("launch = %v, want the backend's stop error after Prepare", err)
			}
			if backend.got == nil {
				t.Fatal("Prepare was never called, so no SpawnSpec was built")
			}
			text := string(backend.got.TaskFile)
			for _, want := range []string{
				"## Earlier rejections",
				"### Attempt 1: review — om requested changes",
				"- id:" + issue.ID + ".1 sev:major internal/x.go:12 — the retry never resets its counter",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("SpawnSpec.TaskFile = %q, want it to carry %q", text, want)
				}
			}
			const fact = "## Rejection unchanged"
			if got := strings.Contains(text, fact); got != tt.wantUnchanged {
				t.Errorf("SpawnSpec.TaskFile carries the unchanged fact = %v, want %v\n%s", got, tt.wantUnchanged, text)
			}
			if len(warnings) != 0 {
				t.Errorf("warnings = %q, want none for a readable bead", warnings)
			}
		})
	}
}
