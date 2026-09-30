package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// AgentBeadsCheck verifies that agent beads exist for all agents.
// This includes:
// - Global agents (mayor) - stored in town beads with hq- prefix
// - Crew workers and polecats - stored in each rig's beads
//
// Agent beads are created by gt rig add (see gt-h3hak, gt-pinkq) and gt crew add.
// Each rig uses its configured prefix (e.g., "gt-" for gastown, "bd-" for beads).
type AgentBeadsCheck struct {
	FixableCheck
}

// NewAgentBeadsCheck creates a new agent beads check.
func NewAgentBeadsCheck() *AgentBeadsCheck {
	return &AgentBeadsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "agent-beads-exist",
				CheckDescription: "Verify agent beads exist for all agents",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// rigInfo holds the rig name and its beads path from routes.
type rigInfo struct {
	name      string // rig name (first component of path)
	beadsPath string // full path to beads directory relative to town root
}

// agentBeadScope holds the known agent beads of a single beads database.
// Existence checks are per-database: rig agents must exist in their rig's
// database and town agents in the town database, because agent lookups
// (gt agents resolve --rig) require rig-local agent beads (gt-abj). Merging
// databases into one map would let a town-level duplicate mask a missing
// rig-local bead.
type agentBeadScope struct {
	issues map[string]*beads.Issue // from issues table (has labels)
	wisps  map[string]bool         // from wisps table (ID only)
}

// loadAgentBeadScope loads agent beads and wisp IDs from one beads database.
// Agent beads are ephemeral (stored in wisps), but we also check issues for
// backward compatibility. The wisps list doesn't include type/labels, so wisp
// IDs are tracked separately for existence checks.
func loadAgentBeadScope(bd *beads.Beads) agentBeadScope {
	scope := agentBeadScope{
		issues: make(map[string]*beads.Issue),
		wisps:  make(map[string]bool),
	}
	if agents, err := bd.ListAgentBeads(); err == nil {
		for id, issue := range agents {
			scope.issues[id] = issue
		}
	}
	if wispIDs, _ := bd.ListWispIDs(); wispIDs != nil {
		for id := range wispIDs {
			scope.wisps[id] = true
		}
	}
	return scope
}

// Run checks if agent beads exist for all expected agents.
func (c *AgentBeadsCheck) Run(ctx *CheckContext) *CheckResult {
	// Load routes to get prefixes (routes.jsonl is source of truth for prefixes)
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
	// Routes have format: prefix "gt-" -> path "gastown/mayor/rig" or "my-saas"
	prefixToRig := make(map[string]rigInfo) // prefix (without hyphen) -> rigInfo
	for _, r := range routes {
		// Extract rig name from path (first component)
		parts := strings.Split(r.Path, "/")
		if len(parts) >= 1 && parts[0] != "." {
			rigName := parts[0]
			if ctx.RigName != "" && rigName != ctx.RigName {
				continue
			}
			prefix := strings.TrimSuffix(r.Prefix, "-")
			prefixToRig[prefix] = rigInfo{
				name:      rigName,
				beadsPath: r.Path, // Use the full route path
			}
		}
	}

	var missing []string
	var missingLabel []string
	var checked int

	// Load known agent beads PER DATABASE. Existence is scoped: town agents
	// (mayor) must exist in the town database, and rig agents
	// (crew, polecats) must exist in their rig's database.
	// Agent lookups (gt agents resolve --rig) hard-require rig-local agent
	// beads, so a town-level duplicate must not satisfy a rig check (gt-abj).
	townBeadsPath := beads.GetTownBeadsPath(ctx.TownRoot)
	townScope := loadAgentBeadScope(ctx.beadsRigLocal(townBeadsPath))

	rigScopes := make(map[string]agentBeadScope) // key: prefix
	for prefix, info := range prefixToRig {
		rigBeadsPath := filepath.Join(ctx.TownRoot, info.beadsPath)
		rigScopes[prefix] = loadAgentBeadScope(ctx.beadsRigLocal(rigBeadsPath))
	}

	// checkAgentBead verifies an agent bead exists in the database it is
	// required to live in (issues or wisps table). Label checking only applies
	// to beads found in the issues table (wisps don't expose labels in their
	// list output). Beads that exist only in the town database are annotated
	// so operators understand why doctor flags them despite bd show finding them.
	checkAgentBead := func(scope agentBeadScope, id string) {
		if issue, exists := scope.issues[id]; exists {
			// Found in issues table — check label
			if !beads.HasLabel(issue, "gt:agent") {
				missingLabel = append(missingLabel, id)
			}
		} else if !scope.wisps[id] {
			// Not in the required database's issues or wisps
			if _, inTown := townScope.issues[id]; inTown || townScope.wisps[id] {
				missing = append(missing, id+" (exists only in town beads; rig-local bead required)")
			} else {
				missing = append(missing, id)
			}
		}
		checked++
	}

	// Check global agents (Mayor)
	mayorID := beads.MayorBeadIDTown()

	checkAgentBead(townScope, mayorID)

	if len(prefixToRig) == 0 {
		// No rigs to check, but we still checked global agents
		if len(missing) == 0 && len(missingLabel) == 0 {
			return &CheckResult{
				Name:    c.Name(),
				Status:  StatusOK,
				Message: fmt.Sprintf("All %d agent beads exist with gt:agent label", checked),
			}
		}
		details := append(missing, missingLabel...)
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("%d agent bead(s) missing, %d missing gt:agent label", len(missing), len(missingLabel)),
			Details: details,
			FixHint: "Run 'gt doctor --fix' to create missing agent beads and add labels",
		}
	}

	// Check each rig for its agents — against that rig's own database only.
	for prefix, info := range prefixToRig {
		rigName := info.name
		rigScope := rigScopes[prefix]

		// Check crew worker agents (canonical naming: prefix-rig-role-name)
		crewWorkers := listCrewWorkers(ctx.TownRoot, rigName)
		for _, workerName := range crewWorkers {
			crewID := beads.CrewBeadIDWithPrefix(prefix, rigName, workerName)
			checkAgentBead(rigScope, crewID)
		}

		// Check polecat agents
		polecatWorkers := listPolecats(ctx.TownRoot, rigName)
		for _, polecatName := range polecatWorkers {
			polecatID := beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName)
			checkAgentBead(rigScope, polecatID)
		}
	}

	if len(missing) == 0 && len(missingLabel) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("All %d agent beads exist with gt:agent label", checked),
		}
	}

	if len(missing) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("%d agent bead(s) missing", len(missing)),
			Details: missing,
			FixHint: "Run 'gt doctor --fix' to create missing agent beads",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d agent bead(s) missing gt:agent label", len(missingLabel)),
		Details: missingLabel,
		FixHint: "Run 'gt doctor --fix' to add missing labels",
	}
}

