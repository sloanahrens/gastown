package cmd

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// slingRollback is what undoing a partial sling reaches outside the
// process: the town, bd, and the seams that release the work and remove the
// sandbox. realSlingRollback wires the cwd's town, the bd on PATH and the
// package seams; tests build one with an in-process bd and fakes, so they
// need no stub on PATH, no chdir and no package-global swap.
type slingRollback struct {
	townRoot string
	townErr  error // set when the cwd is not in a town
	// stores opens the work bead's databases; the zero value is bd.
	stores slingStores
	// townBeads is the town database an auto-convoy is closed in; nil is
	// bd pinned to the town's .beads.
	townBeads beads.Client

	getBeadInfo      func(beadID string) (*beadInfo, error)
	collectMolecules func(info *beadInfo) []string
	burnMolecules    func(molecules []string, beadID, townRoot string) error
	releaseSeat      func()
	newReleaser      func(townRoot, hookWorkDir string) polecatWorkReleaser
	survivingWork    func(townRoot, beadID string) (string, error)
	openSandbox      func(townRoot, rigName string) (spawnedPolecatSandbox, error)
}

// realSlingRollback is the rollback of the running gt: the cwd's town, bd on
// PATH, and the package seams (which tests of other paths still replace).
func realSlingRollback() slingRollback {
	townRoot, err := workspace.FindFromCwdOrError()
	return slingRollback{
		townRoot:         townRoot,
		townErr:          err,
		getBeadInfo:      getBeadInfoForRollback,
		collectMolecules: collectExistingMoleculesForRollback,
		burnMolecules:    burnExistingMoleculesForRollback,
		releaseSeat:      releasePoolSeatClaim,
		newReleaser:      newPolecatWorkReleaserFn,
		survivingWork:    survivingWorkForBeadFn,
		openSandbox:      openSpawnedPolecatSandboxFn,
	}
}

// rollback is rollbackSlingArtifacts on this context.
func (s slingRollback) rollback(spawnInfo *SpawnedPolecatInfo, beadID, hookWorkDir, convoyID string) {
	townRoot, err := s.townRoot, s.townErr

	// 1. Burn any attached molecules from partial formula instantiation.
	// This clears attached_molecule metadata and closes stale wisps that
	// otherwise block subsequent sling attempts.
	// Some failure modes happen before any bead is hooked (e.g., wisp creation fails).
	if beadID != "" {
		if err != nil {
			fmt.Printf("  %s Could not find workspace to rollback bead %s: %v\n", style.Dim.Render("Warning:"), beadID, err)
		} else {
			info, infoErr := s.getBeadInfo(beadID)
			if infoErr != nil {
				fmt.Printf("  %s Could not inspect bead %s for stale molecules: %v\n", style.Dim.Render("Warning:"), beadID, infoErr)
			} else {
				existingMolecules := s.collectMolecules(info)
				if depMolecules, depErr := s.stores.moleculeDeps(beadID, townRoot); depErr != nil {
					fmt.Printf("  %s Could not inspect canonical molecule bonds for %s: %v\n", style.Dim.Render("Warning:"), beadID, depErr)
				} else {
					existingMolecules = appendUniqueMolecules(existingMolecules, depMolecules...)
				}
				canClearWorkflowFields := len(existingMolecules) == 0
				if len(existingMolecules) > 0 {
					if burnErr := s.burnMolecules(existingMolecules, beadID, townRoot); burnErr != nil {
						fmt.Printf("  %s Could not burn stale molecule(s) from %s: %v\n", style.Dim.Render("Warning:"), beadID, burnErr)
					} else {
						fmt.Printf("  %s Burned %d stale molecule(s): %s\n",
							style.Dim.Render("○"), len(existingMolecules), strings.Join(existingMolecules, ", "))
						if refreshed, refreshErr := s.getBeadInfo(beadID); refreshErr != nil {
							fmt.Printf("  %s Could not refresh bead %s after molecule cleanup: %v\n", style.Dim.Render("Warning:"), beadID, refreshErr)
						} else {
							info = refreshed
							canClearWorkflowFields = true
						}
					}
				}
				if canClearWorkflowFields {
					if cleared, clearErr := s.stores.restoreRawWorkflowFields(beadID, townRoot, hookWorkDir, info, nil); clearErr != nil {
						fmt.Printf("  %s Could not clear raw workflow metadata from %s: %v\n", style.Dim.Render("Warning:"), beadID, clearErr)
					} else if cleared {
						fmt.Printf("  %s Cleared raw workflow metadata from %s\n", style.Dim.Render("○"), beadID)
					}
				}
			}
		}
	}

	// 2. Release the bead — only while it is still hooked to this polecat —
	// and undo the spawn: a fresh sandbox is removed, a reused one is kept with
	// its slot reset, and only a branch this sling created may go (gt-7evi4).
	if spawnInfo == nil {
		s.releaseSeat()
		return
	}
	s.cleanupSpawned(spawnInfo, spawnInfo.RigName, beadID, hookWorkDir, convoyID)
}

