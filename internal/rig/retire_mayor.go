package rig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// RetireFS is the filesystem the retire-mayor step reads and renames through.
// Unit tests fake it; the command passes OSRetireFS.
type RetireFS interface {
	// Exists reports whether path exists, following a symlink. A missing path is
	// (false, nil).
	Exists(path string) (bool, error)
	// Lstat reports whether a directory entry exists at path without following a
	// symlink, so a dangling symlink exists. isDir is set only for a real directory.
	Lstat(path string) (exists, isDir bool, err error)
	// Canonical returns path with its symlinks resolved. A path that does not
	// exist resolves to its cleaned spelling.
	Canonical(path string) (string, error)
	// ReadFile returns the file's content. A missing file is an error that
	// matches fs.ErrNotExist.
	ReadFile(path string) ([]byte, error)
	// SubDirs returns the names of the directories directly under path, skipping
	// dot names. A missing path has none.
	SubDirs(path string) ([]string, error)
	// Rename moves oldPath to newPath.
	Rename(oldPath, newPath string) error
}

// OSRetireFS is the RetireFS over the real filesystem.
type OSRetireFS struct{}

// Exists implements RetireFS.
func (OSRetireFS) Exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Lstat implements RetireFS.
func (OSRetireFS) Lstat(path string) (bool, bool, error) {
	info, err := os.Lstat(path)
	if err == nil {
		return true, info.IsDir(), nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}
	return false, false, err
}

// Canonical implements RetireFS.
func (OSRetireFS) Canonical(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return filepath.Clean(path), nil
	}
	return "", err
}

// ReadFile implements RetireFS.
func (OSRetireFS) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // G304: path is built from the town root and a rig name
}

