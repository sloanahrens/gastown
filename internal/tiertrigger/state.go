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

// Changed returns the paths that changed since pkg's recorded run.
func (st State) Changed(g Git, pkg string) ([]string, error) {
	rec, ok := st[pkg]
	if !ok {
		return nil, fmt.Errorf("%s: %w", pkg, ErrNoRecord)
	}
	paths, err := g.DiffNameOnly(rec.Commit, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("%s since %s: %w", pkg, rec.Commit, err)
	}
	return paths, nil
}

// Git is the repository the due decision reads and a sweep records its tip
// through.
type Git interface {
	Rev(ref string) (string, error)
	DiffNameOnly(base, head string) ([]string, error)
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
// missing state file runs every package.
//
// loc is the zone the daily window is read in and must not be nil.
func Due(st State, pkgs []Package, g Git, now time.Time, loc *time.Location) ([]string, error) {
	var due []string
	for _, pkg := range pkgs {
		rec, ok := st[pkg.Name]
		if !ok || DailyRunDue(rec.At, now, loc) {
			due = append(due, pkg.Name)
			continue
		}
		changed, err := st.Changed(g, pkg.Name)
		if err != nil {
			return nil, err
		}
		if Runs(pkg.Triggers, changed, false) {
			due = append(due, pkg.Name)
		}
	}
	return due, nil
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
