package polecat

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

// survivalFixture is a hermetic rig: a local bare "origin", a seed clone that
// writes to it, and a rig root with the shared .repo.git layout.
type survivalFixture struct {
	tmp, origin, seed, rigRoot, bare string
}

func newSurvivalFixture(t *testing.T) *survivalFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	tmp := t.TempDir()
	f := &survivalFixture{
		tmp:     tmp,
		origin:  filepath.Join(tmp, "origin.git"),
		seed:    filepath.Join(tmp, "seed"),
		rigRoot: filepath.Join(tmp, "gastown"),
	}
	f.bare = filepath.Join(f.rigRoot, ".repo.git")
	runGit(t, tmp, "init", "--bare", "--initial-branch=main", f.origin)
	runGit(t, tmp, "init", "--initial-branch=main", f.seed)
	runGit(t, f.seed, "config", "user.email", "test@example.com")
	runGit(t, f.seed, "config", "user.name", "test")
	f.commit(t, "base.txt", "base\n", "base")
	runGit(t, f.seed, "remote", "add", "origin", f.origin)
	runGit(t, f.seed, "push", "origin", "main")
	if err := os.MkdirAll(f.rigRoot, 0755); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "init", "--bare", f.bare)
	runGit(t, f.bare, "remote", "add", "origin", f.origin)
	return f
}

func (f *survivalFixture) commit(t *testing.T, file, content, msg string) {
	t.Helper()
	seedFile(t, filepath.Join(f.seed, file), content)
	runGit(t, f.seed, "add", file)
	runGit(t, f.seed, "commit", "-m", msg)
}

// branchWithWork creates branch off main in the seed with one commit.
func (f *survivalFixture) branchWithWork(t *testing.T, branch, file string) {
	t.Helper()
	runGit(t, f.seed, "checkout", "-q", "-b", branch, "main")
	f.commit(t, file, file+"\n", "work on "+branch)
	runGit(t, f.seed, "checkout", "-q", "main")
}

func (f *survivalFixture) push(t *testing.T, refs ...string) {
	t.Helper()
	runGit(t, f.seed, append([]string{"push", "-q", "origin"}, refs...)...)
}

const survivalIssue = "gt-elvf4"

