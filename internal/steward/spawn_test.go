package steward

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// fakeGit is the rig repository: a set of refs, plus what the spawner did to
// the worktree.
type fakeGit struct {
	refs       map[string]bool
	fetched    []string
	added      []string
	removed    []string
	pruned     bool
	fetchErr   error
	fetchMakes string // the ref the fetch makes present
}

func (f *fakeGit) RefExists(ref string) (bool, error) { return f.refs[ref], nil }

func (f *fakeGit) FetchRefspecWithTimeout(remote, refspec string, timeout time.Duration) error {
	f.fetched = append(f.fetched, remote+" "+refspec)
	if f.fetchErr != nil {
		return f.fetchErr
	}
	if f.fetchMakes != "" {
		f.refs[f.fetchMakes] = true
	}
	return nil
}

func (f *fakeGit) WorktreeAddDetached(path, ref string) error {
	f.added = append(f.added, path+"@"+ref)
	return os.MkdirAll(path, 0o700)
}

func (f *fakeGit) WorktreeRemove(path string, force bool) error {
	f.removed = append(f.removed, path)
	return nil
}

func (f *fakeGit) WorktreePrune() error { f.pruned = true; return nil }

func testSpawner(t *testing.T, g *fakeGit) (*AgentSpawner, *exec.Cmd) {
	t.Helper()
	var last exec.Cmd
	return &AgentSpawner{
		TownRoot: t.TempDir(),
		Rig:      "gastown",
		Repo:     "/town/gastown/.repo.git",
		OpenGit:  func(string) SpawnGit { return g },
		ResolveAgent: func(model string) (*agentconfig.RuntimeConfig, error) {
			return &agentconfig.RuntimeConfig{Command: "claude", Args: []string{"--dangerously-skip-permissions", "--model", model}}, nil
		},
		Run: func(_ context.Context, cmd *exec.Cmd) error {
			last = *cmd
			return nil
		},
	}, &last
}

// TestAgentSpawnerRunsAtTheSubmittedHead: the worktree is the commit the
// event named, fetched first when the rig repository lacks it, and the
// command carries the headless prompt.
func TestAgentSpawnerRunsAtTheSubmittedHead(t *testing.T) {
	t.Parallel()
	g := &fakeGit{refs: map[string]bool{}, fetchMakes: "c0ffee^{commit}"}
	sp, last := testSpawner(t, g)
	dir := filepath.Join(t.TempDir(), "jobs", "steward-1")
	res := sp.Spawn(context.Background(), SpawnRequest{
		ID: "steward-1", Dir: dir, Prompt: "do the thing", Model: "deepseek-flash",
		Timeout: time.Minute, Event: Event{Kind: KindReview, Bead: "gt-x", Branch: "polecat/a/gt-x", Head: "c0ffee"},
	})
	if len(g.fetched) != 1 || !strings.Contains(g.fetched[0], "+refs/heads/polecat/a/gt-x:refs/remotes/origin/polecat/a/gt-x") {
		t.Fatalf("fetched = %v", g.fetched)
	}
	if len(g.added) != 1 || g.added[0] != dir+"@c0ffee" {
		t.Fatalf("worktree added = %v, want %s@c0ffee", g.added, dir)
	}
	if len(g.removed) != 1 || !g.pruned {
		t.Errorf("the job's worktree was not removed: removed=%v pruned=%v", g.removed, g.pruned)
	}
	args := last.Args
	if last.Dir != dir || last.Path == "" {
		t.Fatalf("command ran in %q as %q", last.Dir, last.Path)
	}
	if got := args[len(args)-2]; got != "-p" || args[len(args)-1] != "do the thing" {
		t.Errorf("args = %v, want them to end in -p and the prompt", args)
	}
	if len(args) < 4 || args[1] != "--dangerously-skip-permissions" || args[2] != "--model" || args[3] != "deepseek-flash" {
		t.Errorf("args = %v, want the preset's flags then -p", args)
	}
	var hasHead bool
	for _, env := range last.Env {
		if env == "GT_ROLE=gastown/steward" || env == "GT_STEWARD_BEAD=gt-x" {
			hasHead = true
		}
	}
	if !hasHead {
		t.Errorf("env lacks the seat's identity: %v", last.Env)
	}
	// The verdict is read before the worktree goes away.
	if res.VerdictErr == nil || !strings.Contains(res.VerdictErr.Error(), "no steward-result.json") {
		t.Errorf("a job that wrote no verdict = %+v", res)
	}
}

