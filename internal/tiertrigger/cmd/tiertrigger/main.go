// Command tiertrigger tells the tier sweep which container packages are due
// and records the commit a passing run tested (gt-e69b2).
//
//	go run ./internal/tiertrigger/cmd/tiertrigger skips integration-docker
//	go run ./internal/tiertrigger/cmd/tiertrigger record integration-docker
//
// scripts/tier-sweep.sh calls it twice around the docker.txt run: skips feeds
// TIER_SWEEP_SKIP with the packages that do not need to run, and record stamps
// the commit after a passing run. Both read the sweep's own environment:
// GT_TOWN_ROOT for the state files, TIER_SWEEP_TESTED for the commit the run
// began at, TIER_SWEEP_SKIP for the packages that did not run, and
// TIER_SWEEP_TRIGGER=0 to turn the trigger off entirely. A decision or record
// that fails writes no answer, names the failure in the sweep's own words on
// stderr and exits 1; the sweep runs every container package when skips fails,
// so a broken trigger costs one full run instead of skipping one.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/tiertrigger"
)

// dockerList is the sweep's untagged container package list, relative to the
// repo root the script runs in.
const dockerList = "internal/testpolicy/docker.txt"

// env is one run's inputs. Tests set every field, so the clock, the daily
// window's zone, the repository and the files are all seams and no case reads
// the host.
type env struct {
	town   string
	now    time.Time
	zone   *time.Location
	repo   repo
	files  tiertrigger.Files
	docker string
	// loadZone reads the daily window's zone when zone is nil.
	loadZone func() (*time.Location, error)
	// tested is the commit the run began at (TIER_SWEEP_TESTED): a record stamps
	// it, and only while HEAD is still it.
	tested string
	// skip is the sweep's TIER_SWEEP_SKIP list: the packages it holds did not
	// run their full tests, so a record leaves their state alone.
	skip string
	// off is TIER_SWEEP_TRIGGER=0: the sweep wants every container package to
	// run, as it did before the trigger.
	off bool
	// deps lists the repo directories a package imports, transitively.
	deps func(pkg string) ([]string, error)
}

// repo is the repository the command reads: HEAD, the change since a recorded
// commit, the worktree the tests build, and the checkout's status.
type repo interface {
	tiertrigger.Git
	Rev(ref string) (string, error)
	Status() (*git.GitStatus, error)
}

// gitRepo is the real repository, with the worktree paths the due decision
// reads taken from the checkout's status.
type gitRepo struct{ *git.Git }

func (g gitRepo) Worktree() ([]string, error) {
	status, err := g.Git.Status()
	if err != nil {
		return nil, err
	}
	return worktreePaths(status), nil
}

// worktreePaths is every path a checkout has uncommitted changes in. Untracked
// files count: a new test file is code go test compiles, so its package has to
// run.
func worktreePaths(status *git.GitStatus) []string {
	return append(trackedDirty(status), status.Untracked...)
}

