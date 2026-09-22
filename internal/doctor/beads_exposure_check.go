package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/rig"
)

// BeadsExposureCheck verifies .beads/ is protected from `git clean -fd` in
// every clone across every rig.
//
// Protection can come from either a tracked .gitignore entry OR a per-clone
// .git/info/exclude entry — but info/exclude never propagates across clones
// (it's per-repository and is never committed), so whether a given clone is
// protected depends entirely on how it was provisioned. A clone missing
// this protection shows .beads/ as untracked in `git status`, one
// `git clean -fd` away from deleting the beads credential key, audit log,
// and backups (gt-ylpg).
//
// This check verifies EFFECTIVE ignore status per clone via `git status
// --ignored`, not by scanning .gitignore text — a clone protected only by
// info/exclude has no matching .gitignore line to find, and a stale
// .gitignore line can't prove anything about the clone's actual git state.
// beadProbes is the injected seam for beadsUntrackedAndUnignored so tests can
// exercise the pass/unknown split without fabricating broken git repos.
var beadProbes = beadsUntrackedAndUnignored

type BeadsExposureCheck struct {
	FixableCheck
	exposedClones    []string
	unresolvedClones []string
}

// NewBeadsExposureCheck creates a new .beads/ exposure check.
func NewBeadsExposureCheck() *BeadsExposureCheck {
	return &BeadsExposureCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "beads-exposure",
				CheckDescription: "Check that .beads/ is protected from git clean in every clone",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// Run checks every clone in every rig for an untracked, unignored .beads/ directory.
// A clone whose git state could not be interrogated is never reported as
// protected: a credential-exposure check that cannot prove protection must
// report StatusSkipped (unknown), not a pass.
func (c *BeadsExposureCheck) Run(ctx *CheckContext) *CheckResult {
	rigs := findAllRigs(ctx.TownRoot)
	if len(rigs) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rigs found",
		}
	}

	c.exposedClones = nil
	c.unresolvedClones = nil
	checked := 0
	seen := map[string]bool{}

	for _, rigPath := range rigs {
		for _, clonePath := range findBeadsClones(rigPath) {
			if seen[clonePath] {
				continue
			}
			seen[clonePath] = true
			if _, err := os.Stat(filepath.Join(clonePath, ".beads")); err != nil {
				continue // nothing to protect yet in this clone
			}
			checked++
			switch beadProbes(clonePath) {
			case probeExposed:
				c.exposedClones = append(c.exposedClones, clonePath)
			case probeUnresolved:
				c.unresolvedClones = append(c.unresolvedClones, clonePath)
			}
		}
	}

	if checked == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No .beads/ directories found in any clone",
		}
	}

	var details []string
	for _, clonePath := range c.exposedClones {
		details = append(details, fmt.Sprintf("%s: .beads/ is untracked and not gitignored — `git clean -fd` would delete it", relToTown(ctx, clonePath)))
	}
	for _, clonePath := range c.unresolvedClones {
		details = append(details, fmt.Sprintf("%s: git failed to report status — .beads/ protection UNKNOWN", relToTown(ctx, clonePath)))
	}

	if len(c.exposedClones) == 0 && len(c.unresolvedClones) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf(".beads/ protected in all %d checked clone(s)", checked),
		}
	}

	// Exposed clones prove a real exposure (warning, fixable); unresolved
	// clones only prove this check could not look (skipped — a skipped
	// check must never aggregate as a pass).
	if len(c.exposedClones) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d clone(s) have an unprotected .beads/ directory", len(c.exposedClones)),
			Details: details,
			FixHint: "Run 'gt doctor --fix' to add .beads/ to each clone's local git exclude",
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusSkipped,
		Message: fmt.Sprintf("could not determine .beads/ protection for %d clone(s) (git status failed)", len(c.unresolvedClones)),
		Details: details,
		FixHint: "Repair the affected clone(s) (corrupt .git, git missing, safe.directory refusal), then re-run 'gt doctor'",
	}
}

// relToTown renders clonePath relative to the town root for report details.
func relToTown(ctx *CheckContext, clonePath string) string {
	relPath, err := filepath.Rel(ctx.TownRoot, clonePath)
	if err != nil || relPath == "" {
		return clonePath
	}
	return relPath
}

// Fix adds .beads/ to the local git exclude file for each exposed clone.
func (c *BeadsExposureCheck) Fix(ctx *CheckContext) error {
	for _, clonePath := range c.exposedClones {
		if err := rig.EnsureLocalExcludePatterns(clonePath); err != nil {
			return fmt.Errorf("protecting .beads/ in %s: %w", clonePath, err)
		}
	}
	return nil
}

// probeResult is the outcome of interrogating one clone's git state for
// .beads/ exposure.
type probeResult int

const (
	probeProtected  probeResult = iota // git reports .beads/ as tracked or ignored
	probeExposed                       // git reports .beads/ as untracked (??)
	probeUnresolved                    // git failed to answer at all — unknown
)

// beadsUntrackedAndUnignored reports the exposure state of .beads/ in
// clonePath. Scoping the status query to the .beads pathspec means every "??"
// line in the output — whether the whole directory or individual files
// inside it — is a real exposure; a tracked or ignored .beads/ produces no
// such lines regardless of which mechanism (tracked exception, .gitignore,
// or info/exclude) is doing the protecting.
//
// A git failure (corrupt .git, safe.directory refusal, git missing) returns
// probeUnresolved, never probeProtected: a credential-exposure check whose
// purpose is catching exposure must not report a pass on evidence it could
// not actually gather (gt-whvu).
func beadsUntrackedAndUnignored(clonePath string) probeResult {
	cmd := exec.Command("git", "-C", clonePath, "status", "--porcelain", "--ignored", "--", ".beads")
	out, err := cmd.Output()
	if err != nil {
		return probeUnresolved
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "??") {
			return probeExposed
		}
	}
	return probeProtected
}

// findBeadsClones returns the clones in a rig that hold a .beads/ directory:
// the standard clones (mayor, refinery, crew, polecats) plus the witness
// agent's clone. witnessDir (internal/witness) prefers witness/rig/ for
// legacy witness clones and falls back to witness/ itself; either layout can
// hold a .beads/ (worktree-local or redirect-provisioned), so both are
// enumerated. findRigClones is deliberately left as-is — it is shared with
// the hooks-path check, and both paths are deduped by Run.
func findBeadsClones(rigPath string) []string {
	clones := findRigClones(rigPath)
	clones = append(clones,
		filepath.Join(rigPath, "witness", "rig"),
		filepath.Join(rigPath, "witness"),
	)
	return clones
}
