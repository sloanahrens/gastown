package doctor

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
)

// StaleAgentBeadsCheck detects agent beads that exist in the database but have
// no corresponding agent on disk. This catches beads inherited from upstream or
// left over after crew members are removed.
//
// Checks crew worker beads and polecat agent beads. Polecats have persistent
// identity (agent beads survive nuke cycles), so stale detection applies to them too.
//
// Also detects orphaned agent beads from deregistered rigs — beads whose prefix
// doesn't match any route in routes.jsonl. These accumulate when a rig is removed
// via gt rig remove but its agent beads in the town database are not cleaned up.
//
// The fix closes stale beads so they no longer pollute bd ready output.
type StaleAgentBeadsCheck struct {
	FixableCheck
}

// NewStaleAgentBeadsCheck creates a new stale agent beads check.
func NewStaleAgentBeadsCheck() *StaleAgentBeadsCheck {
	return &StaleAgentBeadsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "stale-agent-beads",
				CheckDescription: "Detect agent beads for removed workers (crew and polecats)",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks for agent beads that have no matching agent on disk.
// agentWispsFromStore returns the agent beads among bd's wisps, keyed by ID. A
// *beads.Beads keeps its bd read, so a bd rig's answer is unchanged. Any other
// store reads its wisps through the Client seam, the read a tracker rig
// answers, and a failed read is returned rather than read as no wisps: the
// rig's list would otherwise be partial and look clean.
func agentWispsFromStore(bd doctorBeads) (map[string]*beads.Issue, error) {
	if b, ok := bd.(*beads.Beads); ok {
		return b.ListAgentBeadsFromWisps()
	}
	wisps, err := bd.List(beads.ListOptions{Ephemeral: true, Priority: -1})
	if err != nil {
		return nil, err
	}
	return beads.AgentBeadsFromWisps(wisps), nil
}

func (c *StaleAgentBeadsCheck) Run(ctx *CheckContext) *CheckResult {
	// Load routes to get prefixes
	beadsDir := filepath.Join(ctx.TownRoot, ".beads")
	routes, err := beads.LoadRoutes(beadsDir)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Could not load routes.jsonl",
		}
	}

	// Build prefix -> rigInfo map from routes
	prefixToRig := make(map[string]rigInfo)
	knownPrefixes := make(map[string]bool) // all known prefixes including town-level
	for _, r := range routes {
		parts := strings.Split(r.Path, "/")
		if len(parts) >= 1 && parts[0] != "." {
			rigName := parts[0]
			prefix := strings.TrimSuffix(r.Prefix, "-")
			prefixToRig[prefix] = rigInfo{
				name:      rigName,
				beadsPath: r.Path,
			}
			knownPrefixes[prefix] = true
		} else {
			// Town-level route (path ".") — track prefix but don't add to prefixToRig
			prefix := strings.TrimSuffix(r.Prefix, "-")
			knownPrefixes[prefix] = true
		}
	}

	var stale []string
	// readErrs records each worker directory (crew or polecats) that exists
	// but could not be read, so its rig's beads were skipped. The result
	// reports them rather than counting them as clean. skippedRigs names
	// each such rig once, so a rig with two unreadable directories counts as
	// one.
	// Phase 1 appends from one goroutine per rig; Phase 1's appends and
	// Phase 2's both go through addReadErr, so no append races the others.
	var readErrs []string
	skippedRigs := make(map[string]bool)
	// queryErrs records each store whose agent bead list could not be read,
	// so a Dolt outage is reported rather than read as a clean town
	// (gt-szdhc).
	var queryErrs []string
	var mu sync.Mutex
	addReadErr := func(rig string, err error) {
		mu.Lock()
		defer mu.Unlock()
		skippedRigs[rig] = true
		readErrs = append(readErrs, err.Error())
	}
	addQueryErr := func(store string, err error) {
		mu.Lock()
		defer mu.Unlock()
		queryErrs = append(queryErrs, store+": "+err.Error())
	}

	// Phase 1: Check known rigs for stale crew/polecat beads.
	// Run rigs in parallel — each rig spawns 2 bd subprocesses (List +
	// ListAgentBeadsFromWisps). With 6 rigs sequentially, that's 12 calls;
	// under Dolt memory pressure each can take 30s of retry, so the check
	// would block the whole doctor for ~6 minutes. Parallelizing collapses
	// the wall-clock time to roughly the slowest single rig.
	// Investigation: dc-1pq8 (forensic report 2026-05-02).
	var wg sync.WaitGroup
	for prefix, info := range prefixToRig {
		wg.Add(1)
		go func(prefix string, info rigInfo) {
			defer wg.Done()

			rigBeadsPath := filepath.Join(ctx.TownRoot, info.beadsPath)
			bd := ctx.beadsAt(rigBeadsPath)
			rigName := info.name

			// Get actual crew workers on disk. Like the polecats directory below,
			// a crew directory that exists but cannot be read is not an empty
			// rig: its crew beads are left out of the stale set (gt-ro1lc).
			crewDiskWorkers, crewErr := listCrewWorkers(ctx.TownRoot, rigName)
			crewDiskSet := make(map[string]bool, len(crewDiskWorkers))
			for _, w := range crewDiskWorkers {
				crewDiskSet[w] = true
			}

			// Get actual polecats on disk. A directory that exists but cannot be
			// read is not an empty rig: its polecat beads are left out of the
			// stale set rather than closed for live workers (gt-9wiz3).
			polecatDiskWorkers, polecatErr := listPolecats(ctx.TownRoot, rigName)
			polecatDiskSet := make(map[string]bool, len(polecatDiskWorkers))
			for _, w := range polecatDiskWorkers {
				polecatDiskSet[w] = true
			}

			// Agent bead ID patterns:
			// Crew:    prefix-rig-crew-name (or prefix-crew-name when prefix == rig)
			// Polecat: prefix-rig-polecat-name (or prefix-polecat-name when prefix == rig)
			crewPrefix := beads.AgentBeadIDWithPrefix(prefix, rigName, constants.RoleCrew, "") + "-"
			polecatPrefix := beads.AgentBeadIDWithPrefix(prefix, rigName, constants.RolePolecat, "") + "-"
			allBeads, err := bd.List(beads.ListOptions{
				Status:   "open",
				Priority: -1,
				Label:    "gt:agent",
			})
			if err != nil {
				// A store that cannot answer is not an empty store: record
				// the rig and leave its beads out of the stale set.
				addQueryErr(rigName, err)
				return
			}

			// Also check wisps table for migrated agent beads. A failed read
			// leaves this rig's list partial, so the rig is left out of the
			// stale set like one whose bead list failed above.
			wispMap, wispErr := agentWispsFromStore(bd)
			if wispErr != nil {
				addQueryErr(rigName, wispErr)
				return
			}
			for _, w := range wispMap {
				if w.Status == "open" || w.Status == "in_progress" || w.Status == "hooked" {
					allBeads = append(allBeads, w)
				}
			}

			var rigStale []string
			for _, issue := range allBeads {
				switch {
				case strings.HasPrefix(issue.ID, crewPrefix):
					if crewErr != nil {
						continue
					}
					workerName := strings.TrimPrefix(issue.ID, crewPrefix)
					if workerName != "" && !crewDiskSet[workerName] {
						rigStale = append(rigStale, issue.ID)
					}
				case strings.HasPrefix(issue.ID, polecatPrefix):
					if polecatErr != nil {
						continue
					}
					workerName := strings.TrimPrefix(issue.ID, polecatPrefix)
					if workerName != "" && !polecatDiskSet[workerName] {
						rigStale = append(rigStale, issue.ID)
					}
				}
			}

			if len(rigStale) > 0 {
				mu.Lock()
				stale = append(stale, rigStale...)
				mu.Unlock()
			}
			for _, readErr := range []error{crewErr, polecatErr} {
				if readErr != nil {
					addReadErr(rigName, readErr)
				}
			}
		}(prefix, info)
	}
	wg.Wait()

	// Phase 2: Detect orphaned agent beads from deregistered rigs.
	// Scan the town beads database for agent beads whose prefix doesn't match
	// any route in routes.jsonl. These accumulate when a rig is removed via
	// gt rig remove but its agent beads in the town database are not cleaned up.
	townBeadsPath := beads.GetTownBeadsPath(ctx.TownRoot)
	townBd := ctx.beadsAt(townBeadsPath)
	townAgents, townErr := townBd.ListAgentBeads()
	if townErr != nil {
		// Orphan detection did not run, so a clean result would be a lie.
		addQueryErr("town beads", townErr)
	}
	for id, issue := range townAgents {
		// Skip closed/non-active beads
		if issue.Status != "open" && issue.Status != "in_progress" && issue.Status != "hooked" {
			continue
		}
		// Only check beads that are agent type
		if issue.Type != "agent" {
			continue
		}
		// Parse the bead ID to extract its prefix
		rig, role, _, ok := beads.ParseAgentBeadID(id)
		if !ok {
			continue
		}
		// Skip town-level agents — they don't belong to a rig
		if rig == "" {
			continue
		}
		// Extract the prefix from the ID (everything before the first hyphen)
		hyphenIdx := strings.Index(id, "-")
		if hyphenIdx < 0 {
			continue
		}
		idPrefix := id[:hyphenIdx]
		// If prefix is not in any known route, this is an orphan from a deregistered rig
		if !knownPrefixes[idPrefix] {
			stale = append(stale, id)
			continue
		}
		// Also check: known prefix but the rig directory no longer exists on disk.
		// This catches beads stored in the town DB (hq) for rigs that still have
		// routes but whose crew/polecat workers were removed. The per-rig scan in
		// Phase 1 may miss these if the beads are in hq rather than the rig DB.
		if info, exists := prefixToRig[idPrefix]; exists {
			switch role {
			case constants.RoleCrew:
				workerName := parseCrewOrPolecatFromID(id, idPrefix, info.name, constants.RoleCrew)
				if workerName != "" {
					crewWorkers, err := listCrewWorkers(ctx.TownRoot, info.name)
					if err != nil {
						addReadErr(info.name, err)
						continue
					}
					found := false
					for _, w := range crewWorkers {
						if w == workerName {
							found = true
							break
						}
					}
					if !found {
						stale = append(stale, id)
					}
				}
			case constants.RolePolecat:
				workerName := parseCrewOrPolecatFromID(id, idPrefix, info.name, constants.RolePolecat)
				if workerName != "" {
					polecats, err := listPolecats(ctx.TownRoot, info.name)
					if err != nil {
						addReadErr(info.name, err)
						continue
					}
					found := false
					for _, p := range polecats {
						if p == workerName {
							found = true
							break
						}
					}
					if !found {
						stale = append(stale, id)
					}
				}
			}
		}
	}

	// Deduplicate stale list (Phase 1 and Phase 2 may find the same beads)
	stale = dedup(stale)
	readErrs = dedup(readErrs)
	queryErrs = dedup(queryErrs)

	if len(stale) == 0 && len(skippedRigs) == 0 && len(queryErrs) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No stale agent beads found",
		}
	}

	// Skipped rigs and failed queries are named in the message, never in
	// Details: Fix closes every entry in Details, and an error string there
	// is not a bead.
	if len(stale) == 0 {
		var reasons []string
		if len(queryErrs) > 0 {
			reasons = append(reasons, "Could not read agent bead lists: "+strings.Join(queryErrs, "; "))
		}
		if len(skippedRigs) > 0 {
			reasons = append(reasons, "Skipped rigs with an unreadable crew or polecats directory: "+strings.Join(readErrs, "; "))
		}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: strings.Join(reasons, "; "),
		}
	}

	message := fmt.Sprintf("%d stale agent bead(s) for removed workers", len(stale))
	if len(skippedRigs) > 0 {
		message += fmt.Sprintf("; %d rig(s) not checked (unreadable crew or polecats directory)", len(skippedRigs))
	}
	if len(queryErrs) > 0 {
		message += fmt.Sprintf("; could not read agent bead lists: %s", strings.Join(queryErrs, "; "))
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: message,
		Details: stale,
		FixHint: "Run 'gt doctor fix stale-agent-beads' to close stale agent beads",
	}
}