// trackedDirty is the paths a checkout's tracked files differ from HEAD in.
// Untracked files are left out: they are in no commit, and the sweep's own
// build leaves one at the repo root.
func trackedDirty(status *git.GitStatus) []string {
	paths := append([]string{}, status.Modified...)
	paths = append(paths, status.Added...)
	paths = append(paths, status.Deleted...)
	paths = append(paths, status.Unmerged...)
	return paths
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one command. A usage error exits 2; skips and record exit 1
// when they cannot answer, so the sweep can say so and run every package.
func run(args []string, stdout, stderr io.Writer) int {
	return runWith(args, env{
		town:     os.Getenv("GT_TOWN_ROOT"),
		now:      time.Now(),
		repo:     gitRepo{git.NewGit(".")},
		files:    tiertrigger.OSFiles{},
		docker:   dockerList,
		tested:   os.Getenv("TIER_SWEEP_TESTED"),
		skip:     os.Getenv("TIER_SWEEP_SKIP"),
		off:      os.Getenv("TIER_SWEEP_TRIGGER") == "0",
		deps:     goDeps,
		loadZone: tiertrigger.DailyZoneLocation,
	}, stdout, stderr)
}

// goDeps lists the repo directories a package imports, transitively, relative to
// the working directory, which is the repo root the sweep runs from.
func goDeps(pkg string) ([]string, error) {
	return listDeps(runGo, os.Getwd, pkg)
}

// runGo is the go tool's output for one command line.
func runGo(args ...string) ([]byte, error) {
	return exec.Command("go", args...).Output()
}

// listDeps turns go list's answer into the repo directories under the working
// directory: standard and module-cache packages are not repo code and are left
// out. It asks twice, untagged and with the integration tag, both times with
// the package's tests, because an import reached only from a _test.go file or a
// tagged file is still code those tests build: a change to it must run them
// (om, gt-e69b2).
func listDeps(run func(args ...string) ([]byte, error), getwd func() (string, error), pkg string) ([]string, error) {
	root, err := getwd()
	if err != nil {
		return nil, err
	}
	// A checkout reached through a symlink has two spellings for one tree: the
	// working directory can be the link while the go tool names the target (a
	// Mac's /tmp is /private/tmp). Resolving the root, and comparing each
	// directory with both spellings, keeps every repo import from reading as
	// the module cache — which would silently shrink the trigger set to the
	// package's own directory (om, gt-e69b2).
	if root, err = filepath.Abs(root); err != nil {
		return nil, fmt.Errorf("resolve the repo root: %w", err)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return nil, fmt.Errorf("resolve the repo root: %w", err)
	}
	var dirs []string
	seen := map[string]bool{}
	for _, tags := range []string{"", "integration"} {
		args := []string{"list", "-deps", "-test", "-f", "{{if not .Standard}}{{.Dir}}{{end}}"}
		if tags != "" {
			args = append(args, "-tags", tags)
		}
		out, err := run(append(args, pkg)...)
		if err != nil {
			return nil, fmt.Errorf("go list -deps -test %s: %w", pkg, err)
		}
		for _, dir := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if dir == "" {
				continue
			}
			rel, ok, err := repoRel(root, dir)
			if err != nil {
				return nil, err
			}
			if !ok || seen[rel] {
				continue
			}
			seen[rel] = true
			dirs = append(dirs, rel)
		}
	}
	return dirs, nil
}

// repoRel places dir under root, returning it slash-spelled and relative. ok is
// false for a directory that is genuinely outside the repository, such as the
// module cache. A directory the root cannot be compared with at all is an
// error: unknown runs the package, a silent drop would not (om, gt-e69b2).
func repoRel(root, dir string) (string, bool, error) {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", false, fmt.Errorf("locate %s under %s: %w", dir, root, err)
	}
	if !outside(rel) {
		return filepath.ToSlash(rel), true, nil
	}
	// The go tool may spell the same tree with the link resolved; a directory
	// it cannot resolve is not one this checkout holds.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		if rel, err := filepath.Rel(root, resolved); err == nil && !outside(rel) {
			return filepath.ToSlash(rel), true, nil
		}
	}
	return "", false, nil
}