// SubDirs implements RetireFS.
func (OSRetireFS) SubDirs(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// Rename implements RetireFS.
func (OSRetireFS) Rename(oldPath, newPath string) error {
	return os.Rename(oldPath, newPath)
}

// RetirePlan is what the retire-mayor step does to one rig: rename From to To.
type RetirePlan struct {
	// From is the rig's mayor/rig directory.
	From string
	// To is the rename destination, mayor/rig.retired-<YYYYMMDD>.
	To string
	// AlreadyRetired is set when the rig directory has no mayor/rig. Nothing is
	// renamed and that is not an error.
	AlreadyRetired bool
	// Refusals name each reason the rename must not happen, each with the path
	// that caused it. An empty list with AlreadyRetired unset means the rename is
	// safe to apply.
	Refusals []string

	// townRoot, rigName and clock are the inputs PlanRetireMayor was given, so
	// Apply can plan again from the same place.
	townRoot string
	rigName  string
	clock    func() time.Time
}

// Undo is the mv that reverses the rename.
func (p RetirePlan) Undo() string {
	return "mv " + p.To + " " + p.From
}

// Apply renames From to To. It plans again before the rename and refuses if the
// new plan has a refusal or no longer matches this one: a redirect or route
// written after the plan must not be renamed over. os.Rename replaces an empty
// directory silently on POSIX, so the destination is checked in that plan too.
func (p RetirePlan) Apply(fsys RetireFS) error {
	if p.AlreadyRetired {
		return nil
	}
	if p.rigName == "" {
		return fmt.Errorf("refusing to rename %s: the plan was not made by PlanRetireMayor", p.From)
	}
	if len(p.Refusals) > 0 {
		return refusalError(p.From, p.Refusals)
	}
	fresh, err := PlanRetireMayor(fsys, p.townRoot, p.rigName, p.clock)
	if err != nil {
		return fmt.Errorf("planning again before the rename of %s: %w", p.From, err)
	}
	if fresh.AlreadyRetired || fresh.To != p.To {
		return fmt.Errorf("the plan for %s is stale: re-run the dry run; nothing renamed", p.From)
	}
	if len(fresh.Refusals) > 0 {
		return refusalError(p.From, fresh.Refusals)
	}
	return fsys.Rename(p.From, p.To)
}

// refusalError is the error for a rename that a refusal stops, naming each one.
func refusalError(from string, refusals []string) error {
	return fmt.Errorf("refusing to rename %s: %s", from, strings.Join(refusals, "; "))
}

// PlanRetireMayor reads what retiring rigName's mayor/rig would do under townRoot.
// It writes nothing. The date in the destination name is the local date of clock.
// A rig name with no directory under townRoot is an error, not a retired rig.
// A read it cannot complete is an error, never a clean plan.
func PlanRetireMayor(fsys RetireFS, townRoot, rigName string, clock func() time.Time) (RetirePlan, error) {
	if rigName == "" || rigName == "." || rigName == ".." || strings.ContainsAny(rigName, `/\`) {
		return RetirePlan{}, fmt.Errorf("invalid rig name %q", rigName)
	}
	rigPath := filepath.Join(townRoot, rigName)
	rigExists, rigIsDir, err := fsys.Lstat(rigPath)
	if err != nil {
		return RetirePlan{}, fmt.Errorf("checking rig %s at %s: %w", rigName, rigPath, err)
	}
	if !rigExists || !rigIsDir {
		return RetirePlan{}, fmt.Errorf("rig %s: no directory %s", rigName, rigPath)
	}
	from := constants.RigMayorPath(rigPath)
	plan := RetirePlan{
		From:     from,
		To:       from + ".retired-" + clock().Local().Format("20060102"),
		townRoot: townRoot,
		rigName:  rigName,
		clock:    clock,
	}

	present, _, err := fsys.Lstat(from)
	if err != nil {
		return RetirePlan{}, fmt.Errorf("checking %s: %w", from, err)
	}
	if !present {
		plan.AlreadyRetired = true
		return plan, nil
	}

	taken, _, err := fsys.Lstat(plan.To)
	if err != nil {
		return RetirePlan{}, fmt.Errorf("checking %s: %w", plan.To, err)
	}
	if taken {
		plan.Refusals = append(plan.Refusals, fmt.Sprintf("destination %s already exists", plan.To))
	}

	beadsDir := constants.RigBeadsWritePath(rigPath)
	hasMetadata, err := fsys.Exists(filepath.Join(beadsDir, "metadata.json"))
	if err != nil {
		return RetirePlan{}, fmt.Errorf("checking %s: %w", beadsDir, err)
	}
	if !hasMetadata {
		plan.Refusals = append(plan.Refusals, fmt.Sprintf(
			"%s has no metadata.json, so the beads are still in %s",
			beadsDir, constants.LegacyRigBeadsPath(rigPath)))
	}

	refusals, err := redirectRefusals(fsys, rigPath, from)
	if err != nil {
		return RetirePlan{}, err
	}
	plan.Refusals = append(plan.Refusals, refusals...)

	refusals, err = routeRefusals(fsys, townRoot, rigName, from)
	if err != nil {
		return RetirePlan{}, err
	}
	plan.Refusals = append(plan.Refusals, refusals...)
	return plan, nil
}

// redirectRefusals names each beads redirect in the rig's own .beads and in its
// crew and polecat worktrees whose target resolves into from.
func redirectRefusals(fsys RetireFS, rigPath, from string) ([]string, error) {
	rigName := filepath.Base(rigPath)
	workDirs := []string{rigPath}
	crew, err := fsys.SubDirs(filepath.Join(rigPath, constants.DirCrew))
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", filepath.Join(rigPath, constants.DirCrew), err)
	}
	for _, name := range crew {
		workDirs = append(workDirs, filepath.Join(rigPath, constants.DirCrew, name))
	}
	polecats, err := fsys.SubDirs(filepath.Join(rigPath, constants.DirPolecats))
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", filepath.Join(rigPath, constants.DirPolecats), err)
	}
	for _, name := range polecats {
		// A polecat's worktree is polecats/<name>/<rig>; the flat layout is
		// polecats/<name>. Either may hold a .beads directory.
		dir := filepath.Join(rigPath, constants.DirPolecats, name)
		workDirs = append(workDirs, filepath.Join(dir, rigName), dir)
	}

	var refusals []string
	for _, dir := range workDirs {
		redirect := filepath.Join(dir, constants.DirBeads, "redirect")
		data, err := fsys.ReadFile(redirect)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", redirect, err)
		}
		target := strings.TrimSpace(string(data))
		if target == "" {
			continue
		}
		resolved := filepath.Clean(target)
		if !filepath.IsAbs(target) {
			resolved = filepath.Join(dir, target)
		}
		if isWithin(resolved, from) {
			refusals = append(refusals, fmt.Sprintf("%s redirects to %s", redirect, resolved))
		}
	}
	return refusals, nil
}

// routeRefusals names each routes.jsonl line whose path resolves to this rig's
// mayor/rig: to the canonical directory from, or to a path ending in
// <rig>/mayor/rig. A route to another rig's mayor/rig is not a refusal. A line
// that does not parse is an error: it is not known to be clean.
func routeRefusals(fsys RetireFS, townRoot, rigName, from string) ([]string, error) {
	routesPath := filepath.Join(townRoot, constants.DirBeads, "routes.jsonl")
	data, err := fsys.ReadFile(routesPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", routesPath, err)
	}
	fromCanon, err := fsys.Canonical(from)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", from, err)
	}
	suffix := filepath.Join(rigName, constants.DirMayor, constants.DirRig)
	var refusals []string
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var route struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(line), &route); err != nil {
			return nil, fmt.Errorf("%s:%d: not a route line: %w", routesPath, i+1, err)
		}
		if route.Path == "" {
			return nil, fmt.Errorf("%s:%d: route line has no path", routesPath, i+1)
		}
		abs := route.Path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(townRoot, abs)
		}
		resolved, err := fsys.Canonical(abs)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: resolving %s: %w", routesPath, i+1, route.Path, err)
		}
		if resolved == fromCanon || endsWithPath(resolved, suffix) {
			refusals = append(refusals, fmt.Sprintf("%s:%d routes to %s", routesPath, i+1, from))
		}
	}
	return refusals, nil
}

// isWithin reports whether path is dir or lies under it.
func isWithin(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// endsWithPath reports whether path is suffix or ends with it at a separator.
func endsWithPath(path, suffix string) bool {
	return path == suffix || strings.HasSuffix(path, string(filepath.Separator)+suffix)
}
