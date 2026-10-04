//go:build integration

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

// TestIntegrationPolecatNukeHookedWorkRealGitAndBd drives the whole
// nukePolecatFullWithOptions over a real polecat sandbox: a temp git repo with
// a local bare origin, and a hooked work bead in the ephemeral Dolt the
// integration TestMain starts. The unit tier (TestNukeHandsItsVerdictToRemoval
// and friends) pins the hooked-work flow with the survival answer and the bead
// store injected; only real git and a real bd can show the parts that need real
// processes — the pre-nuke preserve push onto a real origin, the surviving-work
// predicate reading real git, and the release going through bd (gt-j7dz5).
//
// The two cases are the two answers the survival predicate gives for the same
// bead:
//
//   - an unmerged branch carries work found on no base branch, so the hook is
//     kept and the resume comment names the branch;
//   - a branch whose commits are on origin/main has nothing to protect, so the
//     bead is released.
func TestIntegrationPolecatNukeHookedWorkRealGitAndBd(t *testing.T) {
	// One exclusive scratch Dolt container for both cases: gt rig add mints the
	// databases it names, which the shared container's catalog must not see.
	requireScratchDoltServer(t)
	townRoot := setupTestTown(t)
	// beads.FindTownRoot (the nuke's work-bead releaser) requires the primary
	// marker; setupTestTown writes only the bare mayor/ secondary marker.
	writeTestTownConfig(t, townRoot)
	bridgeDoltPidToTown(t, townRoot)

	for _, tc := range []struct {
		name       string
		rigName    string
		prefix     string
		merged     bool
		wantHeld   bool
		wantResume bool
	}{
		{
			name:       "unmerged branch keeps the hooked bead and the resume comment names the branch",
			rigName:    "nukekeep",
			prefix:     "nkk",
			merged:     false,
			wantHeld:   true,
			wantResume: true,
		},
		{
			name:     "merged branch releases the hooked bead",
			rigName:  "nukerelease",
			prefix:   "nkr",
			merged:   true,
			wantHeld: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := buildNukeHookedWorkWorld(t, townRoot, tc.rigName, tc.prefix)

			if tc.merged {
				w.mergeWorkIntoMain(t)
			}

			// Fixture sanity: the production survival predicate must reach the
			// answer this case claims before the nuke runs. Otherwise the
			// post-nuke difference could come from a fixture that never created
			// the work, not from the nuke.
			got, err := polecat.SurvivingWorkForIssue(w.rigPath, w.workBeadID)
			if err != nil {
				t.Fatalf("surviving work before nuke: %v", err)
			}
			if tc.merged && got != "" {
				t.Fatalf("fixture is wrong: merged branch still reads as surviving on %q", got)
			}
			if !tc.merged && got != w.branch {
				t.Fatalf("fixture is wrong: unmerged branch reads as %q, want %q", got, w.branch)
			}

			// The nuke's first step is a tmux Stop. Prove it will find no
			// session before running it, so a name collision with a live seat
			// on this host fails the test instead of killing that seat.
			assertNoLiveHostSession(t, polecat.NewSessionManager(tmux.NewTmux(), w.rig, townRegistry()).SessionName(w.polecatName))

			// Drive the production function, not a restatement of its ordering.
			if err := nukePolecatFullWithOptions(w.polecatName, w.rigName, w.mgr, w.rig, nukePolecatOptions{}); err != nil {
				t.Fatalf("nukePolecatFullWithOptions: %v", err)
			}

			// The sandbox is gone either way.
			if _, err := os.Stat(filepath.Join(w.rigPath, "polecats", w.polecatName)); !os.IsNotExist(err) {
				t.Fatalf("polecat directory still present after nuke (err=%v)", err)
			}

			issue := w.showWorkBead(t)
			switch {
			case tc.wantHeld:
				if issue.Status != beads.StatusHooked {
					t.Fatalf("work bead status = %q, want %q: the polecat branch holds unmerged work, so the hook must be kept",
						issue.Status, beads.StatusHooked)
				}
				if issue.Assignee != w.agentID {
					t.Fatalf("work bead assignee = %q, want %q", issue.Assignee, w.agentID)
				}
				if !strings.Contains(w.comments(t), w.branch) {
					t.Fatalf("work bead comments do not name the surviving branch %q:\n%s", w.branch, w.comments(t))
				}
				if tc.wantResume && !strings.Contains(w.comments(t), "Resume it:") {
					t.Fatalf("work bead comments carry no resume command:\n%s", w.comments(t))
				}
			default:
				// Released: back to open with no assignee, the state
				// ReleaseIfAssignee writes for work with nothing to protect.
				if issue.Status != "open" {
					t.Fatalf("work bead status = %q, want open: a merged branch has nothing to protect", issue.Status)
				}
				if issue.Assignee != "" {
					t.Fatalf("work bead assignee = %q, want empty: a merged branch has nothing to protect", issue.Assignee)
				}
			}
		})
	}
}

