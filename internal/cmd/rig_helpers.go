package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/sling"
	"github.com/steveyegge/gastown/internal/workspace"
)

// checkRigNotParkedOrDocked checks if a rig is parked or docked and returns
// an error if so. This prevents starting agents on rigs that have been
// intentionally taken offline.
func checkRigNotParkedOrDocked(rigName string) error {
	townRoot, r, err := getRig(rigName)
	if err != nil {
		return err
	}

	if IsRigParked(townRoot, rigName) {
		return fmt.Errorf("rig '%s' is parked - use 'gt rig unpark %s' first", rigName, rigName)
	}

	prefix := "gt"
	if r.Config != nil && r.Config.Prefix != "" {
		prefix = r.Config.Prefix
	}

	if IsRigDocked(townRoot, rigName, prefix) {
		return fmt.Errorf("rig '%s' is docked - use 'gt rig undock %s' first", rigName, rigName)
	}

	return nil
}

// getRig finds the town root and retrieves the specified rig.
// This is the common boilerplate extracted from get*Manager functions.
// Returns the town root path and rig instance.
func getRig(rigName string) (string, *rig.Rig, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return "", nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	return getRigIn(townRoot, rigName)
}

// getRigIn is getRig in a known town.
func getRigIn(townRoot, rigName string) (string, *rig.Rig, error) {
	rigsConfigPath := constants.MayorRigsPath(townRoot)
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}

	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	r, err := rigMgr.GetRig(rigName)
	if err != nil {
		return "", nil, fmt.Errorf("rig '%s' not found", rigName)
	}

	return townRoot, r, nil
}

// findCurrentRig resolves the rig the caller is working in: the first path
// component of cwd under the town root, else GT_RIG (gt run from the town
// root through a shell alias).
func findCurrentRig(townRoot string) (string, *rig.Rig, error) {
	rigName, err := inferRigFromCwd(townRoot)
	if err != nil || rigName == "" {
		rigName = os.Getenv("GT_RIG")
	}
	if rigName == "" {
		return "", nil, fmt.Errorf("not inside a rig directory (and GT_RIG not set)")
	}
	_, r, err := getRig(rigName)
	if err != nil {
		return "", nil, err
	}
	return rigName, r, nil
}

// slingBlocked is why a sling into rigName must not run, or nil when it may.
// The guards themselves are sling.Blocked's: the dispatch engine runs them for
// the daemon's convoy feeder as well as for this command, and a second copy
// here is a rule the two could drift apart on.
func slingBlocked(townRoot, rigName string, estopOn func(townRoot, rigName string) (bool, error), parked func(townRoot, rigName string) (bool, string)) error {
	_, err := sling.Blocked(townRoot, rigName, estopOn, parked)
	return err
}

// IsRigParkedOrDocked checks if a rig is parked or docked. Returns
// (blocked, reason) with reason "parked" or "docked". This is the single
// entry point for all dispatch paths (sling, polecat spawn, the daemon,
// gt spec) to check rig availability.
//
// Parked is the rig's record in mayor/rigs.json, read through the config
// kernel, and fails closed: a rig whose park state cannot be read is parked
// (gt-y3pgh.4). Docked is the rig identity bead's status:docked label.
func IsRigParkedOrDocked(townRoot, rigName string) (bool, string) {
	if IsRigParked(townRoot, rigName) {
		return true, "parked"
	}

	// Look up the beads prefix from rigs.json (the rig registry), with fallback
	// to the rig's own config.json for isolated/test scenarios.
	rigPath := filepath.Join(townRoot, rigName)
	prefix := rigBeadsPrefix(townRoot, rigPath, rigName)
	if prefix == "" {
		return false, ""
	}

	beadsPath := filepath.Join(rigPath, "mayor", "rig")
	if _, err := os.Stat(beadsPath); err != nil {
		beadsPath = rigPath
	}

	bd := beads.New(beadsPath)
	rigBeadID := beads.RigBeadIDWithPrefix(prefix, rigName)
	rigBead, err := bd.Show(rigBeadID)
	if err != nil {
		return false, ""
	}

	for _, l := range rigBead.Labels {
		if l == RigDockedLabel {
			return true, "docked"
		}
	}

	return false, ""
}

func rigBeadsPrefix(townRoot, rigPath, rigName string) string {
	rigsConfigPath := constants.MayorRigsPath(townRoot)
	if rigsConfig, err := config.LoadRigsConfig(rigsConfigPath); err == nil {
		if entry, ok := rigsConfig.Rigs[rigName]; ok && entry.BeadsConfig != nil && entry.BeadsConfig.Prefix != "" {
			return entry.BeadsConfig.Prefix
		}
	}

	rigConfigPath := filepath.Join(rigPath, "config.json")
	if rigCfg, err := config.LoadRigConfig(rigConfigPath); err == nil && rigCfg.Beads != nil && rigCfg.Beads.Prefix != "" {
		return rigCfg.Beads.Prefix
	}

	return ""
}

// discoverRigsForTownRoot loads the rigs config for the given town root and
// returns all registered rigs. Callers that don't yet have a town root
// should use getAllRigs, which resolves it from the cwd first.
func discoverRigsForTownRoot(townRoot string) ([]*rig.Rig, error) {
	rigsConfig, err := config.LoadRigsConfig(constants.MayorRigsPath(townRoot))
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}

	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	return rigMgr.DiscoverRigs()
}

// autoInferRig returns the sole registered rig for a given townRoot, or an
// actionable error when the result is ambiguous. Callers use this when no
// --rig flag was provided and cwd-based detection found nothing (e.g. Deacon
// at HQ level on a non-default install where "gastown" rig does not exist).
func autoInferRig(townRoot string) (name, path string, err error) {
	rigs, err := discoverRigsForTownRoot(townRoot)
	if err != nil {
		return "", "", fmt.Errorf("cannot determine target rig: %w; use --rig=NAME", err)
	}

	switch len(rigs) {
	case 1:
		return rigs[0].Name, rigs[0].Path, nil
	case 0:
		return "", "", fmt.Errorf("cannot determine target rig: no rigs registered in this workspace; use --rig=NAME")
	default:
		names := make([]string, len(rigs))
		for i, r := range rigs {
			names[i] = r.Name
		}
		return "", "", fmt.Errorf("cannot determine target rig (available: %s); use --rig=NAME", strings.Join(names, ", "))
	}
}

// getAllRigs discovers all rigs in the current Gas Town workspace.
// Returns the list of rigs and any error.
func getAllRigs() ([]*rig.Rig, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	return discoverRigsForTownRoot(townRoot)
}

// rigCmdEnv is where a rig command finds its rig and writes its report. The
// cobra entry points use the cwd's town and stdout; tests give a town and a
// buffer, so they need neither a chdir nor a stdout swap.
type rigCmdEnv struct {
	findRig func(rigName string) (string, *rig.Rig, error)
	out     io.Writer
	errOut  io.Writer
}

func cwdRigCmdEnv() rigCmdEnv { return rigCmdEnv{findRig: getRig, out: os.Stdout, errOut: os.Stderr} }

func townRigCmdEnv(townRoot string, out, errOut io.Writer) rigCmdEnv {
	return rigCmdEnv{
		findRig: func(rigName string) (string, *rig.Rig, error) { return getRigIn(townRoot, rigName) },
		out:     out,
		errOut:  errOut,
	}
}