func TestAgentSpawnerReadsTheVerdict(t *testing.T) {
	t.Parallel()
	g := &fakeGit{refs: map[string]bool{"c0ffee^{commit}": true}}
	sp, _ := testSpawner(t, g)
	sp.Run = func(_ context.Context, cmd *exec.Cmd) error {
		return os.WriteFile(filepath.Join(cmd.Dir, ResultFile), []byte(`{"outcome":"fixed","summary":"rebased"}`), 0o600)
	}
	res := sp.Spawn(context.Background(), SpawnRequest{
		ID: "steward-1", Dir: filepath.Join(t.TempDir(), "steward-1"), Prompt: "p", Model: "deepseek-flash",
		Timeout: time.Minute, Event: Event{Bead: "gt-x", Branch: "b", Head: "c0ffee"},
	})
	if res.Verdict == nil || res.Verdict.Outcome != OutcomeFixed || res.Verdict.Summary != "rebased" {
		t.Fatalf("verdict = %+v (err %v)", res.Verdict, res.VerdictErr)
	}
	if len(g.fetched) != 0 {
		t.Errorf("a head already in the repository was fetched: %v", g.fetched)
	}
}

// TestAgentSpawnerRefusals: every way a job cannot start names why, and
// nothing is added to the repository when it does not start.
func TestAgentSpawnerRefusals(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		git   *fakeGit
		event Event
		want  string
	}{
		"no head":                            {&fakeGit{refs: map[string]bool{}}, Event{Bead: "gt-x", Branch: "b"}, "names no head"},
		"head absent and no branch to fetch": {&fakeGit{refs: map[string]bool{}}, Event{Bead: "gt-x", Head: "c0ffee"}, "names no branch to fetch"},
		"fetch fails": {&fakeGit{refs: map[string]bool{}, fetchErr: errors.New("origin unreachable")},
			Event{Bead: "gt-x", Branch: "b", Head: "c0ffee"}, "origin unreachable"},
		"head still absent after the fetch": {&fakeGit{refs: map[string]bool{}},
			Event{Bead: "gt-x", Branch: "b", Head: "c0ffee"}, "is not on origin/b after fetching"},
		"a head absent from the repository is not run on a guess": {&fakeGit{refs: map[string]bool{}},
			Event{Bead: "gt-x", Branch: "b", Head: "c0ffee"}, "not on origin/b"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sp, _ := testSpawner(t, tc.git)
			res := sp.Spawn(context.Background(), SpawnRequest{
				ID: "steward-1", Dir: filepath.Join(t.TempDir(), "steward-1"), Prompt: "p", Model: "deepseek-flash",
				Timeout: time.Minute, Event: tc.event,
			})
			if res.Err == nil || !strings.Contains(res.Err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %q", res.Err, tc.want)
			}
			if len(tc.git.added) != 0 {
				t.Errorf("a refused job still made a worktree: %v", tc.git.added)
			}
		})
	}
}

func TestAgentSpawnerUnknownPresetIsAnError(t *testing.T) {
	t.Parallel()
	g := &fakeGit{refs: map[string]bool{"c0ffee^{commit}": true}}
	sp, _ := testSpawner(t, g)
	sp.ResolveAgent = func(string) (*agentconfig.RuntimeConfig, error) {
		return nil, errors.New("unknown agent \"nope\"")
	}
	res := sp.Spawn(context.Background(), SpawnRequest{
		ID: "steward-1", Dir: filepath.Join(t.TempDir(), "steward-1"), Prompt: "p", Model: "nope",
		Timeout: time.Minute, Event: Event{Bead: "gt-x", Branch: "b", Head: "c0ffee"},
	})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "unknown agent") {
		t.Fatalf("err = %v", res.Err)
	}
}

// TestPruneJobDirs: a worktree that outlived its job goes when it is old,
// and a young one is left alone.
func TestPruneJobDirs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	old := filepath.Join(root, "steward-old")
	young := filepath.Join(root, "steward-young")
	other := filepath.Join(root, "not-a-job")
	for _, dir := range []string{old, young, other} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	past := testEpoch.Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	g := &fakeGit{refs: map[string]bool{}}
	if err := PruneJobDirs(g, root, 7*24*time.Hour, testEpoch); err != nil {
		t.Fatal(err)
	}
	if len(g.removed) != 1 || g.removed[0] != old {
		t.Errorf("removed = %v, want only %s", g.removed, old)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the old job dir is still there: %v", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Errorf("the young job dir was removed: %v", err)
	}
}