// assertNoLiveHostSession fails if the host's tmux server has a session by
// this name. The nuke under test ends a polecat's session unconditionally; a
// test-only rig must never name a live one.
func assertNoLiveHostSession(t *testing.T, sessionName string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		return // no tmux: Stop fails on the missing binary, killing nothing
	}
	if err := exec.Command("tmux", "has-session", "-t", sessionName).Run(); err == nil {
		t.Fatalf("a live tmux session %q exists on this host; refusing to run a nuke that would stop it", sessionName)
	}
}

// writeTestTownConfig stamps the town's primary marker (mayor/town.json) so
// beads.FindTownRoot resolves the temp town rather than abandoning the walk at
// the bare mayor/ directory.
func writeTestTownConfig(t *testing.T, townRoot string) {
	t.Helper()
	cfg := &config.TownConfig{
		Type:      "town",
		Version:   config.CurrentTownVersion,
		Name:      "nuke-hooked-work-test",
		CreatedAt: time.Now().Truncate(time.Second),
	}
	if err := config.SaveTownConfig(filepath.Join(townRoot, "mayor", "town.json"), cfg); err != nil {
		t.Fatalf("save town.json: %v", err)
	}
}

// nukeHookedWorkWorld is one rig with a real bare origin, a real polecat
// worktree on a generated polecat branch, and a work bead hooked to that
// polecat in the rig's Dolt database.
type nukeHookedWorkWorld struct {
	rigName     string
	rigPath     string
	polecatName string
	agentID     string
	branch      string
	workBeadID  string
	rig         *rig.Rig
	mgr         *polecat.Manager
	bd          *beads.Beads
}

