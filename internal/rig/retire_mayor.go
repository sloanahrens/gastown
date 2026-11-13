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
	// Exists reports whether path exists. A missing path is (false, nil).
	Exists(path string) (bool, error)
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
	// AlreadyRetired is set when the rig has no mayor/rig directory. Nothing is
	// renamed and that is not an error.
	AlreadyRetired bool
	// Refusals name each reason the rename must not happen, each with the path
	// that caused it. An empty list with AlreadyRetired unset means the rename is
	// safe to apply.
	Refusals []string
}

// Undo is the mv that reverses the rename.
func (p RetirePlan) Undo() string {
	return "mv " + p.To + " " + p.From
}

// Apply renames From to To. It refuses a plan that has refusals, and it checks
// that To is still free: os.Rename replaces an empty directory silently on POSIX,
// so the plan's check alone is not enough.
func (p RetirePlan) Apply(fsys RetireFS) error {
	if p.AlreadyRetired {
		return nil
	}
	if len(p.Refusals) > 0 {
		return fmt.Errorf("refusing to rename %s: %d refusal(s) in the plan", p.From, len(p.Refusals))
	}
	taken, err := fsys.Exists(p.To)
	if err != nil {
		return fmt.Errorf("checking %s before the rename: %w", p.To, err)
	}
	if taken {
		return fmt.Errorf("refusing to rename %s: %s exists now", p.From, p.To)
	}
	return fsys.Rename(p.From, p.To)
}

// PlanRetireMayor reads what retiring rigName's mayor/rig would do under townRoot.
// It writes nothing. The date in the destination name is the local date of clock.
// A read it cannot complete is an error, never a clean plan.
func PlanRetireMayor(fsys RetireFS, townRoot, rigName string, clock func() time.Time) (RetirePlan, error) {
	if rigName == "" || rigName == "." || rigName == ".." || strings.ContainsAny(rigName, `/\`) {
		return RetirePlan{}, fmt.Errorf("invalid rig name %q", rigName)
	}
	rigPath := filepath.Join(townRoot, rigName)
	from := constants.RigMayorPath(rigPath)
	plan := RetirePlan{From: from, To: from + ".retired-" + clock().Local().Format("20060102")}

	present, err := fsys.Exists(from)
	if err != nil {
		return RetirePlan{}, fmt.Errorf("checking %s: %w", from, err)
	}
	if !present {
		plan.AlreadyRetired = true
		return plan, nil
	}

	taken, err := fsys.Exists(plan.To)
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

	refusals, err = routeRefusals(fsys, townRoot, from)
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

// routeRefusals names each routes.jsonl line in the town whose path resolves to
// from. A line that does not parse is an error: it is not known to be clean.
func routeRefusals(fsys RetireFS, townRoot, from string) ([]string, error) {
	routesPath := filepath.Join(townRoot, constants.DirBeads, "routes.jsonl")
	data, err := fsys.ReadFile(routesPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", routesPath, err)
	}
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
		resolved := filepath.Clean(route.Path)
		if !filepath.IsAbs(route.Path) {
			resolved = filepath.Join(townRoot, route.Path)
		}
		if resolved == from {
			refusals = append(refusals, fmt.Sprintf("%s:%d routes to %s", routesPath, i+1, from))
		}
	}
	return refusals, nil
}

// isWithin reports whether path is dir or lies under it.
func isWithin(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}