func TestSurvivingWorkForIssue(t *testing.T) {
	const (
		older = "polecat/basalt/gt-elvf4+mu5wzd6q"
		newer = "polecat/agate/gt-elvf4+mu72g5cz"
	)

	t.Run("branch with an unmerged commit on origin survives", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		f.branchWithWork(t, older, "work.txt")
		f.push(t, older)
		assertSurvivor(t, f.rigRoot, older)
	})

	t.Run("branch equal to main does not survive", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		runGit(t, f.seed, "branch", older, "main")
		f.push(t, older)
		assertSurvivor(t, f.rigRoot, "")
	})

	t.Run("merged branch does not survive", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		f.branchWithWork(t, older, "work.txt")
		f.push(t, older)
		runGit(t, f.seed, "merge", "-q", "--no-ff", "-m", "merge", older)
		f.push(t, "main")
		assertSurvivor(t, f.rigRoot, "")
	})

	t.Run("rebase-merged branch does not survive (patch-id, not ancestry)", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		f.branchWithWork(t, older, "work.txt")
		f.push(t, older)
		// main moves on, then takes the same patch as a new commit: the
		// branch tip is not an ancestor of main, but its patch is on main.
		f.commit(t, "other.txt", "other\n", "unrelated")
		runGit(t, f.seed, "cherry-pick", older)
		f.push(t, "main")
		assertSurvivor(t, f.rigRoot, "")
	})

	t.Run("local-only branch with unpushed work survives", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		f.branchWithWork(t, older, "work.txt")
		// The rig repo holds the branch; origin never saw it.
		runGit(t, f.seed, "push", "-q", f.bare, older+":refs/heads/"+older)
		assertSurvivor(t, f.rigRoot, older)
	})

	t.Run("newest branch merged, older one still carries work", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		f.branchWithWork(t, older, "old.txt")
		runGit(t, f.seed, "branch", newer, "main")
		f.push(t, older, newer)
		assertSurvivor(t, f.rigRoot, older)
	})

	t.Run("other beads' branches are ignored", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		f.branchWithWork(t, "polecat/basalt/gt-other+mu5wzd6q", "work.txt")
		f.push(t, "polecat/basalt/gt-other+mu5wzd6q")
		assertSurvivor(t, f.rigRoot, "")
	})

	t.Run("idle polecat on an epic branch merged into integration does not survive", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		runGit(t, f.seed, "checkout", "-q", "-b", "integration/epic-x", "main")
		f.commit(t, "epic.txt", "epic\n", "epic groundwork")
		runGit(t, f.seed, "checkout", "-q", "-b", older, "integration/epic-x")
		f.commit(t, "work.txt", "work\n", "work on the epic")
		runGit(t, f.seed, "checkout", "-q", "integration/epic-x")
		runGit(t, f.seed, "merge", "-q", "--no-ff", "-m", "merge into epic", older)
		runGit(t, f.seed, "checkout", "-q", "main")
		f.push(t, older, "integration/epic-x")
		// The work (and the epic's own groundwork) is on the integration
		// branch, not on main: judged against main alone it would survive.
		assertSurvivor(t, f.rigRoot, "")
	})

	t.Run("epic branch with work not yet in integration survives", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		runGit(t, f.seed, "checkout", "-q", "-b", "integration/epic-x", "main")
		f.commit(t, "epic.txt", "epic\n", "epic groundwork")
		runGit(t, f.seed, "checkout", "-q", "-b", older, "integration/epic-x")
		f.commit(t, "work.txt", "work\n", "work on the epic")
		runGit(t, f.seed, "checkout", "-q", "main")
		f.push(t, older, "integration/epic-x")
		assertSurvivor(t, f.rigRoot, older)
	})

	t.Run("stale local origin ref is re-fetched", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		// The rig repo last saw the branch at main's tip (no work) ...
		runGit(t, f.seed, "branch", older, "main")
		f.push(t, older)
		runGit(t, f.bare, "fetch", "-q", "origin", "+refs/heads/*:refs/remotes/origin/*")
		// ... then work landed on origin.
		runGit(t, f.seed, "checkout", "-q", older)
		f.commit(t, "work.txt", "work\n", "late work")
		runGit(t, f.seed, "checkout", "-q", "main")
		f.push(t, older)
		assertSurvivor(t, f.rigRoot, older)
	})

	t.Run("unreachable origin with no local candidate is unknown", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		if err := os.RemoveAll(f.origin); err != nil {
			t.Fatal(err)
		}
		if got, err := SurvivingWorkForIssue(f.rigRoot, survivalIssue); err == nil {
			t.Fatalf("want an error for an unreachable origin, got branch %q", got)
		}
	})

	t.Run("no rig repo", func(t *testing.T) {
		t.Parallel()
		if _, err := SurvivingWorkForIssue(t.TempDir(), survivalIssue); !errors.Is(err, ErrNoRigRepo) {
			t.Fatalf("err = %v, want ErrNoRigRepo", err)
		}
	})
}

func assertSurvivor(t *testing.T, rigRoot, want string) {
	t.Helper()
	got, err := SurvivingWorkForIssue(rigRoot, survivalIssue)
	if err != nil {
		t.Fatalf("SurvivingWorkForIssue: %v", err)
	}
	if got != want {
		t.Fatalf("surviving work = %q, want %q", got, want)
	}
}

