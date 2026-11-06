package townstate

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

// RecentWindow is how recently a session-classed path under a daemon subsystem
// name must have changed for verify to mention it. It is information, never a
// failure: file metadata cannot say who wrote a file.
const RecentWindow = 24 * time.Hour

// maxSessionDepth bounds the walk under a session entry, so a crew worktree's
// checkout does not fill the report.
const maxSessionDepth = 3

// Problem is one town root path that does not match the split.
type Problem struct {
	Path   string
	Reason string
	// Info marks a finding that is not a failure: something worth a look that
	// metadata alone cannot judge. Only a Problem without Info makes verify fail.
	Info bool
}

// String renders the problem as "<path>: <reason>", with "info: " in front of
// an informational finding.
func (p Problem) String() string {
	if p.Info {
		return "info: " + p.Path + ": " + p.Reason
	}
	return p.Path + ": " + p.Reason
}

// Failures counts the problems that are not informational.
func Failures(problems []Problem) int {
	n := 0
	for _, p := range problems {
		if !p.Info {
			n++
		}
	}
	return n
}

// Verify checks the town root under fsys against the split, reading metadata
// only, and returns the problems it found sorted by path.
//
// A path FAILS when no entry covers it, or when two entries overlap with
// different owners; both are facts about the manifest and the tree, not about
// who wrote what. A recent change under a session entry, below a name the
// manifest gives a daemon subsystem (.runtime/<bird>/usage), is reported as
// INFO only: metadata cannot say who wrote a file, and a live session's own
// work changes files all day, so recency is never grounds for failing. Who
// writes a path is held by the guard tests (internal/daemon and
// internal/townstate), which read the code. .dolt-data* and .git trees are
// skipped at any depth, and so is anything more than maxSessionDepth below a
// session entry.
func Verify(fsys fs.FS, m *Manifest, now time.Time) ([]Problem, error) {
	if m == nil {
		return nil, errors.New("townstate: nil manifest")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	rigs, birds, err := townInputs(fsys, m)
	if err != nil {
		return nil, err
	}
	entries := m.Expand(rigs, birds)
	daemonNames := daemonSubsystemNames(entries)
	problems := overlaps(entries)
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == "." {
			return nil
		}
		if skipped(p) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		entry, ok := Lookup(entries, p)
		if !ok {
			if IsContainer(entries, p) {
				return nil
			}
			problems = append(problems, Problem{
				Path:   p,
				Reason: "no manifest entry; classify it daemon, session or shared",
			})
			return nil
		}
		if entry.Owner != OwnerSession {
			return nil
		}
		if depthBelow(entry.Path, p) > maxSessionDepth {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		first := firstSegmentBelow(entry.Path, p)
		if first == "" {
			return nil
		}
		if !daemonNames[first] {
			// Nothing below a directory whose first name is not a daemon
			// subsystem can be one, so the walk does not descend into it.
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := fs.Stat(fsys, p)
		if err != nil {
			return fmt.Errorf("stat %s: %w", p, err)
		}
		if now.Sub(info.ModTime()) <= RecentWindow {
			problems = append(problems, Problem{
				Path: p,
				Info: true,
				Reason: fmt.Sprintf("below the session entry %s, named like a daemon subsystem, and changed in the last %s; "+
					"it may be a live session's own write, and if the daemon wrote it the path is daemon-owned", entry.Path, RecentWindow),
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking the town root: %w", err)
	}
	return sortedProblems(problems), nil
}

// townInputs names the rigs and birds the town has, from directory metadata.
// A rig is a top-level directory with a config.json that the manifest does not
// name itself: a town-level directory such as settings/ carries a config.json
// too, and the manifest is what tells the two apart.
func townInputs(fsys fs.FS, m *Manifest) (rigs, birds []string, err error) {
	tops, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, nil, fmt.Errorf("reading the town root: %w", err)
	}
	for _, d := range tops {
		if !d.IsDir() {
			continue
		}
		if m.names(d.Name()) {
			continue
		}
		if _, err := fs.Stat(fsys, path.Join(d.Name(), "config.json")); err == nil {
			rigs = append(rigs, d.Name())
		}
	}
	birds = BirdRuntimeNames()
	runtimeDirs, err := fs.ReadDir(fsys, ".runtime")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("reading .runtime: %w", err)
	}
	for _, d := range runtimeDirs {
		if !d.IsDir() || slices.Contains(birds, d.Name()) {
			continue
		}
		if _, err := fs.Stat(fsys, path.Join(".runtime", d.Name(), "reports")); err == nil {
			birds = append(birds, d.Name())
		}
	}
	return rigs, birds, nil
}

// skipped reports whether p is a subtree verify never reads: a Dolt data
// directory or any git tree, at any depth.
func skipped(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".dolt-data") || seg == ".git" {
			return true
		}
	}
	return false
}

// depthBelow is how many path segments separate p from the entry at base.
func depthBelow(base, p string) int {
	if p == base {
		return 0
	}
	return strings.Count(strings.TrimPrefix(p, base+"/"), "/") + 1
}

// firstSegmentBelow is the first path segment of p below the entry at base, or
// "" when p is the entry itself.
func firstSegmentBelow(base, p string) string {
	rest, ok := strings.CutPrefix(p, base+"/")
	if !ok {
		return ""
	}
	first, _, _ := strings.Cut(rest, "/")
	return first
}

// daemonSubsystemNames returns the names of the daemon-owned directories
// directly under .runtime, such as usage for .runtime/usage.
func daemonSubsystemNames(entries []Entry) map[string]bool {
	names := map[string]bool{}
	for _, e := range entries {
		if e.Owner != OwnerDaemon {
			continue
		}
		if name, ok := strings.CutPrefix(e.Path, ".runtime/"); ok && name != "" && !strings.Contains(name, "/") {
			names[name] = true
		}
	}
	return names
}

// overlaps reports each pair of entries where one covers the other and they
// disagree on the owner.
func overlaps(entries []Entry) []Problem {
	var problems []Problem
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			a, b := entries[i], entries[j]
			if a.Owner == b.Owner || !(covers(a.Path, b.Path) || covers(b.Path, a.Path)) {
				continue
			}
			deeper, shallower := a, b
			if strings.Count(b.Path, "/") > strings.Count(a.Path, "/") {
				deeper, shallower = b, a
			}
			problems = append(problems, Problem{
				Path: deeper.Path,
				Reason: fmt.Sprintf("overlaps %s (%s) with a different owner (%s)",
					shallower.Path, shallower.Owner, deeper.Owner),
			})
		}
	}
	return problems
}

// covers reports whether base is p or one of its ancestors.
func covers(base, p string) bool {
	return p == base || strings.HasPrefix(p, base+"/")
}

// sortedProblems orders problems by path, then reason, and drops repeats of
// the same pair seen from a shallower entry.
func sortedProblems(problems []Problem) []Problem {
	slices.SortFunc(problems, func(a, b Problem) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return strings.Compare(a.Reason, b.Reason)
	})
	return slices.Compact(problems)
}
