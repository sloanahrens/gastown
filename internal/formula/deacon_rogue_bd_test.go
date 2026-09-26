package formula

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// rogueBdCheckSnippet extracts the shell step 18 hands the deacon.
func rogueBdCheckSnippet(t *testing.T) string {
	t.Helper()
	d := deaconPatrolStep(t, "rogue-bd-check").Description
	const marker = "**Run the check:**"
	i := strings.Index(d, marker)
	if i < 0 {
		t.Fatalf("rogue-bd-check no longer contains %q", marker)
	}
	rest := d[i+len(marker):]
	open := strings.Index(rest, "```bash")
	if open < 0 {
		t.Fatalf("no bash block after %q", marker)
	}
	body := rest[open+len("```bash"):]
	end := strings.Index(body, "```")
	if end < 0 {
		t.Fatal("unterminated bash block in rogue-bd-check")
	}
	return strings.TrimSpace(body[:end])
}

// rogueBdTown is a throwaway ~/gt. The scan globs ~/gt/*/polecats and the
// sibling aggregates, so the run is given this as HOME and the real town never
// enters it.
type rogueBdTown struct {
	home string
	tmp  string
	src  string
}

func newRogueBdTown(t *testing.T) *rogueBdTown {
	t.Helper()
	requireGitWorktreeSupport(t)
	home := t.TempDir()
	tmp := filepath.Join(home, ".tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatalf("creating the fixture TMPDIR: %v", err)
	}
	return &rogueBdTown{home: home, tmp: tmp}
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

// bdSource returns the town's copy of the repo that builds bd, creating it on
// first use. It carries cmd/bd and ignores /bd, which is the shape of
// ~/gt/beads.
func (town *rogueBdTown) bdSource(t *testing.T) string {
	t.Helper()
	if town.src != "" {
		return town.src
	}
	src := filepath.Join(town.home, "bd-src")
	if err := os.MkdirAll(filepath.Join(src, "cmd", "bd"), 0o755); err != nil {
		t.Fatalf("creating the bd source repo: %v", err)
	}
	gitIn(t, src, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(src, "cmd", "bd", "main.go"), "package main\n")
	writeFile(t, filepath.Join(src, ".gitignore"), "/bd\n")
	gitIn(t, src, "add", "-A")
	gitIn(t, src, "commit", "-qm", "base")
	town.src = src
	return src
}

// newBdSourceWorktree adds a worktree of the bd source repo under the given
// aggregate and returns the path to the `make build` output at its root.
func (town *rogueBdTown) newBdSourceWorktree(t *testing.T, rig, agent string) string {
	t.Helper()
	wt := filepath.Join(town.home, "gt", rig, "polecats", agent, rig)
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatalf("creating the worktree parent: %v", err)
	}
	gitIn(t, town.bdSource(t), "worktree", "add", "-q", wt, "-b", agent)

	built := filepath.Join(wt, "bd")
	writeExec(t, built, "#!/bin/sh\nexit 0\n")
	return built
}

// newUnignoredBd drops a bd into a subdirectory the repo does not ignore, the
// shape of a copy an agent put there by hand.
func (town *rogueBdTown) newUnignoredBd(t *testing.T, worktree string) string {
	t.Helper()
	p := filepath.Join(worktree, "sub", "bd")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("creating the unignored subdirectory: %v", err)
	}
	writeExec(t, p, "#!/bin/sh\nexit 0\n")
	return p
}

// newSymlinkBd points a link named bd at another bd, the shape that resolves
// through to a release the worktree does not own (gt-19zn).
func (town *rogueBdTown) newSymlinkBd(t *testing.T, worktree, target string) string {
	t.Helper()
	p := filepath.Join(worktree, "bin", "bd")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("creating the symlink directory: %v", err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatalf("symlinking %s: %v", p, err)
	}
	return p
}

// newForeignBd drops a bd into a worktree of a repo that builds none, so the
// build-output exemption has no source to point at. ignoresAll adds a
// .gitignore that hides everything, which is the shape that would slip past a
// check-ignore-only exemption.
func (town *rogueBdTown) newForeignBd(t *testing.T, rig, agent string, ignoresAll bool) string {
	t.Helper()
	repo := filepath.Join(town.home, "gt", rig, "polecats", agent, "nope")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("creating the foreign repo: %v", err)
	}
	gitIn(t, repo, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(repo, "a.txt"), "x\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-qm", "base")
	if ignoresAll {
		writeFile(t, filepath.Join(repo, ".gitignore"), "*\n")
	}

	p := filepath.Join(repo, "bd")
	writeExec(t, p, "#!/bin/sh\nexit 0\n")
	return p
}

// asSymlink replaces a bd on disk with a link to another one.
func asSymlink(t *testing.T, path, target string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing %s: %v", path, err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlinking %s: %v", path, err)
	}
}