// Fix creates missing agent beads and adds gt:agent labels to beads missing them.
// Existence is checked per database (see agentBeadScope): a rig agent bead that
// exists only in the town database is treated as missing and recreated in its
// rig's database, so agent lookups that require rig-local beads work (gt-abj).
func (c *AgentBeadsCheck) Fix(ctx *CheckContext) error {
	// Collect errors instead of failing on first — one broken rig shouldn't
	// block fixes for all other rigs.
	var errs []error

	// Fix global agents (Mayor) in town beads.
	// NewRigLocal pins each wrapper to its own database. Spawn paths now
	// create agent beads rig-local via canonical prefix routing (gt-8we),
	// but doctor keeps the explicit pin: the fix must target a SPECIFIC
	// database chosen from routes, independent of resolution heuristics
	// (the historical bug: a routed wrapper re-targeted the town DB, the
	// create succeeded as an upsert there, and fixed nothing — gt-8po).
	townBeadsPath := beads.GetTownBeadsPath(ctx.TownRoot)
	townBd := ctx.beadsRigLocal(townBeadsPath)

	// Pre-load known agent bead IDs for the town database (from both issues
	// and wisps tables) so existence checks don't need per-bead Show() calls
	// that miss ephemeral wisps.
	townScope := loadAgentBeadScope(townBd)

	// fixAgentBead ensures an agent bead exists and is open in the database
	// it is required to live in (scope must be loaded from the same database
	// as bd).
	// Logic:
	//   1. If in issues table → ensure gt:agent label
	//   2. If in wisps table (open) → ensure gt:agent label
	//   3. If exists but closed → REOPEN it (don't recreate)
	//   4. If truly missing → CREATE it
	// Uses CreateAgentBead which creates durable agent beads (not wisps)
	// so they survive wisp GC (GH#2768).
	// workDir is the rig directory for direct SQL fallback when bd update
	// fails silently (e.g., legacy prefixes that can't be routed — GH#2127).
	fixAgentBead := func(bd *beads.Beads, scope agentBeadScope, workDir, id, desc string, fields *beads.AgentFields) error {
		// Check issues table first
		if issue, exists := scope.issues[id]; exists {
			// In issues table — ensure it has the gt:agent label.
			if !beads.HasLabel(issue, "gt:agent") {
				return ensureAgentLabel(ctx, bd, workDir, id)
			}
			return nil
		}

		// Check wisps table (only open wisps are listed)
		if scope.wisps[id] {
			// Exists as open wisp — ensure it has gt:agent label
			// (ListWispIDs doesn't return labels, so we need to check)
			if issue, err := bd.Show(id); err == nil && issue != nil {
				if !beads.HasLabel(issue, "gt:agent") {
					_ = bd.Update(id, beads.UpdateOptions{AddLabels: []string{"gt:agent"}})
				}
			}
			return nil
		}

		// Not in issues or open wisps — the bead may still exist in this
		// database without the gt:agent label (scope.issues only sees labeled
		// beads). Legacy 1.1.0-era identity beads are type=task with no
		// gt:agent label, so they land here: repair them in place instead of
		// falling through to a duplicate-ID create (gt-8po).
		if issue, err := bd.Show(id); err == nil && issue != nil {
			// Bead exists but is closed — REOPEN it instead of recreating
			if issue.Status == "closed" {
				openStatus := "open"
				if err := bd.Update(id, beads.UpdateOptions{Status: &openStatus}); err != nil {
					return fmt.Errorf("reopening closed agent bead %s: %w", id, err)
				}
			}
			// Ensure it has the gt:agent label so existence checks can see it.
			if !beads.HasLabel(issue, "gt:agent") {
				return ensureAgentLabel(ctx, bd, workDir, id)
			}
			return nil
		}

		// Bead truly missing — create it (CreateAgentBead handles ephemeral fallback)
		if _, err := bd.CreateAgentBead(id, desc, fields); err != nil {
			return fmt.Errorf("creating %s: %w", id, err)
		}
		// CreateAgentBead may create a wisp-backed bead, and doctor's wisp
		// queries join wisp_labels (gt-3vx). bd routes a label to wisp_labels
		// for a wisp, so adding it through bd, pinned to this database, puts it
		// where those queries look.
		if err := ctx.repair(workDir).Update(id, beads.UpdateOptions{AddLabels: []string{"gt:agent"}}); err != nil {
			return fmt.Errorf("labeling new agent bead %s gt:agent: %w", id, err)
		}
		return nil
	}

	mayorID := beads.MayorBeadIDTown()
	if err := fixAgentBead(townBd, townScope, townBeadsPath, mayorID,
		"Mayor - global coordinator, handles cross-rig communication and escalations.",
		&beads.AgentFields{RoleType: "mayor", AgentState: "idle"},
	); err != nil {
		errs = append(errs, err)
	}

	// Load routes to get prefixes for rig-level agents
	beadsDir := filepath.Join(ctx.TownRoot, ".beads")
	routes, err := beads.LoadRoutes(beadsDir)
	if err != nil {
		return fmt.Errorf("loading routes.jsonl: %w", err)
	}

	// Build prefix -> rigInfo map from routes
	prefixToRig := make(map[string]rigInfo)
	for _, r := range routes {
		parts := strings.Split(r.Path, "/")
		if len(parts) >= 1 && parts[0] != "." {
			rigName := parts[0]
			if ctx.RigName != "" && rigName != ctx.RigName {
				continue
			}
			prefix := strings.TrimSuffix(r.Prefix, "-")
			prefixToRig[prefix] = rigInfo{
				name:      rigName,
				beadsPath: r.Path,
			}
		}
	}

	if len(prefixToRig) == 0 {
		return errors.Join(errs...)
	}

	// Fix agents for each rig — existence checked against that rig's own
	// database, so town-level duplicates don't mask missing rig-local beads.
	for prefix, info := range prefixToRig {
		rigBeadsPath := filepath.Join(ctx.TownRoot, info.beadsPath)
		// NewRigLocal: the create MUST land in this rig's database (see the
		// townBd comment above); a routed wrapper would re-target the town DB.
		bd := ctx.beadsRigLocal(rigBeadsPath)
		rigName := info.name
		rigScope := loadAgentBeadScope(bd)

		crewWorkers := listCrewWorkers(ctx.TownRoot, rigName)
		for _, workerName := range crewWorkers {
			crewID := beads.CrewBeadIDWithPrefix(prefix, rigName, workerName)
			if err := fixAgentBead(bd, rigScope, rigBeadsPath, crewID,
				fmt.Sprintf("Crew worker %s in %s - human-managed persistent workspace.", workerName, rigName),
				&beads.AgentFields{RoleType: "crew", Rig: rigName, AgentState: "idle"},
			); err != nil {
				errs = append(errs, err)
			}
		}

		polecatWorkers := listPolecats(ctx.TownRoot, rigName)
		for _, polecatName := range polecatWorkers {
			polecatID := beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName)
			if err := fixAgentBead(bd, rigScope, rigBeadsPath, polecatID,
				fmt.Sprintf("Polecat worker %s in %s - autonomous worker with persistent identity.", polecatName, rigName),
				&beads.AgentFields{RoleType: "polecat", Rig: rigName, AgentState: "idle"},
			); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return errors.Join(errs...)
}

// listCrewWorkers returns the names of canonical crew workers in a rig.
// Filters out git worktrees and other non-identity directories that may
// exist under <rig>/crew/ (e.g., fix branches, cross-rig worktrees).
// See GH#2767.
func listCrewWorkers(townRoot, rigName string) []string {
	crewDir := filepath.Join(townRoot, rigName, "crew")
	entries, err := os.ReadDir(crewDir)
	if err != nil {
		return nil // No crew directory or can't read it
	}

	var workers []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		// Git worktrees have a .git FILE (not directory) that contains
		// "gitdir: /path/to/main/.git/worktrees/<name>". Canonical crew
		// workers have a .git DIRECTORY (they are the main checkout).
		// Skip directories where .git is a file — they're worktrees.
		dotGit := filepath.Join(crewDir, entry.Name(), ".git")
		if info, err := os.Lstat(dotGit); err == nil && !info.IsDir() {
			continue // .git is a file → this is a worktree, not a crew identity
		}
		workers = append(workers, entry.Name())
	}
	return workers
}