// cleanupSpawned is cleanupSpawnedPolecatWork on this context.
func (s slingRollback) cleanupSpawned(spawnInfo *SpawnedPolecatInfo, rigName, beadID, hookWorkDir, convoyID string) {
	// The spawn's seat claim goes with the spawn: no session will ever exist
	// for this polecat, so the seat it reserved must not stay reserved. This is
	// the one path every caller-side failure after a spawn comes through —
	// returning before the cleanup below, which is best-effort and gives up
	// early when the workspace or rig cannot be read (gt-t8q5).
	s.releaseSeat()

	if spawnInfo == nil {
		return
	}
	townRoot := s.townRoot
	if s.townErr != nil {
		return
	}

	// Give the work back first: the sandbox removal below resets a fresh
	// polecat's agent bead, and a kept sandbox needs its slot reset here.
	// Work that survives on a branch goes back to its pre-sling holder rather
	// than being released (the shared work-survival rule).
	rel := s.newReleaser(townRoot, hookWorkDir)
	if restoreOriginalHoldIfWorkSurvivesWith(rel, s.survivingWork, townRoot, spawnInfo.AgentID(), beadID, spawnInfo.originalHold) {
		beadID = ""
	}
	releasePolecatWork(rel, spawnInfo.AgentID(), beadID, !spawnInfo.FreshSpawn)

	if spawnInfo.FreshSpawn {
		if sandbox, err := s.openSandbox(townRoot, rigName); err != nil {
			fmt.Printf("  %s Could not open rig %s to clean up polecat %s: %v\n",
				style.Dim.Render("Warning:"), rigName, spawnInfo.PolecatName, err)
		} else {
			if err := sandbox.RemovePolecat(spawnInfo.PolecatName); err != nil {
				fmt.Printf("  %s Could not clean up orphaned polecat %s: %v\n",
					style.Dim.Render("Warning:"), spawnInfo.PolecatName, err)
			} else {
				fmt.Printf("  %s Cleaned up orphaned polecat %s\n",
					style.Dim.Render("○"), spawnInfo.PolecatName)
			}
			if spawnInfo.Branch != "" && spawnInfo.BranchCreated {
				sandbox.DeleteBranch(spawnInfo.Branch)
			}
		}
	} else {
		fmt.Printf("  %s Kept reused polecat %s (sandbox predates this sling)\n",
			style.Dim.Render("○"), spawnInfo.PolecatName)
	}
	if spawnInfo.Branch != "" && !spawnInfo.BranchCreated {
		fmt.Printf("  %s Kept branch %s (not created by this sling)\n",
			style.Dim.Render("○"), spawnInfo.Branch)
	}

	// Close the auto-convoy if one was created
	if convoyID != "" {
		s.closeConvoy(convoyID, "Sling rollback - hook failed")
	}
}

// closeConvoy closes an auto-created convoy in the town beads.
func (s slingRollback) closeConvoy(convoyID, reason string) {
	townRoot, err := s.townRoot, s.townErr
	if err != nil {
		fmt.Printf("  %s Could not find workspace to close convoy %s: %v\n", style.Dim.Render("Warning:"), convoyID, err)
		return
	}
	db := s.townBeads
	if db == nil {
		db = beads.NewPinned(filepath.Join(townRoot, ".beads"))
	}
	if err := db.CloseWithReason(reason, convoyID); err != nil {
		fmt.Printf("  %s Could not close convoy %s: %v\n", style.Dim.Render("Warning:"), convoyID, err)
	} else {
		fmt.Printf("  %s Closed convoy %s\n", style.Dim.Render("○"), convoyID)
	}
}
