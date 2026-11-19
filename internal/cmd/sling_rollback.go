package cmd

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// slingRollback is what undoing a partial sling reaches outside the
// process: the town, bd, and the seams that release the work and remove the
// sandbox. realSlingRollbackIn wires the caller's town (or the cwd's), the bd
// on PATH and the package seams; tests build one with an in-process bd and
// fakes, so they need no stub on PATH, no chdir and no package-global swap.
type slingRollback struct {
	townRoot string
	townErr  error // set when the cwd is not in a town
	// stores opens the work bead's databases; the zero value is bd.
	stores slingStores

	getBeadInfo      func(beadID string) (*beadInfo, error)
	collectMolecules func(info *beadInfo) []string
	burnMolecules    func(molecules []string, beadID, townRoot string) error
	releaseSeat      func(*SpawnedPolecatInfo)
	newReleaser      func(townRoot, hookWorkDir string) polecatWorkReleaser
	survivingWork    func(townRoot, beadID string) (string, error)
	openSandbox      func(townRoot, rigName string) (spawnedPolecatSandbox, error)

	// log collects the rollback's decision lines. Nil writes them to the
	// console; unit tests collect them instead of swapping process stdio.
	log func(format string, args ...any)
}

// say writes one line of the rollback's output.
func (s slingRollback) say(format string, args ...any) {
	if s.log != nil {
		s.log(format, args...)
		return
	}
	fmt.Printf(format, args...)
}

// warnf writes one warning line.
func (s slingRollback) warnf(format string, args ...any) {
	s.say("  %s "+format+"\n", append([]any{style.Dim.Render("Warning:")}, args...)...)
}

// realSlingRollbackIn is the rollback of the running gt: the caller's town
// root (falling back to the cwd's), bd on PATH, and the package seams (which
// tests of other paths still replace).
//
// The town root is a parameter rather than a lookup because the dispatch
// engine runs from the daemon as well as from the cobra command, and the
// daemon's cwd is not the town it dispatches into.
func realSlingRollbackIn(townRoot string, err error, stores slingStores) slingRollback {
	if townRoot == "" && err == nil {
		townRoot, err = workspace.FindFromCwdOrError()
	}
	return slingRollback{
		townRoot:         townRoot,
		townErr:          err,
		stores:           stores,
		getBeadInfo:      func(beadID string) (*beadInfo, error) { return getBeadInfoForRollback(stores, beadID) },
		collectMolecules: collectExistingMoleculesForRollback,
		burnMolecules: func(molecules []string, beadID, townRoot string) error {
			return burnExistingMoleculesForRollback(stores, molecules, beadID, townRoot)
		},
		releaseSeat: func(spawn *SpawnedPolecatInfo) { spawn.releaseSeatClaim() },
		newReleaser: func(townRoot, hookWorkDir string) polecatWorkReleaser {
			return newPolecatWorkReleaserFn(stores, townRoot, hookWorkDir)
		},
		survivingWork: survivingWorkForBeadFn,
		openSandbox:   openSpawnedPolecatSandboxFn,
	}
}

// rollback is rollbackSlingArtifacts on this context.
func (s slingRollback) rollback(spawnInfo *SpawnedPolecatInfo, beadID, hookWorkDir string) {
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
	//
	// No seat claim to drop when there is no spawn: a claim belongs to a spawn
	// and is dropped by its rollback, so a failure before the spawn has none
	// to give back.
	if spawnInfo == nil {
		return
	}
	s.cleanupSpawned(spawnInfo, spawnInfo.RigName, beadID, hookWorkDir)
}

// cleanupSpawned is cleanupSpawnedPolecatWork on this context.
func (s slingRollback) cleanupSpawned(spawnInfo *SpawnedPolecatInfo, rigName, beadID, hookWorkDir string) {
	if spawnInfo == nil {
		return
	}

	// Everything below is written for a seat only this sling holds. The spawn
	// wrote this sling's bead into the seat's agent bead (hook_bead, state
	// spawning); a sling that claimed the seat since overwrote it with its own,
	// and releasing the claim, resetting the slot or removing the sandbox and
	// branch under it would destroy that sling's work (gt-7izlwj). So read the
	// agent bead before touching anything.
	//
	// The read and the release below are two steps, not one atomic step: a
	// sling can claim the seat in between. No lock spans the two — the window
	// is only the time between two calls of one rollback, and it is accepted.
	spawnedFor := spawnInfo.HookBead
	if spawnedFor == "" {
		spawnedFor = beadID
	}
	heldBead, found, err := s.seatHeldBead(rigName, spawnInfo.PolecatName)
	switch {
	case err != nil:
		// An unreadable agent bead does not say who holds the seat, so its
		// slot and branch are not this sling's to undo. The seat claim still
		// goes back: no session will ever exist for this spawn, and a leaked
		// claim shadows a free seat until its TTL (gt-t8q5). releaseSeatClaim
		// drops only this spawn's own claim file and needs no town root.
		s.releaseSeat(spawnInfo)
		s.warnf("Could not read %s's agent bead to see who holds its seat: %v; leaving its slot and branch alone",
			spawnInfo.AgentID(), err)
		return
	case found && heldBead != "" && heldBead != spawnedFor:
		not := spawnedFor
		if not == "" {
			not = "the bead this sling spawned for"
		}
		s.say("  %s Leaving %s's seat to the sling that holds it: its hook names %s, not %s\n",
			style.Dim.Render("○"), spawnInfo.PolecatName, heldBead, not)
		return
	}

	// The spawn's seat claim goes with the spawn: no session will ever exist
	// for this polecat, so the seat it reserved must not stay reserved. This is
	// the one path every caller-side failure after a spawn comes through —
	// returning before the cleanup below, which is best-effort (gt-t8q5).
	s.releaseSeat(spawnInfo)

	townRoot := s.townRoot

	// Give the work back first: the sandbox removal below resets a fresh
	// polecat's agent bead, and a kept sandbox needs its slot reset here.
	// Work that survives on a branch goes back to its pre-sling holder rather
	// than being released (the shared work-survival rule).
	rel := s.newReleaser(townRoot, hookWorkDir)
	restored, restoreErr := restoreOriginalHoldIfWorkSurvivesWith(rel, s.survivingWork, townRoot, spawnInfo.AgentID(), beadID, spawnInfo.originalHold)
	if restoreErr != nil {
		// The restore failed, so the bead is still hooked to the polecat being
		// removed. Fall through and release it instead of dropping it from
		// release and leaving it wedged (gt-34z9v).
		fmt.Printf("  %s Restore failed, releasing instead: %v\n", style.Dim.Render("Warning:"), restoreErr)
	}
	if restored {
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
}

// seatHeldBead is the bead the agent bead of seat rigName/polecatName says its
// hook holds. found is false when the seat has no agent bead: it names no
// other claimant, so the rollback has nothing to leave alone. An agent bead
// that cannot be read is an error, never "no claimant".
func (s slingRollback) seatHeldBead(rigName, polecatName string) (hookBead string, found bool, err error) {
	if s.townErr != nil {
		return "", false, s.townErr
	}
	agentBeadID := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(s.townRoot, rigName), rigName, polecatName)
	_, fields, err := beads.GetAgentBead(s.stores.pinnedAt(resolveBeadDirFromTownRoot(s.townRoot, agentBeadID)), agentBeadID)
	if err != nil {
		return "", false, err
	}
	if fields == nil {
		return "", false, nil
	}
	return strings.TrimSpace(fields.HookBead), true, nil
}