// installWorkBeadBd is a bd stub for unassignWorkBeads: `list` returns one
// in_progress work bead assigned to gastown/polecats/basalt, and every call's
// argv is appended to the returned log.
func installWorkBeadBd(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX bd stub")
	}
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "bd.log")
	script := `#!/bin/sh
echo "$@" >> '` + logPath + `'
for arg in "$@"; do
  case "$arg" in
    --*) ;;
    list) echo '[{"id":"gt-elvf4","title":"work","status":"in_progress","assignee":"gastown/polecats/basalt","issue_type":"task"}]'; exit 0 ;;
    *) exit 0 ;;
  esac
done
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// Polecat removal gives a hooked bead back only when its work does not
// survive, and then only through the guarded write (gt-vm5g4).
func TestUnassignWorkBeadsKeepsSurvivingWork(t *testing.T) {
	const branch = "polecat/basalt/gt-elvf4+mu5wzd6q"
	for _, tc := range []struct {
		name        string
		setup       func(t *testing.T, f *survivalFixture)
		wantRelease bool
	}{
		{name: "surviving work keeps the hook", setup: func(t *testing.T, f *survivalFixture) {
			f.branchWithWork(t, branch, "work.txt")
			f.push(t, branch)
		}},
		{name: "merged branch releases the hook", wantRelease: true, setup: func(t *testing.T, f *survivalFixture) {
			f.branchWithWork(t, branch, "work.txt")
			f.push(t, branch)
			runGit(t, f.seed, "merge", "-q", "--no-ff", "-m", "merge", branch)
			f.push(t, "main")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSurvivalFixture(t)
			tc.setup(t, f)
			logPath := installWorkBeadBd(t)

			mgr := NewManager(&rig.Rig{Name: "gastown", Path: f.rigRoot}, git.NewGit(f.rigRoot), nil)
			mgr.unassignWorkBeads("basalt")

			logged, _ := os.ReadFile(logPath)
			var releases []string
			for _, line := range strings.Split(string(logged), "\n") {
				if strings.Contains(line, "--status=open") {
					releases = append(releases, line)
				}
			}
			if !tc.wantRelease {
				if len(releases) != 0 {
					t.Fatalf("surviving work was released: %v", releases)
				}
				return
			}
			if len(releases) != 1 || !strings.Contains(releases[0], "--if-assignee=gastown/polecats/basalt") {
				t.Fatalf("want one guarded release, got %v", releases)
			}
		})
	}
}

// An https origin that stalls must not hang the predicate: over http git
// forks git-remote-http, and the bounded remote calls kill that helper along
// with git, so the answer comes back "unknown" near the bound (gt-vm5g4).
func TestSurvivingWorkStallingHTTPOriginIsUnknownAndBounded(t *testing.T) {
	for _, k := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY", "all_proxy", "ALL_PROXY"} {
		t.Setenv(k, "")
	}
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	var (
		mu   sync.Mutex
		held []net.Conn
	)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c) // accept, then say nothing
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	const bound = time.Second
	prev := workSurvivalFetchTimeout
	workSurvivalFetchTimeout = bound
	t.Cleanup(func() { workSurvivalFetchTimeout = prev })
	stallURL := "http://" + ln.Addr().String() + "/repo.git"

	for _, tc := range []struct {
		name       string
		localWork  bool
		remoteHits int // stalled remote calls before the answer: ls-remote, then the base fetch
	}{
		{name: "no local candidate", remoteHits: 1},
		{name: "local candidate needs the base refresh", localWork: true, remoteHits: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSurvivalFixture(t)
			if tc.localWork {
				branch := "polecat/basalt/" + survivalIssue + "+mu5wzd6q"
				f.branchWithWork(t, branch, "work.txt")
				f.push(t, branch)
				runGit(t, f.bare, "fetch", "-q", "origin", "+refs/heads/*:refs/heads/*")
			}
			runGit(t, f.bare, "remote", "set-url", "origin", stallURL)
			start := time.Now()
			got, err := SurvivingWorkForIssue(f.rigRoot, survivalIssue)
			elapsed := time.Since(start)
			t.Logf("returned in %v", elapsed.Round(10*time.Millisecond))
			if err == nil {
				t.Fatalf("want unknown (error) from a stalling origin, got branch %q", got)
			}
			if !strings.Contains(err.Error(), "timed out") {
				t.Fatalf("want a timeout in the unknown answer, got %v", err)
			}
			if limit := time.Duration(tc.remoteHits)*bound + 8*time.Second; elapsed > limit {
				t.Fatalf("predicate took %v, limit %v", elapsed, limit)
			}
		})
	}
}