// buildNukeHookedWorkWorld lays out the town's rig through the production
// rig.Manager (a bare .repo.git whose origin is a local bare repo, a mayor/rig
// clone, and a beads database on the scratch container), then creates the
// polecat worktree through the production polecat.Manager and hooks a work bead
// to it.
func buildNukeHookedWorkWorld(t *testing.T, townRoot, rigName, prefix string) *nukeHookedWorkWorld {
	t.Helper()

	originDir := buildLocalBareOrigin(t)

	rigsConfig, err := config.LoadRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"))
	if err != nil {
		t.Fatalf("load rigs.json: %v", err)
	}
	r, err := rig.NewManager(townRoot, rigsConfig, git.NewGit(townRoot)).AddRig(rig.AddRigOptions{
		Name:        rigName,
		GitURL:      originDir,
		BeadsPrefix: prefix,
	})
	if err != nil {
		t.Fatalf("AddRig(%s): %v", rigName, err)
	}

	rigPath := filepath.Join(townRoot, rigName)
	// A name no real seat uses: the nuke's first step is a tmux Stop, and an
	// unregistered rig resolves to the default "gt" session prefix, so a
	// realistic-looking name could name a live session on the host. This one
	// never does, so Stop returns ErrSessionNotFound and touches no session.
	polecatName := "nukepc"
	agentID := fmt.Sprintf("%s/polecats/%s", rigName, polecatName)

	bd := beads.New(rigPath)
	workBead, err := bd.Create(beads.CreateOptions{
		Title: "work for a nuked polecat",
		Type:  "task",
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	status, assignee := beads.StatusHooked, agentID
	if err := bd.Update(workBead.ID, beads.UpdateOptions{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("hook work bead %s to %s: %v", workBead.ID, agentID, err)
	}

	mgr := polecat.NewManager(r, git.NewGit(r.Path), nil, nil)
	added, err := mgr.AddWithOptions(polecatName, polecat.AddOptions{HookBead: workBead.ID})
	if err != nil {
		t.Fatalf("AddWithOptions(%s): %v", polecatName, err)
	}
	if added.Branch == "" {
		t.Fatal("polecat has no branch; the survival predicate would have nothing to judge")
	}

	w := &nukeHookedWorkWorld{
		rigName:     rigName,
		rigPath:     rigPath,
		polecatName: polecatName,
		agentID:     agentID,
		branch:      added.Branch,
		workBeadID:  workBead.ID,
		rig:         r,
		mgr:         mgr,
		bd:          bd,
	}
	w.commitPolecatWork(t)
	return w
}

// buildLocalBareOrigin makes a bare repo on a local path with one commit on
// main, seeded from a throwaway work repo. Every push a nuke makes lands here.
func buildLocalBareOrigin(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	originDir := filepath.Join(tmp, "origin.git")
	runGitCmd(t, tmp, "init", "--bare", "--initial-branch=main", originDir)

	seed := filepath.Join(tmp, "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatalf("mkdir seed: %v", err)
	}
	runGitCmd(t, seed, "init", "--initial-branch=main")
	runGitCmd(t, seed, "config", "user.email", "test@example.com")
	runGitCmd(t, seed, "config", "user.name", "Test User")
	writeTestFile(t, filepath.Join(seed, "README.md"), "base\n")
	runGitCmd(t, seed, "add", "README.md")
	runGitCmd(t, seed, "commit", "-m", "base")
	runGitCmd(t, seed, "remote", "add", "origin", originDir)
	runGitCmd(t, seed, "push", "-u", "origin", "main")
	return originDir
}

// worktree is the polecat's git worktree.
func (w *nukeHookedWorkWorld) worktree() string {
	return filepath.Join(w.rigPath, "polecats", w.polecatName, w.rigName)
}

// commitPolecatWork adds a commit on the polecat branch so the branch carries
// work the survival predicate must be able to see.
func (w *nukeHookedWorkWorld) commitPolecatWork(t *testing.T) {
	t.Helper()
	wt := w.worktree()
	// The hermetic harness gives the process a test HOME with no global git
	// identity, so the commit needs one set on the repo.
	runGitCmd(t, wt, "config", "user.email", "test@example.com")
	runGitCmd(t, wt, "config", "user.name", "Test User")
	writeTestFile(t, filepath.Join(wt, "work.txt"), "polecat work\n")
	runGitCmd(t, wt, "add", "work.txt")
	runGitCmd(t, wt, "commit", "-m", "polecat work")
}

// mergeWorkIntoMain makes the polecat branch's commits reachable from
// origin/main, the state in which the survival predicate finds nothing to
// protect.
func (w *nukeHookedWorkWorld) mergeWorkIntoMain(t *testing.T) {
	t.Helper()
	wt := w.worktree()
	runGitCmd(t, wt, "push", "origin", "HEAD:main")
}

// showWorkBead reads the work bead's final state out of the rig database.
func (w *nukeHookedWorkWorld) showWorkBead(t *testing.T) *beads.Issue {
	t.Helper()
	issue, err := w.bd.Show(w.workBeadID)
	if err != nil {
		t.Fatalf("show work bead %s: %v", w.workBeadID, err)
	}
	if issue == nil {
		t.Fatalf("work bead %s vanished", w.workBeadID)
	}
	return issue
}

// comments returns the work bead's comments joined, for asserting on the
// resume note.
func (w *nukeHookedWorkWorld) comments(t *testing.T) string {
	t.Helper()
	comments, err := w.bd.Comments(w.workBeadID)
	if err != nil {
		t.Fatalf("comments on %s: %v", w.workBeadID, err)
	}
	texts := make([]string, 0, len(comments))
	for _, c := range comments {
		texts = append(texts, c.Text)
	}
	return strings.Join(texts, "\n")
}