// outside reports whether a root-relative path names something above the root,
// such as the module cache. The root itself (".") is inside: a package that
// imports the root package reads code any change there touched, and Runs reads
// "." as every path, which runs the package.
func outside(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// runWith answers one command from e's inputs.
func runWith(args []string, e env, stdout, stderr io.Writer) int {
	if len(args) < 2 || args[0] == "" || args[1] == "" {
		fmt.Fprintln(stderr, "tiertrigger: usage: tiertrigger skips|record <tier>")
		return 2
	}
	tier := args[1]
	if !validTier(tier) {
		fmt.Fprintf(stderr, "tiertrigger: %q is not a tier name\n", tier)
		return 2
	}
	// TIER_SWEEP_TRIGGER=0 asks for the sweep every container package used to
	// get: both commands answer nothing and succeed.
	if e.off {
		return 0
	}
	switch args[0] {
	case "skips":
		return e.skips(tier, stdout, stderr)
	case "record":
		return e.record(tier, stderr)
	default:
		fmt.Fprintf(stderr, "tiertrigger: unknown command %q (want skips or record)\n", args[0])
		return 2
	}
}

// validTier reports whether tier is a plain name: the run's start state is
// saved under it, so a name that could climb out of that directory, or reach
// git's or the shell's argument list, is refused.
func validTier(tier string) bool {
	if tier == "" || tier[0] == '-' || tier[0] == '.' {
		return false
	}
	for _, r := range tier {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// warn writes the sweep's own line for a trigger failure, where the rest of the
// sweep prints: the operator reads what broke, and what the sweep does instead.
func warn(w io.Writer, mode, tier string, err error, instead string) {
	fmt.Fprintf(w, "tier-sweep: TIER_SWEEP_TRIGGER: %s %s failed (%v); %s\n", mode, tier, err, instead)
}

// skips writes the TIER_SWEEP_SKIP entries for the packages that are not due,
// space-separated. It saves how the run begins first, so a record can tell a
// checkout that was already dirty from one the run dirtied. When either step
// fails it writes nothing instead, names the failure in the sweep's own words,
// and exits 1: the sweep runs every container package, so a broken trigger
// never skips a test.
func (e env) skips(tier string, stdout, stderr io.Writer) int {
	if err := e.begin(tier); err != nil {
		// No start state for this run, so drop an earlier one: a stale record
		// of a clean checkout must not vouch for a run whose start nobody saw.
		e.discardRun(tier, stderr)
		warn(stderr, "skips", tier, err, "every container package runs")
		return 1
	}
	entries, err := e.notDue(tier, stderr)
	if err != nil {
		warn(stderr, "skips", tier, err, "every container package runs")
		return 1
	}
	if len(entries) > 0 {
		fmt.Fprintln(stdout, strings.Join(entries, " "))
	}
	return 0
}

// discardRun drops the tier's run start state. A failure to remove it is named:
// record would otherwise read a stale start state and stamp a run it does not
// describe.
func (e env) discardRun(tier string, stderr io.Writer) {
	if e.town == "" {
		return
	}
	if err := e.files.RemoveFile(tiertrigger.RunPath(e.town, tier)); err != nil {
		fmt.Fprintf(stderr, "tiertrigger: drop the previous run state: %v\n", err)
	}
}

// begin saves how the run starts: HEAD, and the tracked files that already
// differ from it.
func (e env) begin(tier string) error {
	if e.town == "" {
		return errors.New("GT_TOWN_ROOT is unset")
	}
	head, err := e.repo.Rev("HEAD")
	if err != nil {
		return fmt.Errorf("read HEAD: %w", err)
	}
	status, err := e.repo.Status()
	if err != nil {
		return fmt.Errorf("read the checkout: %w", err)
	}
	return tiertrigger.SaveRun(e.files, tiertrigger.RunPath(e.town, tier), tiertrigger.Run{
		Commit: head,
		Dirty:  trackedDirty(status),
	})
}

// notDue returns the tier's skip entries for the packages the trigger rule
// keeps out of this sweep, in the list's order. A record whose commit cannot be
// diffed runs its package and is named on stderr: unreadable is not unchanged,
// and a record that keeps failing would otherwise turn every sweep into a full
// container run with nothing said about why.
func (e env) notDue(tier string, stderr io.Writer) ([]string, error) {
	if e.town == "" {
		return nil, errors.New("GT_TOWN_ROOT is unset")
	}
	pkgs, err := e.packages()
	if err != nil {
		return nil, err
	}
	loc, err := e.location()
	if err != nil {
		return nil, err
	}
	state, err := e.store().Load()
	if err != nil {
		return nil, err
	}
	due, unreadable := tiertrigger.Due(state, pkgs, e.repo, e.now, loc)
	for _, name := range due {
		if err, ok := unreadable[name]; ok {
			fmt.Fprintf(stderr, "tiertrigger: skips %s: %s: %v: running it\n", tier, name, err)
		}
	}
	runs := make(map[string]bool, len(due))
	for _, name := range due {
		runs[name] = true
	}
	var entries []string
	for _, pkg := range pkgs {
		if !runs[pkg.Name] {
			entries = append(entries, tier+":"+pkg.Name)
		}
	}
	return entries, nil
}

// record stamps the tier's packages with the commit a passing run tested. When
// it cannot be sure what ran it writes nothing, names the failure in the
// sweep's own words, and exits 1: the next sweep runs those packages again.
func (e env) record(tier string, stderr io.Writer) int {
	if err := e.recordRun(tier); err != nil {
		warn(stderr, "record", tier, err, "the next sweep runs the packages again")
		return 1
	}
	return 0
}

// recordRun saves the record after a passing run: every package the run
// covered, at HEAD, now.
func (e env) recordRun(tier string) error {
	if e.town == "" {
		return errors.New("GT_TOWN_ROOT is unset")
	}
	pkgs, err := e.packages()
	if err != nil {
		return err
	}
	head, err := e.repo.Rev("HEAD")
	if err != nil {
		return fmt.Errorf("read HEAD: %w", err)
	}
	// The run tested the commit it began at. A commit that landed during the run
	// is not what was tested, so nothing is recorded for it.
	if e.tested == "" {
		return errors.New("no tested commit (TIER_SWEEP_TESTED is unset)")
	}
	if head != e.tested {
		return fmt.Errorf("HEAD is %s, not the commit the run began at (%s)", head, e.tested)
	}
	// The run began where skips found the checkout. A tree that already
	// differed from HEAD then was not what HEAD holds, so the run tested
	// something else; files the run itself dirtied say nothing about that.
	run, err := tiertrigger.LoadRun(e.files, tiertrigger.RunPath(e.town, tier))
	if err != nil {
		return err
	}
	if run.Commit != head {
		return fmt.Errorf("the sweep decided at %s, not at %s", run.Commit, head)
	}
	if len(run.Dirty) > 0 {
		return fmt.Errorf("the checkout already differed from %s in %s when the run began", head, strings.Join(run.Dirty, " "))
	}
	store := e.store()
	state, err := store.Load()
	if err != nil {
		return err
	}
	skipped := skippedPackages(tier, strings.Fields(e.skip))
	recorded := false
	for _, pkg := range pkgs {
		if skipped[pkg.Name] {
			continue
		}
		state.Ran(pkg.Name, head, e.now)
		recorded = true
	}
	if !recorded {
		return nil
	}
	return store.Save(state)
}

// packages reads the sweep's untagged container list. Each package triggers on
// its own directory and the module's dependency files: go.mod and go.sum
// change what every package in the module builds against.
func (e env) packages() ([]tiertrigger.Package, error) {
	data, err := e.files.ReadFile(e.docker)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", e.docker, err)
	}
	var pkgs []tiertrigger.Package
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		path := strings.TrimSpace(line)
		if path == "" {
			continue
		}
		name := "./" + strings.TrimSuffix(strings.TrimPrefix(path, "./"), "/")
		triggers := []string{name, "go.mod", "go.sum"}
		if e.deps != nil {
			// A change in a repo package this one imports, however far down, makes it
			// due too: its build reads that code.
			dirs, err := e.deps(name)
			if err != nil {
				return nil, fmt.Errorf("list the imports of %s: %w", name, err)
			}
			for _, dir := range dirs {
				triggers = append(triggers, "./"+dir)
			}
		}
		pkgs = append(pkgs, tiertrigger.Package{Name: name, Triggers: triggers})
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("%s lists no container package", e.docker)
	}
	return pkgs, nil
}

// skippedPackages is the set of packages a skip entry keeps from running all of
// their tests: <tier>:<pkg> skips the package whole, and <tier>:<pkg>:<regexp>
// skips the tests the regexp names. Neither package passed its whole test set
// in this run, so neither is recorded as tested at HEAD; a record that claimed
// otherwise would let the next sweep skip tests that never ran.
func skippedPackages(tier string, entries []string) map[string]bool {
	out := map[string]bool{}
	for _, entry := range entries {
		pkg, ok := strings.CutPrefix(entry, tier+":")
		if !ok {
			continue
		}
		if i := strings.IndexByte(pkg, ':'); i >= 0 {
			pkg = pkg[:i]
		}
		if pkg == "" {
			continue
		}
		out[pkg] = true
	}
	return out
}

// location is the zone the daily window is read in.
func (e env) location() (*time.Location, error) {
	if e.zone != nil {
		return e.zone, nil
	}
	return e.loadZone()
}

// store is the state file of the town the trigger was dispatched from.
func (e env) store() tiertrigger.Store {
	return tiertrigger.Store{Path: tiertrigger.StatePath(e.town), Files: e.files}
}
