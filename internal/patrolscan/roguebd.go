package patrolscan

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Rogue bd check, ported from the deacon patrol's rogue-bd-check step
// (gt-li4t, gt-5zsc, gt-19zn). A `bd` executable inside an agent worktree can
// shadow the town's bd release and migrate a production database behind the
// town's back. The check walks the agent aggregate directories to a bounded
// depth, sorts each executable `bd` into build output (by design) or a
// finding, and neutralizes findings: a regular file loses its execute bits,
// a symlink is removed (never followed: chmod through a link named bd could
// strip the town's shared release). A found binary is never executed.

// rogueBDMaxDepth bounds the walk below each aggregate directory, so it never
// descends into a repository's object store.
const rogueBDMaxDepth = 4

// RogueBDAggregates returns the directories the check walks under townRoot:
// every rig's polecats, crew, refinery, witness and mayor trees, the
// town-level mayor tree and the dogs.
func RogueBDAggregates(townRoot string) []string {
	var out []string
	for _, pat := range []string{"*/polecats", "*/crew", "*/refinery", "*/witness", "*/mayor"} {
		m, _ := filepath.Glob(filepath.Join(townRoot, pat))
		out = append(out, m...)
	}
	out = append(out, filepath.Join(townRoot, "mayor"), filepath.Join(townRoot, "deacon", "dogs"))
	sort.Strings(out)
	return out
}

// RogueBDVerdict classifies one candidate.
type RogueBDVerdict string

const (
	RogueBDByDesign    RogueBDVerdict = "by-design"   // git-ignored build output of a repo with cmd/bd
	RogueBDNeutralized RogueBDVerdict = "neutralized" // finding, execute bits removed or link removed
	RogueBDUnknown     RogueBDVerdict = "unknown"     // could not classify; left alone
	RogueBDFailed      RogueBDVerdict = "failed"      // finding, neutralization failed
)

// RogueBDFinding is one executable bd the walk found.
type RogueBDFinding struct {
	Path    string
	Symlink bool
	OnPath  bool
	Verdict RogueBDVerdict
	Detail  string
}

// RogueBDResult is one run of the check.
type RogueBDResult struct {
	Candidates []RogueBDFinding
	// WalkErrors are directories the walk could not read. A run with walk
	// errors is inconclusive, never "clean".
	WalkErrors []string
}

// Findings returns the candidates that are not by design.
func (r RogueBDResult) Findings() []RogueBDFinding {
	var out []RogueBDFinding
	for _, c := range r.Candidates {
		if c.Verdict != RogueBDByDesign {
			out = append(out, c)
		}
	}
	return out
}

// Clean reports a completed walk with nothing but build output.
func (r RogueBDResult) Clean() bool {
	return len(r.WalkErrors) == 0 && len(r.Findings()) == 0
}

// RogueBDOptions are the check's seams.
type RogueBDOptions struct {
	// PathDirs are the directories on PATH; a bd in one of them is always a
	// finding, whatever repo it sits in.
	PathDirs []string
	// IsBuildOutput reports whether path is git-ignored build output of a
	// worktree that carries cmd/bd. An error leaves the candidate alone.
	IsBuildOutput func(path string) (bool, error)
	// DryRun reports findings without neutralizing them.
	DryRun bool
}

// CheckRogueBD walks the aggregate directories and handles every executable
// bd found. A missing aggregate is skipped.
func CheckRogueBD(aggregates []string, o RogueBDOptions) RogueBDResult {
	var r RogueBDResult
	onPath := map[string]bool{}
	for _, d := range o.PathDirs {
		if d != "" {
			onPath[filepath.Clean(d)] = true
		}
	}
	seen := map[string]bool{}
	for _, agg := range aggregates {
		info, err := os.Stat(agg)
		if err != nil || !info.IsDir() {
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				r.WalkErrors = append(r.WalkErrors, fmt.Sprintf("%s: %v", agg, err))
			}
			continue
		}
		base := strings.Count(filepath.Clean(agg), string(filepath.Separator))
		walkErr := filepath.WalkDir(agg, func(path string, de fs.DirEntry, err error) error {
			if err != nil {
				r.WalkErrors = append(r.WalkErrors, fmt.Sprintf("%s: %v", path, err))
				if de != nil && de.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			depth := strings.Count(filepath.Clean(path), string(filepath.Separator)) - base
			if de.IsDir() {
				if de.Name() == ".git" || depth >= rogueBDMaxDepth {
					return fs.SkipDir
				}
				return nil
			}
			if de.Name() != "bd" || seen[path] {
				return nil
			}
			seen[path] = true
			if c, ok := classifyBD(path, de, onPath, o); ok {
				r.Candidates = append(r.Candidates, c)
			}
			return nil
		})
		if walkErr != nil {
			r.WalkErrors = append(r.WalkErrors, fmt.Sprintf("%s: %v", agg, walkErr))
		}
	}
	return r
}

// classifyBD handles one file named bd. It reports false for a regular file
// without the owner execute bit (already neutralized).
func classifyBD(path string, de fs.DirEntry, onPath map[string]bool, o RogueBDOptions) (RogueBDFinding, bool) {
	c := RogueBDFinding{Path: path, OnPath: onPath[filepath.Dir(path)]}
	if de.Type()&fs.ModeSymlink != 0 {
		c.Symlink = true
		return neutralize(c, o), true
	}
	info, err := de.Info()
	if err != nil {
		c.Verdict, c.Detail = RogueBDUnknown, "stat: "+err.Error()
		return c, true
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o100 == 0 {
		return c, false
	}
	if c.OnPath {
		return neutralize(c, o), true
	}
	if o.IsBuildOutput != nil {
		built, err := o.IsBuildOutput(path)
		if err != nil {
			c.Verdict, c.Detail = RogueBDUnknown, "build-output check failed: "+err.Error()
			return c, true
		}
		if built {
			c.Verdict = RogueBDByDesign
			return c, true
		}
	}
	return neutralize(c, o), true
}

// neutralize removes a symlink or clears a file's execute bits.
func neutralize(c RogueBDFinding, o RogueBDOptions) RogueBDFinding {
	if o.DryRun {
		c.Verdict, c.Detail = RogueBDFailed, "dry run: not neutralized"
		return c
	}
	if c.Symlink {
		// Lstat again: act only on the link itself, never its target.
		if fi, err := os.Lstat(c.Path); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			c.Verdict, c.Detail = RogueBDUnknown, "changed under the check; left alone"
			return c
		}
		if err := os.Remove(c.Path); err != nil {
			c.Verdict, c.Detail = RogueBDFailed, "remove symlink: "+err.Error()
			return c
		}
		c.Verdict, c.Detail = RogueBDNeutralized, "symlink removed"
		return c
	}
	if err := os.Chmod(c.Path, 0o644); err != nil {
		c.Verdict, c.Detail = RogueBDFailed, "chmod 644: "+err.Error()
		return c
	}
	c.Verdict, c.Detail = RogueBDNeutralized, "chmod 644"
	return c
}
