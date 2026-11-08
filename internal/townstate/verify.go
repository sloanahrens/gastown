package townstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/manifestdata"
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
// only (plus the rig names the registry holds), and returns the problems it
// found sorted by path. A path under no entry is reported once, at the
// topmost unclassified directory, never once per descendant.
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
	daemonNames := manifestdata.DaemonSubsystemNames(entries)
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
			if d.IsDir() {
				// No entry names p or an ancestor, and none lies below it, so
				// nothing under p can be classified either: p is one problem.
				return fs.SkipDir
			}
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

// The rig registry is mayor/rigs.json on the five-file layout, and the
// "registry" section of machineFile on the two-file layout (internal/config
// layout.go). A host that carries the section wins over a leftover file.
const (
	machineFile     = config.MachineConfigFile
	rigsFile        = "mayor/" + constants.FileRigsJSON
	registrySection = "registry"
)

// rigMarkers are the paths whose presence marks a top-level directory as a
// rig: its registry record, its bare repository and its canonical clone.
var rigMarkers = []string{
	"config.json",
	constants.DirRepoGit,
	constants.DirMayor + "/" + constants.DirRig,
}

// townInputs names the rigs and birds the town has, from the rig registry and
// directory metadata. A rig is a top-level directory the registry names, or one
// that holds a rig marker; a name the manifest gives itself is never a rig,
// since a town-level directory such as settings/ can carry a config.json too.
// A registry or marker that cannot be read is an error, never an empty rig
// list: that would classify every rig's tree as unclassified.
func townInputs(fsys fs.FS, m *Manifest) (rigs, birds []string, err error) {
	registered, err := registryRigs(fsys)
	if err != nil {
		return nil, nil, err
	}
	tops, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, nil, fmt.Errorf("reading the town root: %w", err)
	}
	for _, d := range tops {
		if !d.IsDir() || m.names(d.Name()) {
			continue
		}
		if slices.Contains(registered, d.Name()) {
			rigs = append(rigs, d.Name())
			continue
		}
		marked, err := hasRigMarker(fsys, d.Name())
		if err != nil {
			return nil, nil, err
		}
		if marked {
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

// registryRigs returns the rig names the town's registry holds, read the way
// the daemon reads it: the "registry" section of mayor/town.json when the file
// carries one, else mayor/rigs.json. A town with neither has no rigs yet, the
// same as config.LoadRigsConfigOrEmpty. A registry that cannot be read or
// decoded is an error. Only the names are kept.
// A null registry section is an error too: it is not a registry.
func registryRigs(fsys fs.FS) ([]string, error) {
	top, err := readMachineTopLevel(fsys)
	if err != nil {
		return nil, err
	}
	var cfg config.RigsConfig
	if raw, ok := top[registrySection]; ok {
		// Decoding null yields zero rigs, which would hide mayor/rigs.json
		// without a word, so a null section is refused instead.
		if string(raw) == "null" {
			return nil, fmt.Errorf("reading %s: section %q is null, not an object", machineFile, registrySection)
		}
		if err := config.DecodeJSONFile(machineFile+` (section "`+registrySection+`")`, raw, &cfg); err != nil {
			return nil, err
		}
		return slices.Sorted(maps.Keys(cfg.Rigs)), nil
	}
	data, err := fs.ReadFile(fsys, rigsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", rigsFile, err)
	}
	if err := config.DecodeJSONFile(rigsFile, data, &cfg); err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(cfg.Rigs)), nil
}

// settingsRigs names the rigs the town registers, from the settings the landing
// worker and the daemon read: the registry the copy's rig rows expand to. Export
// and import use this list and only this list; a top-level directory that merely
// holds a config.json is not a rig to them, so its contents never become
// daemon-owned. Verify keeps its own, broader classification through
// townInputs (gt-gg9hv.68).
func settingsRigs(fsys fs.FS) ([]string, error) {
	return registryRigs(fsys)
}

// readMachineTopLevel returns the top-level keys of mayor/town.json, nil when
// the file is absent. A file that is not a JSON object is an error: verify
// cannot tell whether it carries the registry.
func readMachineTopLevel(fsys fs.FS) (map[string]json.RawMessage, error) {
	data, err := fs.ReadFile(fsys, machineFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", machineFile, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		// The decoder's message quotes the file's bytes, so it is not kept.
		return nil, fmt.Errorf("reading %s: not a JSON object", machineFile)
	}
	// JSON null decodes to a nil map with no error, and it is not an object.
	if top == nil {
		return nil, fmt.Errorf("reading %s: not a JSON object", machineFile)
	}
	return top, nil
}

// hasRigMarker reports whether the top-level directory top holds one of the
// rig markers. A marker that cannot be stat'd for a reason other than its
// absence is an error, never a missing rig.
func hasRigMarker(fsys fs.FS, top string) (bool, error) {
	for _, marker := range rigMarkers {
		p := path.Join(top, marker)
		_, err := fs.Stat(fsys, p)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, fs.ErrNotExist):
		default:
			return false, fmt.Errorf("stat %s: %w", p, err)
		}
	}
	return false, nil
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
