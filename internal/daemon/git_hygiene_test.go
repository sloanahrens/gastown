package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/rig"
)

// newGitHygieneDaemon is a daemon over a temp town whose mayor/rigs.json lists
// rigs, with git, the feed and the log captured.
func newGitHygieneDaemon(t *testing.T, rigs ...string) (*Daemon, *feedCapture, *bytes.Buffer) {
	t.Helper()
	town := t.TempDir()
	entries := make([]string, 0, len(rigs))
	for _, r := range rigs {
		entries = append(entries, `"`+r+`": {}`)
	}
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte(`{"rigs": {`+strings.Join(entries, ",")+`}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	feed := &feedCapture{}
	var logs bytes.Buffer
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(&logs, "", 0), dogFeedFn: feed.record}
	return d, feed, &logs
}

func TestGitHygieneRepoPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// layout is what the rig root holds: "bare" for the shared .repo.git,
		// "clone" for a mayor/rig working tree.
		layout []string
		want   string // the repository to clean, relative to the rig root; "" for none
	}{
		{name: "shared bare repo only", layout: []string{"bare"}, want: ".repo.git"},
		{name: "bare repo beside the working clone", layout: []string{"bare", "clone"}, want: ".repo.git"},
		{name: "working clone only", layout: []string{"clone"}, want: filepath.Join("mayor", "rig")},
		{name: "no repository", layout: nil, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rigRoot := t.TempDir()
			for _, dir := range tt.layout {
				if dir == "bare" {
					mustMkdirAll(t, filepath.Join(rigRoot, ".repo.git"))
					continue
				}
				mustMkdirAll(t, filepath.Join(rigRoot, "mayor", "rig", ".git"))
			}

			want := tt.want
			if want != "" {
				want = filepath.Join(rigRoot, want)
			}
			if got := gitHygieneRepoPath(&rig.Rig{Name: "r1", Path: rigRoot}); got != want {
				t.Errorf("gitHygieneRepoPath() = %q, want %q", got, want)
			}
		})
	}
}

// The preference for the shared bare repo is the patrol's alone: gt rig
// list --json and every other Rig.RepoPath caller still see the working clone.
func TestGitHygieneRepoPath_LeavesRepoPathAlone(t *testing.T) {
	t.Parallel()
	rigRoot := t.TempDir()
	clone := filepath.Join(rigRoot, "mayor", "rig")
	mustMkdirAll(t, filepath.Join(rigRoot, ".repo.git"))
	mustMkdirAll(t, filepath.Join(clone, ".git"))
	r := &rig.Rig{Name: "r1", Path: rigRoot}

	if got := r.RepoPath(); got != clone {
		t.Errorf("Rig.RepoPath() = %q, want the working clone %q", got, clone)
	}
	if got, want := gitHygieneRepoPath(r), filepath.Join(rigRoot, ".repo.git"); got != want {
		t.Errorf("gitHygieneRepoPath() = %q, want %q", got, want)
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRunGitHygiene_CleansMergedAndOrphanedBranches(t *testing.T) {
	t.Parallel()
	d, feed, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	f.SetRef(t, origin, "refs/heads/polecat/merged", base)
	f.SetRef(t, origin, "refs/heads/dependabot/merged", base)
	f.SetRef(t, origin, "refs/heads/polecat/live", base)
	open := f.Commit(t, origin, "polecat/open", "open work", map[string]string{"o.txt": "o\n"})
	f.Commit(t, origin, "polecat/live", "live work", map[string]string{"l.txt": "l\n"})

	repo := filepath.Join(d.config.TownRoot, "r1", "mayor", "rig")
	f.Clone(t, origin, repo)
	f.SetRef(t, repo, "refs/heads/done", base)
	f.SetRef(t, repo, "refs/heads/merge/held", base)
	f.SetRef(t, repo, "refs/heads/polecat/old", base)
	f.Commit(t, repo, "polecat/old", "unpushed", map[string]string{"x.txt": "x\n"})
	f.SetRef(t, repo, "refs/heads/polecat/live", base)
	f.Commit(t, repo, "polecat/live", "local live", map[string]string{"y.txt": "y\n"})
	f.SetRef(t, repo, "refs/heads/notes", base)
	f.Commit(t, repo, "notes", "someone's work", map[string]string{"n.txt": "n\n"})

	d.runGitHygiene()

	g := d.gitAt(repo)
	local, err := g.ListBranches("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"main", "merge/held", "notes", "polecat/live"}; !slices.Equal(local, want) {
		t.Errorf("local branches = %v, want %v (done merged, polecat/old orphaned)", local, want)
	}
	refs, err := g.ListRemoteRefsWithHashes("origin", "refs/heads/")
	if err != nil {
		t.Fatal(err)
	}
	var remote []string
	for _, r := range refs {
		remote = append(remote, strings.TrimPrefix(r.Name, "refs/heads/"))
	}
	if want := []string{"dependabot/merged", "main", "polecat/live", "polecat/open"}; !slices.Equal(remote, want) {
		t.Errorf("origin branches = %v, want %v (only polecat/merged deleted)", remote, want)
	}
	if tip := f.Ref(origin, "refs/heads/polecat/open"); tip != open {
		t.Errorf("origin polecat/open = %s, want %s", tip, open)
	}
	if !strings.Contains(logs.String(), "1 repo(s): 1 merged, 1 orphan, 1 remote branch(es) deleted") {
		t.Errorf("log lacks the summary:\n%s", logs.String())
	}
	if len(feed.payloads) != 0 {
		t.Errorf("feed = %v, want no failed cycle", feed.payloads)
	}
	if _, found, err := loadPatrolLastRun(d.config.TownRoot, "git_hygiene"); !found || err != nil {
		t.Errorf("last run not recorded: found=%t err=%v", found, err)
	}
}

func TestRunGitHygiene_UnreadableOriginDeletesOnlyMergedBranches(t *testing.T) {
	t.Parallel()
	d, _, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	repo := filepath.Join(d.config.TownRoot, "r1", "mayor", "rig")
	f.Clone(t, origin, repo)
	f.SetRef(t, repo, "refs/heads/polecat/done", base)
	f.SetRef(t, repo, "refs/heads/polecat/wip", base)
	f.Commit(t, repo, "polecat/wip", "unpushed", map[string]string{"w.txt": "w\n"})
	f.RemoveRepo(t, origin)

	d.runGitHygiene()

	local, _ := d.gitAt(repo).ListBranches("")
	if want := []string{"main", "polecat/wip"}; !slices.Equal(local, want) {
		t.Errorf("local branches = %v, want %v: without origin an unmerged branch cannot be called orphaned", local, want)
	}
	if !strings.Contains(logs.String(), "ls-remote origin") {
		t.Errorf("log does not report the unreadable origin:\n%s", logs.String())
	}
}

func TestRunGitHygiene_CleansARigThatHasOnlyTheSharedBareRepo(t *testing.T) {
	t.Parallel()
	d, feed, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	f.SetRef(t, origin, "refs/heads/polecat/merged", base)
	f.Commit(t, origin, "polecat/live", "live work", map[string]string{"l.txt": "l\n"})

	bare := filepath.Join(d.config.TownRoot, "r1", ".repo.git")
	f.InitBare(t, bare)
	f.AddRemote(t, bare, "origin", origin)
	f.SetRef(t, bare, "refs/heads/main", base)
	f.SetRef(t, bare, "refs/heads/done", base)
	f.SetRef(t, bare, "refs/heads/polecat/old", base)
	f.Commit(t, bare, "polecat/old", "unpushed", map[string]string{"x.txt": "x\n"})
	f.Commit(t, bare, "polecat/live", "local live", map[string]string{"y.txt": "y\n"})

	d.runGitHygiene()

	g := d.gitAt(bare)
	local, err := g.ListBranches("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"main", "polecat/live"}; !slices.Equal(local, want) {
		t.Errorf("local branches = %v, want %v (done merged, polecat/old orphaned)", local, want)
	}
	refs, err := g.ListRemoteRefsWithHashes("origin", "refs/heads/")
	if err != nil {
		t.Fatal(err)
	}
	var remote []string
	for _, r := range refs {
		remote = append(remote, strings.TrimPrefix(r.Name, "refs/heads/"))
	}
	if want := []string{"main", "polecat/live"}; !slices.Equal(remote, want) {
		t.Errorf("origin branches = %v, want %v (polecat/merged merged and deleted)", remote, want)
	}
	if !strings.Contains(logs.String(), "1 repo(s): 1 merged, 1 orphan, 1 remote branch(es) deleted") {
		t.Errorf("log lacks the summary:\n%s", logs.String())
	}
	if len(feed.payloads) != 0 {
		t.Errorf("feed = %v, want no failed cycle", feed.payloads)
	}
}

// The working clone install-gt.sh merges into is left alone while the shared
// bare repo beside it is cleaned (gt-ckoau).
func TestRunGitHygiene_CleansTheBareRepoWhenTheRigAlsoHasAClone(t *testing.T) {
	t.Parallel()
	d, _, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})

	rigRoot := filepath.Join(d.config.TownRoot, "r1")
	bare := filepath.Join(rigRoot, ".repo.git")
	f.InitBare(t, bare)
	f.AddRemote(t, bare, "origin", origin)
	f.SetRef(t, bare, "refs/heads/main", base)
	f.SetRef(t, bare, "refs/heads/done", base)

	clone := filepath.Join(rigRoot, "mayor", "rig")
	f.Clone(t, origin, clone)
	f.SetRef(t, clone, "refs/heads/done", base)

	d.runGitHygiene()

	local, err := d.gitAt(bare).ListBranches("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"main"}; !slices.Equal(local, want) {
		t.Errorf("bare repo branches = %v, want %v", local, want)
	}
	cloneBranches, err := d.gitAt(clone).ListBranches("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"done", "main"}; !slices.Equal(cloneBranches, want) {
		t.Errorf("mayor/rig branches = %v, want %v: the patrol must not clean the working clone", cloneBranches, want)
	}
	if !strings.Contains(logs.String(), bare) {
		t.Errorf("log does not name the bare repo %s:\n%s", bare, logs.String())
	}
}

func TestRunGitHygiene_NoRigRepositoryIsAFailedCycle(t *testing.T) {
	t.Parallel()
	d, feed, _ := newGitHygieneDaemon(t, "norepo")
	useGitfake(t, d)

	d.runGitHygiene()

	if len(feed.payloads) != 1 || !strings.Contains(feed.payloads[0]["reason"].(string), "no rig has a repository") {
		t.Errorf("feed = %v, want one failed git_hygiene cycle", feed.payloads)
	}
}

func TestGitHygieneConfig(t *testing.T) {
	t.Parallel()
	if got := gitHygieneInterval(nil); got != defaultGitHygieneInterval {
		t.Errorf("default interval = %v", got)
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{GitHygiene: &PatrolConfig{Enabled: true, Interval: "2h"}}}
	if got := gitHygieneInterval(on); got != 2*time.Hour {
		t.Errorf("configured interval = %v", got)
	}
	if !IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "git_hygiene") {
		t.Error("git_hygiene is off with no config entry; it defaults on")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{GitHygiene: &PatrolConfig{}}}, "git_hygiene") {
		t.Error("an explicit enabled=false entry did not turn git_hygiene off")
	}
}