// newOnPathBd drops a bd in a crew directory and returns it with that
// directory, which the caller puts on PATH.
func (town *rogueBdTown) newOnPathBd(t *testing.T, rig, agent string) (bd, dir string) {
	t.Helper()
	dir = filepath.Join(town.home, "gt", rig, "crew", agent)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating the crew directory: %v", err)
	}
	bd = filepath.Join(dir, "bd")
	writeExec(t, bd, "#!/bin/sh\nexit 0\n")
	return bd, dir
}

// runRogueBdCheck runs the extracted snippet in the fixture town. PATH is
// passed in rather than inherited: whether a bd is reachable at all decides
// which branch of the scan it lands in, so the test owns it.
func runRogueBdCheck(t *testing.T, town *rogueBdTown, pathDirs string) string {
	t.Helper()
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range testGitEnv() {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "TMPDIR=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+town.home, "PATH="+pathDirs, "TMPDIR="+town.tmp)

	cmd := exec.Command("bash", "-c", rogueBdCheckSnippet(t))
	cmd.Dir = town.home
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rogue-bd-check exited non-zero (err=%v):\n%s", err, out)
	}

	left, err := filepath.Glob(filepath.Join(town.tmp, "*"))
	if err != nil {
		t.Fatalf("globbing the fixture TMPDIR: %v", err)
	}
	if len(left) > 0 {
		t.Errorf("rogue-bd-check left its candidate list behind: %v", left)
	}
	return strings.TrimSpace(string(out))
}

// classOf returns the verdict line the scan printed for path, or "" when it
// never listed the path at all.
func classOf(t *testing.T, out, path string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(line, " "+path) {
			return line
		}
	}
	t.Errorf("scan never listed %s:\n%s", path, out)
	return ""
}

// TestDeaconRogueBdCheckSparesBuildOutputOfTheBdRepo pins gt-5zsc: step 18
// flagged <worktree-root>/bd in every beads worktree that had ever run `make
// build`, and neutralizing those (chmod 644) breaks the rig's own build and
// test loop. A town whose only bd is that build output reports clean.
func TestDeaconRogueBdCheckSparesBuildOutputOfTheBdRepo(t *testing.T) {
	town := newRogueBdTown(t)
	built := town.newBdSourceWorktree(t, "beads", "dust")

	out := runRogueBdCheck(t, town, os.Getenv("PATH"))

	if got := classOf(t, out, built); got != "BY-DESIGN build output "+built {
		t.Errorf("the beads rig's own make build output is not exempt: %q\n%s", got, out)
	}
	if strings.Contains(out, "FINDING") {
		t.Errorf("a town holding only the bd source's build output reported a finding:\n%s", out)
	}
	if !strings.Contains(out, "fail=0 candidates=1") {
		t.Errorf("scan did not account for its one candidate:\n%s", out)
	}
}

// TestDeaconRogueBdCheckFlagsEveryShadow is the other half: the exemption
// covers the bd source's own ignored build output and nothing else. It runs
// step 18's shell against a town holding one of each shape a bd can take and
// reads the verdict printed for each (gt-li4t).
func TestDeaconRogueBdCheckFlagsEveryShadow(t *testing.T) {
	town := newRogueBdTown(t)
	built := town.newBdSourceWorktree(t, "beads", "dust")
	worktree := filepath.Dir(built)
	unignored := town.newUnignoredBd(t, worktree)
	symlinked := town.newSymlinkBd(t, worktree, built)
	foreign := town.newForeignBd(t, "other", "p1", false)
	hidden := town.newForeignBd(t, "other", "p2", true)
	onPath, onPathDir := town.newOnPathBd(t, "other", "sloan")

	// A bd symlinked where make build would have written one: the path is
	// ignored and its worktree carries cmd/bd, so only the symlink test keeps
	// it out of the by-design bucket (gt-19zn).
	symlinkedRoot := town.newBdSourceWorktree(t, "beads", "fury")
	asSymlink(t, symlinkedRoot, built)

	out := runRogueBdCheck(t, town, os.Getenv("PATH"))
	for path, want := range map[string]string{
		built:         "BY-DESIGN build output ",
		unignored:     "FINDING ",
		symlinked:     "FINDING symlink ",
		symlinkedRoot: "FINDING symlink ",
		foreign:       "FINDING ",
		hidden:        "FINDING ",
		onPath:        "FINDING ",
	} {
		if got := classOf(t, out, path); got != want+path {
			t.Errorf("%s\n  got  %q\n  want %q\n%s", path, got, want+path, out)
		}
	}
	if !strings.Contains(out, "fail=0 candidates=7") {
		t.Errorf("scan did not account for all seven candidates:\n%s", out)
	}

	// A bd reachable on PATH is the shadow that matters most, and this is what
	// stops the exemption from reading as "any ignored bd, anywhere".
	out = runRogueBdCheck(t, town, onPathDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if got := classOf(t, out, onPath); got != "FINDING on-PATH "+onPath {
		t.Errorf("a bd whose directory is on PATH is not flagged on-PATH: %q\n%s", got, out)
	}
}