// ensureAgentLabel adds gt:agent to id through bd. bd update can exit 0
// without touching a bead whose legacy prefix does not route (GH#2127), so
// the label is read back, and a miss is retried through bd pinned to
// workDir's database, where prefix routing cannot send it elsewhere. There
// is no SQL fallback (gt-fcxe9.12): a label bd cannot add is an error.
func ensureAgentLabel(ctx *CheckContext, bd *beads.Beads, workDir, id string) error {
	add := beads.UpdateOptions{AddLabels: []string{"gt:agent"}}
	routedErr := bd.Update(id, add)
	if routedErr == nil && verifyLabelAdded(ctx, workDir, id, "gt:agent") {
		return nil
	}
	if err := ctx.repair(workDir).Update(id, add); err != nil {
		return fmt.Errorf("adding gt:agent label to %s: bd update: %v; pinned to %s: %w", id, routedErr, workDir, err)
	}
	if !verifyLabelAdded(ctx, workDir, id, "gt:agent") {
		return fmt.Errorf("adding gt:agent label to %s: bd update reported success but the label is absent in %s", id, workDir)
	}
	return nil
}

// verifyLabelAdded checks whether a label exists on a bead by querying labels table.
// Returns false if the label is not found or the query fails.
func verifyLabelAdded(ctx *CheckContext, workDir, beadID, label string) bool {
	escapedID := strings.ReplaceAll(beadID, "'", "''")
	escapedLabel := strings.ReplaceAll(label, "'", "''")
	query := fmt.Sprintf("SELECT 1 FROM labels WHERE issue_id = '%s' AND label = '%s' LIMIT 1", escapedID, escapedLabel)
	output, err := ctx.bd(workDir, nil).SQL(query)
	if err != nil {
		return false
	}
	// bd sql returns header + data rows; if we got more than just a header, the label exists
	return strings.Contains(string(output), "1")
}

// listPolecats returns the names of canonical polecat directories in a rig.
// Filters out git worktrees (same logic as listCrewWorkers). See GH#2767.
func listPolecats(townRoot, rigName string) []string {
	polecatDir := filepath.Join(townRoot, rigName, "polecats")
	entries, err := os.ReadDir(polecatDir)
	if err != nil {
		return nil // No polecats directory or can't read it
	}

	var polecats []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dotGit := filepath.Join(polecatDir, entry.Name(), ".git")
		if info, err := os.Lstat(dotGit); err == nil && !info.IsDir() {
			continue // worktree — skip
		}
		polecats = append(polecats, entry.Name())
	}
	return polecats
}
