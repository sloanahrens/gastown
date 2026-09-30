package doctor

import (
	"fmt"
	"strings"
)

// ForeignRemoteCheck detects git remotes in the town repo that point to
// unrelated repositories. The town repo (~/gt, tracking stevey-gt) should
// only have its own origin remote. Remotes for rig repos (gastown, beads)
// pollute the ref space with unrelated history and cause confusion — agents
// comparing branches across unrelated remotes see phantom divergence.
type ForeignRemoteCheck struct {
	FixableCheck
	foreignRemotes []foreignRemote // Cached during Run for use in Fix
}

type foreignRemote struct {
	name string
	url  string
}

// NewForeignRemoteCheck creates a new foreign remote check.
func NewForeignRemoteCheck() *ForeignRemoteCheck {
	return &ForeignRemoteCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "foreign-remotes",
				CheckDescription: "Detect git remotes pointing to unrelated repositories",
				CheckCategory:    CategoryCore,
			},
		},
	}
}

// Run checks if the town repo has remotes beyond origin that point to
// unrelated repositories (no shared commit ancestry with origin/main).
func (c *ForeignRemoteCheck) Run(ctx *CheckContext) *CheckResult {
	c.foreignRemotes = nil

	g := ctx.git(ctx.TownRoot)
	remotes, err := g.Remotes()
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not list git remotes",
			Details: []string{err.Error()},
		}
	}

	if len(remotes) <= 1 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No extra remotes configured",
		}
	}

	// For each non-origin remote, check if it shares history with origin
	for _, remote := range remotes {
		if remote == "origin" {
			continue
		}

		// Get remote URL for reporting
		url, _ := g.RemoteURL(remote)
		url = strings.TrimSpace(url)

		// Check if remote has a main/master branch we can compare
		refName := remote + "/main"
		if ok, _ := g.RefExists(refName); !ok {
			// Try master
			refName = remote + "/master"
			if ok, _ := g.RefExists(refName); !ok {
				// No main or master branch — can't verify, skip
				continue
			}
		}

		// Check for shared ancestry with origin/main
		if _, err := g.MergeBase("origin/main", refName); err != nil {
			// No common ancestor — this is a foreign remote
			c.foreignRemotes = append(c.foreignRemotes, foreignRemote{
				name: remote,
				url:  url,
			})
		}
	}

	if len(c.foreignRemotes) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "All remotes share history with origin",
		}
	}

	details := []string{
		"The town repo has remotes pointing to unrelated repositories.",
		"These pollute the ref space and cause phantom divergence in git status.",
		"Rig remotes belong in their own clones (mayor/rig, crew worktrees), not here.",
	}
	for _, fr := range c.foreignRemotes {
		details = append(details, fmt.Sprintf("  %s → %s", fr.name, fr.url))
	}

	names := make([]string, len(c.foreignRemotes))
	for i, fr := range c.foreignRemotes {
		names[i] = fr.name
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("Found %d foreign remote(s): %s", len(c.foreignRemotes), strings.Join(names, ", ")),
		Details: details,
		FixHint: "Run 'gt doctor --fix' to remove foreign remotes",
	}
}

// Fix removes foreign remotes from the town repo.
func (c *ForeignRemoteCheck) Fix(ctx *CheckContext) error {
	if len(c.foreignRemotes) == 0 {
		return nil
	}

	g := ctx.git(ctx.TownRoot)
	var errs []string
	for _, fr := range c.foreignRemotes {
		if err := g.RemoveRemote(fr.name); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", fr.name, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to remove some remotes: %s", strings.Join(errs, "; "))
	}
	return nil
}
