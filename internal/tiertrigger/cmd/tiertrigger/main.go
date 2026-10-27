// Command tiertrigger tells the tier sweep which container packages are due
// and records the commit a passing run tested (gt-e69b2).
//
//	go run ./internal/tiertrigger/cmd/tiertrigger skips integration-docker
//	go run ./internal/tiertrigger/cmd/tiertrigger record integration-docker
//
// scripts/tier-sweep.sh calls it twice around the docker.txt run: skips feeds
// TIER_SWEEP_SKIP with the packages that do not need to run, and record stamps
// the commit after a passing run. Both read the sweep's own environment:
// GT_TOWN_ROOT for the state file, TIER_SWEEP_TESTED for the commit the run
// began at, TIER_SWEEP_SKIP for the packages that did not run. A decision or
// record that fails writes no answer, names the failure on stderr and exits 1;
// the sweep turns that into its own line and runs every container package, so a
// broken trigger costs one full run instead of skipping one.
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
	// run, so a record leaves their state alone.
	skip string
	// deps lists the repo directories a package imports, transitively.
	deps func(pkg string) ([]string, error)
}

// repo is the repository the command reads: HEAD, the change since a recorded
// commit, and the checkout's status.
type repo interface {
	Rev(ref string) (string, error)
	DiffNameOnly(base, head string) ([]string, error)
	Status() (*git.GitStatus, error)
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
		repo:     git.NewGit("."),
		files:    tiertrigger.OSFiles{},
		docker:   dockerList,
		tested:   os.Getenv("TIER_SWEEP_TESTED"),
		skip:     os.Getenv("TIER_SWEEP_SKIP"),
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
			rel, err := filepath.Rel(root, dir)
			if err != nil {
				// An answer this root cannot be compared with leaves the package's
				// imports unknown, and unknown runs the package.
				return nil, fmt.Errorf("locate %s under %s: %w", dir, root, err)
			}
			if outside(rel) {
				continue
			}
			rel = filepath.ToSlash(rel)
			if seen[rel] {
				continue
			}
			seen[rel] = true
			dirs = append(dirs, rel)
		}
	}
	return dirs, nil
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
	switch args[0] {
	case "skips":
		return e.skips(args[1], stdout, stderr)
	case "record":
		return e.record(args[1], stderr)
	default:
		fmt.Fprintf(stderr, "tiertrigger: unknown command %q (want skips or record)\n", args[0])
		return 2
	}
}

// skips writes the TIER_SWEEP_SKIP entries for the packages that are not due,
// space-separated. When the decision fails it writes nothing instead, names the
// failure, and exits 1: the sweep says so in its own line and runs every
// container package, so a broken trigger never skips a test.
func (e env) skips(tier string, stdout, stderr io.Writer) int {
	entries, err := e.notDue(tier, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "tiertrigger: skips %s: %v: every container package runs\n", tier, err)
		return 1
	}
	if len(entries) > 0 {
		fmt.Fprintln(stdout, strings.Join(entries, " "))
	}
	return 0
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
// it cannot be sure what ran it writes nothing, names the failure, and exits 1:
// the sweep says so, and the next sweep runs those packages again.
func (e env) record(tier string, stderr io.Writer) int {
	if err := e.recordRun(tier); err != nil {
		fmt.Fprintf(stderr, "tiertrigger: record %s: %v: the next sweep runs them again\n", tier, err)
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
		return errors.New("no tested commit (TIER_SWEEP_TESTED is unset): nothing recorded")
	}
	if head != e.tested {
		return fmt.Errorf("HEAD is %s, not the commit the run began at (%s): nothing recorded", head, e.tested)
	}
	status, err := e.repo.Status()
	if err != nil {
		return fmt.Errorf("read the checkout: %w", err)
	}
	// Untracked files are in no commit, and the sweep's own build leaves one at
	// the repo root, so only a tracked difference means the run tested
	// something other than HEAD.
	if len(status.Modified)+len(status.Added)+len(status.Deleted)+len(status.Unmerged) > 0 {
		return fmt.Errorf("the checkout's tracked files differ from %s", head)
	}
	store := e.store()
	state, err := store.Load()
	if err != nil {
		return err
	}
	skipped := wholeSkips(tier, strings.Fields(e.skip))
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

// wholeSkips is the set of packages an exact <tier>:<pkg> entry skips whole;
// such a package did not run, so its record stays where it was. A
// <tier>:<pkg>:<regexp> entry still runs the package, so it is recorded.
func wholeSkips(tier string, entries []string) map[string]bool {
	out := map[string]bool{}
	for _, entry := range entries {
		pkg, ok := strings.CutPrefix(entry, tier+":")
		if !ok || pkg == "" || strings.Contains(pkg, ":") {
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