// parseCrewOrPolecatFromID extracts the worker name from a crew or polecat bead ID.
// Returns the worker name, or empty string if the ID doesn't match the expected pattern.
func parseCrewOrPolecatFromID(id, prefix, rigName, role string) string {
	// Build the expected prefix pattern: prefix-rig-role- or prefix-role- (collapsed)
	var idPrefix string
	if prefix == rigName {
		idPrefix = prefix + "-" + role + "-"
	} else {
		idPrefix = prefix + "-" + rigName + "-" + role + "-"
	}
	if strings.HasPrefix(id, idPrefix) {
		return strings.TrimPrefix(id, idPrefix)
	}
	return ""
}

// dedup removes duplicate strings from a slice, preserving order.
func dedup(s []string) []string {
	if len(s) == 0 {
		return s
	}
	seen := make(map[string]bool, len(s))
	result := make([]string, 0, len(s))
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			result = append(result, v)
		}
	}
	return result
}

// DestructiveFix marks this repair as destructive (gt-638go.3): it closes tracker beads.
func (c *StaleAgentBeadsCheck) DestructiveFix() bool { return true }

// Fix closes stale agent beads for crew members that no longer exist on disk.
// For beads with known prefixes, closes via the rig's beads client.
// For orphan beads from deregistered rigs (unknown prefix), closes via the
// town beads client since that's where they were found by Phase 2 detection.
func (c *StaleAgentBeadsCheck) Fix(ctx *CheckContext) error {
	// Re-run detection to get current stale list
	result := c.Run(ctx)
	if result.Status == StatusOK {
		return nil
	}

	// Load routes to get beads paths
	beadsDir := filepath.Join(ctx.TownRoot, ".beads")
	routes, err := beads.LoadRoutes(beadsDir)
	if err != nil {
		return fmt.Errorf("loading routes.jsonl: %w", err)
	}

	// Build prefix -> beads path map
	prefixToPath := make(map[string]string)
	for _, r := range routes {
		parts := strings.Split(r.Path, "/")
		if len(parts) >= 1 && parts[0] != "." {
			prefix := strings.TrimSuffix(r.Prefix, "-")
			prefixToPath[prefix] = filepath.Join(ctx.TownRoot, r.Path)
		}
	}

	// Town beads client as fallback for orphan beads from deregistered rigs
	townBeadsPath := beads.GetTownBeadsPath(ctx.TownRoot)
	townBd := ctx.beadsAt(townBeadsPath)

	// Close each stale bead
	closedStatus := "closed"
	var errs []error
	for _, beadID := range result.Details {
		// Determine which rig's beads client to use based on bead ID prefix
		var bd doctorBeads
		for prefix, path := range prefixToPath {
			if strings.HasPrefix(beadID, prefix+"-") {
				bd = ctx.beadsAt(path)
				break
			}
		}
		if bd == nil {
			// Unknown prefix — orphan from deregistered rig. These beads
			// live in the town database (hq), so close them there.
			bd = townBd
		}

		if err := bd.Update(beadID, beads.UpdateOptions{
			Status: &closedStatus,
		}); err != nil {
			errs = append(errs, fmt.Errorf("closing stale bead %s: %w", beadID, err))
		}
	}

	return errors.Join(errs...)
}
