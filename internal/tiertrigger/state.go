package tiertrigger

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
)

// StateFileName is the state file's name under the town runtime.
const StateFileName = "state.json"

// StatePath is <town>/.runtime/tiertrigger/state.json, the one file the tier
// sweep reads and writes.
func StatePath(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "tiertrigger", StateFileName)
}

// ErrNoRun reports a run whose start state was never saved.
var ErrNoRun = errors.New("no record of how the run began")

// Run is the checkout as it stood when the sweep asked which packages to run:
// the commit the run began at, and the tracked files that already differed
// from it. A record reads it to tell a tree that was dirty before the run (so
// the run tested something other than the commit) from files the run itself
// dirtied, which say nothing about what it tested (om, gt-e69b2).
type Run struct {
	Commit string   `json:"commit"`
	Dirty  []string `json:"dirty,omitempty"`
}

// RunPath is <town>/.runtime/tiertrigger/<tier>.run.json, the run's start state.
// tier is a plain name — commands reject anything else before it reaches here.
func RunPath(townRoot, tier string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "tiertrigger", tier+".run.json")
}

// SaveRun writes r to path through f.
func SaveRun(f Files, path string, r Run) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := f.WriteFile(path, append(data, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// LoadRun reads the run at path. A missing file is ErrNoRun: a run nobody
// recorded the start of cannot be stamped as tested.
func LoadRun(f Files, path string) (Run, error) {
	data, err := f.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Run{}, fmt.Errorf("%s: %w", path, ErrNoRun)
	}
	if err != nil {
		return Run{}, fmt.Errorf("read %s: %w", path, err)
	}
	var r Run
	if err := json.Unmarshal(data, &r); err != nil {
		return Run{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if r.Commit == "" {
		return Run{}, fmt.Errorf("parse %s: no commit", path)
	}
	return r, nil
}

// Record is one package's last passing container run: the commit its tests ran
// at, and when.
type Record struct {
	Commit string    `json:"commit"`
	At     time.Time `json:"at"`
}

// State is the per-package record the state file holds, keyed by package name.
type State map[string]Record

// ErrNoRecord reports a package whose container tests have never run.
var ErrNoRecord = errors.New("no container run recorded")

// Ran records pkg's container tests passing at commit at when.
func (st State) Ran(pkg, commit string, when time.Time) {
	st[pkg] = Record{Commit: commit, At: when}
}

// Changed returns the paths that changed since pkg's recorded run: the commits
// since it, and the checkout's uncommitted changes, which the container tests
// build exactly as they build HEAD, so a change nobody has committed there is
// still code they read.
//
// A recorded commit that is not an object id is an error rather than a git
// argument: the state file is input, and a record holding a flag or a revision
// expression must not reach git's command line (om, gt-e69b2).
func (st State) Changed(g Git, pkg string) ([]string, error) {
	rec, ok := st[pkg]
	if !ok {
		return nil, fmt.Errorf("%s: %w", pkg, ErrNoRecord)
	}
	if !objectID(rec.Commit) {
		return nil, fmt.Errorf("%s: recorded commit %q is not an object id", pkg, rec.Commit)
	}
	paths, err := g.DiffNameOnly(rec.Commit, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("%s since %s: %w", pkg, rec.Commit, err)
	}
	dirty, err := g.Worktree()
	if err != nil {
		return nil, fmt.Errorf("%s: read the working tree: %w", pkg, err)
	}
	return append(paths, dirty...), nil
}

// objectID reports whether c spells a git object id: hex, 40 characters for a
// SHA-1 repository or 64 for a SHA-256 one.
func objectID(c string) bool {
	switch len(c) {
	case 40, 64:
	default:
		return false
	}
	for _, r := range c {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// Git is the repository the due decision reads.
type Git interface {
	DiffNameOnly(base, head string) ([]string, error)
	// Worktree lists the repo-relative paths with uncommitted changes, tracked
	// or not.
	Worktree() ([]string, error)
}

// Package is a container-backed package and the paths whose change runs it.
type Package struct {
	Name     string
	Triggers []string
}

// Due returns the packages whose container tests run in a sweep at now: those
// with no recorded run, those whose daily window has passed since their last
// run, and those with a change on a trigger path since their recorded commit.
// The result is in pkgs' order. An empty state runs every package, so a
// missing state file runs every package. A record whose commit can no longer
// be diffed — history rewritten or garbage-collected — runs its package too,
// since unreadable is not unchanged and one stale record must not stop the
// sweep (gt-42nss). Those packages are also returned in unreadable with the
// diff error, so the caller can name them: a persistently broken repository
// otherwise turns every package into a full run with no signal (om, gt-42nss).
//
// loc is the zone the daily window is read in and must not be nil.
func Due(st State, pkgs []Package, g Git, now time.Time, loc *time.Location) (due []string, unreadable map[string]error) {
	unreadable = map[string]error{}
	for _, pkg := range pkgs {
		rec, ok := st[pkg.Name]
		if !ok || DailyRunDue(rec.At, now, loc) {
			due = append(due, pkg.Name)
			continue
		}
		changed, err := st.Changed(g, pkg.Name)
		if err != nil {
			unreadable[pkg.Name] = err
			due = append(due, pkg.Name)
			continue
		}
		if Runs(pkg.Triggers, changed, false) {
			due = append(due, pkg.Name)
		}
	}
	return due, unreadable
}

// Files is the file interface the state file is read and written through.
type Files interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte) error
}

// Store is the state file at Path, read and written through Files.
type Store struct {
	Path  string
	Files Files
}

// Load returns the state file's records. A missing file is an empty state; a
// file that does not parse is an error, never a reset to empty, because a
// reset would spend a full container run on a writer's bug.
func (s Store) Load() (State, error) {
	data, err := s.Files.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.Path, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.Path, err)
	}
	if st == nil {
		st = State{}
	}
	for pkg, rec := range st {
		if rec.Commit == "" || rec.At.IsZero() {
			return nil, fmt.Errorf("parse %s: %s has no commit or time", s.Path, pkg)
		}
	}
	return st, nil
}

// Save replaces the state file with st.
func (s Store) Save(st State) error {
	if st == nil {
		st = State{}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := s.Files.WriteFile(s.Path, append(data, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", s.Path, err)
	}
	return nil
}

// OSFiles is the real filesystem, with the state file written atomically.
type OSFiles struct{}

// ReadFile returns the file at path.
func (OSFiles) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// WriteFile writes path atomically, creating its directory.
func (OSFiles) WriteFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return atomicfile.WriteFile(path, data, 0o644)
}
