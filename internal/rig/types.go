// Package rig provides rig management functionality.
package rig

import (
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// Rig represents a managed repository in the workspace.
type Rig struct {
	// Name is the rig identifier (directory name).
	Name string `json:"name"`

	// Path is the absolute path to the rig directory.
	Path string `json:"path"`

	// GitURL is the remote repository URL (fetch/pull).
	GitURL string `json:"git_url"`

	// PushURL is an optional push URL for read-only upstreams.
	// When set, polecats push here instead of to GitURL (e.g., personal fork).
	PushURL string `json:"push_url,omitempty"`

	// LocalRepo is an optional local repository used for reference clones.
	LocalRepo string `json:"local_repo,omitempty"`

	// Config is the rig-level configuration.
	Config *config.BeadsConfig `json:"config,omitempty"`

	// Polecats is the list of polecat names in this rig.
	Polecats []string `json:"polecats,omitempty"`

	// Crew is the list of crew worker names in this rig.
	// Crew workers are user-managed persistent workspaces.
	Crew []string `json:"crew,omitempty"`

	// HasMayor indicates if the rig has a mayor clone.
	HasMayor bool `json:"has_mayor"`

	// BDRunner, when set, answers the bd calls this Rig makes itself (the
	// rig identity bead read behind the config lookups) in process instead
	// of the bd on PATH. Nil is the real bd. Tests of code that holds a Rig
	// set it; production leaves it nil.
	BDRunner beads.BDRunner `json:"-"`
}

// AgentDirs are the standard agent directories in a rig.
var AgentDirs = []string{
	"polecats",
	"crew",
	"mayor/rig",
}

// RigSummary provides a concise overview of a rig.
type RigSummary struct {
	Name         string `json:"name"`
	PolecatCount int    `json:"polecat_count"`
	CrewCount    int    `json:"crew_count"`
}

// Summary returns a RigSummary for this rig.
func (r *Rig) Summary() RigSummary {
	return RigSummary{
		Name:         r.Name,
		PolecatCount: len(r.Polecats),
		CrewCount:    len(r.Crew),
	}
}

// BeadsPath returns the path to use for beads operations.
// Always returns the rig root path where .beads/ contains either:
//   - A local beads database (when repo doesn't track .beads/)
//   - A redirect file pointing to mayor/rig/.beads (when repo tracks .beads/)
//
// The redirect is set up by initBeads() during rig creation and followed
// automatically by the bd CLI and beads.ResolveBeadsDir().
//
// This ensures we never write to the user's repo clone (mayor/rig/) and
// all beads operations go through the redirect system.
func (r *Rig) BeadsPath() string {
	return r.Path
}

// DefaultBranch returns the configured default branch for this rig.
// Falls back to "main" if not configured or if config cannot be loaded.
func (r *Rig) DefaultBranch() string {
	cfg, err := LoadRigConfig(r.Path)
	if err != nil || cfg.DefaultBranch == "" {
		return "main"
	}
	return cfg.DefaultBranch
}

// RepoPath returns the absolute path of the rig's git working clone, or ""
// when the rig has no repository checked out.
//
// The rig root is not usually a git worktree: `gt rig add` lays out the clones
// at <rig>/mayor/rig and <rig>/refinery/rig, while the rig root holds only the
// .beads/ redirect and the agent directories. Callers that need to run git in
// the rig's repository (plugins like gitignore-reconcile and git-hygiene, which
// enumerate rigs through `gt rig list --json`) need one of those clones — the
// rig root would make `git -C` resolve to the enclosing town repo instead.
//
// Candidates are tried in the same order as Manager.detectGitURL: the rig root
// first (legacy and adopted layouts check the repository out in place), then
// the mayor and refinery clones. A candidate only counts when it is the root of
// its own working tree; see isWorkTreeRoot for why that is stricter than
// `git rev-parse --git-dir`.
func (r *Rig) RepoPath() string {
	candidates := []string{
		r.Path,
		filepath.Join(r.Path, "mayor", "rig"),
		filepath.Join(r.Path, "refinery", "rig"),
	}
	for _, candidate := range candidates {
		if isWorkTreeRoot(candidate) {
			return candidate
		}
	}
	return ""
}

// isWorkTreeRoot reports whether path is the root of a git working tree.
//
// Both regular clones (.git directory) and linked worktrees (.git file pointing
// at the shared git directory) qualify, so a plain os.Stat is enough for both.
// Bare repositories do not qualify — they have no working tree to inspect — and
// neither does a directory that merely sits inside some other repository, which
// is the case that matters here: the town root is itself git-tracked, so
// `git -C <rig>` succeeds by upward search and reports the town repository,
// not the rig's.
func isWorkTreeRoot(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}
