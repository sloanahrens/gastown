package cmd

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/convoy"
	"github.com/steveyegge/gastown/internal/workspace"
)

// StageInputKind identifies the type of input provided to gt convoy stage.
type StageInputKind int

const (
	StageInputEpic   StageInputKind = iota // single epic ID → walk children
	StageInputTasks                        // one or more task IDs → analyze as-is
	StageInputConvoy                       // single convoy ID → read tracked beads
)

// StageInput represents parsed and validated input for gt convoy stage.
type StageInput struct {
	Kind    StageInputKind
	IDs     []string // bead IDs to process
	RawArgs []string // original args for error messages
}

// ---------------------------------------------------------------------------
// Overlapping convoy detection (prevent duplicate convoy creation)
// ---------------------------------------------------------------------------

// convoyTrackedBeadIDs returns the set of bead IDs tracked by a convoy.
// Uses bdDepListRawIDs to query the raw dependencies table, which works
// for cross-database deps where tracked issues live in a different Dolt
// database (e.g., ds-* issues tracked by an hq-cv-* convoy). See GH #2624.
func convoyTrackedBeadIDs(townBeads, convoyID string) (map[string]bool, error) {
	trackedIDs, err := bdDepListRawIDs(townBeads, convoyID, "down", "tracks")
	if err != nil {
		return nil, fmt.Errorf("tracked deps for %s: %w", convoyID, err)
	}

	ids := make(map[string]bool, len(trackedIDs))
	for _, id := range trackedIDs {
		ids[id] = true
	}
	return ids, nil
}

// createStagedConvoy creates a convoy with the given staged status.
// It generates a convoy ID, builds a title and description, then runs
// `bd create` to create the convoy and typed tracking relations for each slingable bead.
// Convoys live in the town HQ beads database (hq-cv-* prefix), so all bd
// commands run against getTownBeadsDir(), matching gt convoy create behavior.
// Returns the convoy ID.
func createStagedConvoy(dag *ConvoyDAG, waves []Wave, status string, title string) (string, error) {
	// Convoys live in the town HQ beads database.
	townBeads, err := getTownBeadsDir()
	if err != nil {
		return "", err
	}

	// Resolve the actual .beads directory (follows redirects) before calling
	// EnsureCustomTypes/Statuses, which expect a .beads path, not a workspace root.
	resolvedBeads := beads.ResolveBeadsDir(townBeads)

	// Ensure custom types (including 'convoy') are registered in town beads.
	if err := beads.EnsureCustomTypes(resolvedBeads); err != nil {
		return "", fmt.Errorf("ensuring custom types: %w", err)
	}

	// Ensure custom statuses (staged_ready, staged_warnings) are registered.
	if err := beads.EnsureCustomStatuses(resolvedBeads); err != nil {
		return "", fmt.Errorf("ensuring custom statuses: %w", err)
	}

	// Generate convoy ID.
	convoyID := fmt.Sprintf("hq-cv-%s", generateShortID())

	// Count slingable tasks and unique rigs.
	taskCount := 0
	rigSet := make(map[string]bool)
	var slingableIDs []string
	for _, node := range dag.Nodes {
		if isSlingableType(node.Type) {
			taskCount++
			slingableIDs = append(slingableIDs, node.ID)
			if node.Rig != "" {
				rigSet[node.Rig] = true
			}
		}
	}
	rigCount := len(rigSet)

	// Sort slingable IDs for determinism.
	sort.Strings(slingableIDs)

	// Build title and description.
	if title == "" {
		title = fmt.Sprintf("Staged: %d beads across %d rigs", taskCount, rigCount)
	}
	description := fmt.Sprintf("Staged convoy: %d tasks, %d waves. Staged at %s",
		taskCount, len(waves), time.Now().UTC().Format(time.RFC3339))

	// Create the convoy via bd create in town beads, then set status via bd update.
	createArgs := []string{
		"create",
		"--type=task",
		"--id=" + convoyID,
		"--title=" + title,
		"--description=" + description,
		"--labels=gt:convoy",
	}
	if beads.NeedsForceForID(convoyID) {
		createArgs = append(createArgs, "--force")
	}
	if out, err := BdCmd(createArgs...).Dir(townBeads).WithAutoCommit().CombinedOutput(); err != nil {
		return "", fmt.Errorf("bd create convoy: %w\noutput: %s", err, out)
	}

	// Set the staged status.
	// Strip BEADS_DIR so bd discovers the correct database from Dir()
	// rather than using an inherited (possibly wrong) override.
	if out, err := BdCmd("update", convoyID, "--status="+status).
		Dir(townBeads).StripBeadsDir().WithAutoCommit().
		CombinedOutput(); err != nil {
		return "", fmt.Errorf("bd update convoy status: %w\noutput: %s", err, out)
	}

	// Track each slingable bead via the typed dependency helper.
	for _, beadID := range slingableIDs {
		if err := addTrackingRelationFn(townBeads, convoyID, beadID); err != nil {
			printStageWarning("  Warning: could not track %s in convoy: %v\n", beadID, err)
		}
	}

	return convoyID, nil
}

// ConvoyDAG represents an in-memory dependency graph for convoy staging.
type ConvoyDAG struct {
	Nodes map[string]*ConvoyDAGNode
}

// ConvoyDAGNode represents a single bead in the DAG.
type ConvoyDAGNode struct {
	ID        string
	Title     string
	Type      string // "epic", "task", "bug", etc.
	Status    string
	Rig       string
	BlockedBy []string // IDs of beads that block this one (execution edges)
	Blocks    []string // IDs of beads this one blocks
	Children  []string // parent-child children (hierarchy only, not execution)
	Parent    string   // parent-child parent
}

// detectCycles checks the DAG for cycles in execution edges (blocks/conditional-blocks/waits-for).
// Returns the cycle path as []string if a cycle is found, or nil if acyclic.
// Only considers BlockedBy/Blocks edges (execution edges), NOT parent-child.
//
// Uses DFS with 3-color marking:
//   - white (0): unvisited
//   - gray  (1): on the current recursion stack
//   - black (2): fully explored
func detectCycles(dag *ConvoyDAG) []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)

	color := make(map[string]int)     // default zero = white
	parent := make(map[string]string) // tracks DFS parent for cycle extraction

	// Sort node IDs for deterministic traversal order.
	ids := make([]string, 0, len(dag.Nodes))
	for id := range dag.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// extractCycle walks back from the back-edge target through the DFS
	// parent chain to reconstruct the cycle path.
	extractCycle := func(from, to string) []string {
		// from -> to is the back-edge. The cycle is: to -> ... -> from -> to.
		path := []string{to}
		cur := from
		for cur != to {
			path = append(path, cur)
			cur = parent[cur]
		}
		// Reverse so the cycle reads in traversal order.
		for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
			path[i], path[j] = path[j], path[i]
		}
		return path
	}

	var dfs func(id string) []string
	dfs = func(id string) []string {
		color[id] = gray
		node := dag.Nodes[id]
		if node == nil {
			color[id] = black
			return nil
		}

		// Sort neighbors for deterministic traversal.
		neighbors := make([]string, len(node.Blocks))
		copy(neighbors, node.Blocks)
		sort.Strings(neighbors)

		for _, next := range neighbors {
			switch color[next] {
			case white:
				parent[next] = id
				if cycle := dfs(next); cycle != nil {
					return cycle
				}
			case gray:
				// Back-edge found → cycle.
				return extractCycle(id, next)
			}
			// black → already fully explored, skip.
		}

		color[id] = black
		return nil
	}

	for _, id := range ids {
		if color[id] == white {
			if cycle := dfs(id); cycle != nil {
				return cycle
			}
		}
	}

	return nil
}

// Wave represents a group of tasks that can execute in parallel.
type Wave struct {
	Number int
	Tasks  []string // bead IDs, sorted for determinism
}

// GatedTask represents a slingable task that cannot be placed in any wave
// because it is blocked (directly or transitively) by an open non-slingable
// node such as a decision or epic.
type GatedTask struct {
	TaskID  string
	GatedBy []string // IDs of non-slingable open blockers (direct gates only)
}

// isSlingableType delegates to the canonical convoy.IsSlingableType, which
// handles empty types (legacy beads that default to "task").
func isSlingableType(beadType string) bool {
	return convoy.IsSlingableType(beadType)
}

// computeWaves assigns each slingable task to an execution wave using Kahn's algorithm.
// Wave 1 = tasks with no unsatisfied blocking deps within the staged set.
// Wave N+1 = tasks whose blockers are ALL in wave N or earlier.
// Epics and non-slingable types are excluded from wave task lists but their
// blocking edges ARE respected — a task blocked by a decision bead will not
// appear until that decision is resolved (fixes #2141).
// Parent-child deps do NOT create execution edges.
// Returns (waves, gatedTasks, error). gatedTasks lists tasks blocked by open
// non-slingable nodes (decisions, epics) that cannot be placed in any wave.
func computeWaves(dag *ConvoyDAG) ([]Wave, []GatedTask, error) {
	// Step 1: Filter to slingable types only.
	slingable := make(map[string]*ConvoyDAGNode)
	for id, node := range dag.Nodes {
		if isSlingableType(node.Type) {
			slingable[id] = node
		}
	}
	if len(slingable) == 0 {
		return nil, nil, fmt.Errorf("no slingable tasks in DAG (need task, bug, feature, or chore)")
	}

	// Step 2: Calculate in-degree for each slingable node.
	// Count slingable blockers (decremented by Kahn's) and open
	// non-slingable blockers (never decremented — act as gates).
	inDegree := make(map[string]int, len(slingable))
	for id, node := range slingable {
		deg := 0
		for _, blocker := range node.BlockedBy {
			if _, ok := slingable[blocker]; ok {
				deg++ // slingable blocker — handled by Kahn's
			} else if bNode, ok := dag.Nodes[blocker]; ok {
				if bNode.Status != "closed" && bNode.Status != "tombstone" {
					deg++ // non-slingable open blocker — gate
				}
			}
		}
		inDegree[id] = deg
	}

	// Step 3-6: Kahn's algorithm — peel off waves of in-degree-0 nodes.
	var waves []Wave
	processed := 0
	waveNum := 0

	for processed < len(slingable) {
		// Collect nodes with in-degree 0.
		var ready []string
		for id, deg := range inDegree {
			if deg == 0 {
				ready = append(ready, id)
			}
		}

		if len(ready) == 0 {
			// No cycles exist (detectCycles ran before computeWaves),
			// so remaining tasks are gated by open non-slingable nodes
			// (decisions, epics, etc.) either directly or transitively.
			var gated []GatedTask
			for id := range inDegree {
				node := slingable[id]
				var gatedBy []string
				for _, blocker := range node.BlockedBy {
					if _, ok := slingable[blocker]; ok {
						continue
					}
					if bNode, ok := dag.Nodes[blocker]; ok {
						if bNode.Status != "closed" && bNode.Status != "tombstone" {
							gatedBy = append(gatedBy, blocker)
						}
					}
				}
				sort.Strings(gatedBy)
				gated = append(gated, GatedTask{TaskID: id, GatedBy: gatedBy})
			}
			sort.Slice(gated, func(i, j int) bool { return gated[i].TaskID < gated[j].TaskID })
			return waves, gated, nil
		}

		// Step 7: Sort within each wave for determinism.
		sort.Strings(ready)
		waveNum++

		waves = append(waves, Wave{
			Number: waveNum,
			Tasks:  ready,
		})

		// Remove processed nodes and decrement in-degrees of their dependents.
		for _, id := range ready {
			delete(inDegree, id)
			processed++

			// Decrement in-degree of nodes this one blocks (that are slingable).
			for _, blocked := range slingable[id].Blocks {
				if _, ok := inDegree[blocked]; ok {
					inDegree[blocked]--
				}
			}
		}
	}

	return waves, nil, nil
}

// BeadInfo represents raw bead data from bd show output.
type BeadInfo struct {
	ID     string
	Title  string
	Type   string // "epic", "task", "bug", etc.
	Status string
	Rig    string // resolved rig name
}

// DepInfo represents a raw dependency from bd dep list output.
type DepInfo struct {
	IssueID     string // the dependent bead
	DependsOnID string // the bead it depends on
	Type        string // "blocks", "parent-child", "waits-for", "conditional-blocks", "tracks", "related", etc.
}

// buildConvoyDAG constructs a ConvoyDAG from raw bead and dependency data.
// Edge classification:
//   - blocks, conditional-blocks, waits-for → execution edges (BlockedBy/Blocks)
//   - parent-child → hierarchy metadata (Children/Parent), NOT execution edges
//   - related, tracks, discovered-from, etc. → ignored
func buildConvoyDAG(beads []BeadInfo, deps []DepInfo) *ConvoyDAG {
	dag := &ConvoyDAG{Nodes: make(map[string]*ConvoyDAGNode)}

	// Create nodes from beads.
	for _, b := range beads {
		dag.Nodes[b.ID] = &ConvoyDAGNode{
			ID:     b.ID,
			Title:  b.Title,
			Type:   b.Type,
			Status: b.Status,
			Rig:    b.Rig,
		}
	}

	// Process deps.
	for _, d := range deps {
		from := dag.Nodes[d.DependsOnID] // the blocker
		to := dag.Nodes[d.IssueID]       // the blocked
		if from == nil || to == nil {
			continue // skip deps referencing beads not in our set
		}

		switch d.Type {
		case "blocks", "conditional-blocks", "waits-for", "merge-blocks":
			// Execution edges.
			from.Blocks = append(from.Blocks, to.ID)
			to.BlockedBy = append(to.BlockedBy, from.ID)
		case "parent-child":
			// Hierarchy only.
			from.Children = append(from.Children, to.ID)
			to.Parent = from.ID
		default:
			// related, tracks, discovered-from, etc. — ignored.
		}
	}

	return dag
}

// StagingFinding represents an error or warning found during convoy staging analysis.
type StagingFinding struct {
	Severity     string   // "error" or "warning"
	Category     string   // "cycle", "no-rig", "orphan", "blocked-rig", "cross-rig", "capacity", "missing-branch"
	BeadIDs      []string // affected bead IDs
	Message      string   // human-readable description
	SuggestedFix string   // actionable fix suggestion
}

// categorizeFindings splits findings into errors and warnings by severity.
func categorizeFindings(findings []StagingFinding) (errors, warnings []StagingFinding) {
	for _, f := range findings {
		switch f.Severity {
		case "error":
			errors = append(errors, f)
		default:
			warnings = append(warnings, f)
		}
	}
	return
}

// detectErrors runs all error detection checks on the DAG.
// Returns findings with severity="error" for fatal issues.
func detectErrors(dag *ConvoyDAG) []StagingFinding {
	var findings []StagingFinding

	// Check for cycles
	cyclePath := detectCycles(dag)
	if cyclePath != nil {
		findings = append(findings, StagingFinding{
			Severity:     "error",
			Category:     "cycle",
			BeadIDs:      cyclePath,
			Message:      fmt.Sprintf("dependency cycle detected: %s", strings.Join(cyclePath, " → ")),
			SuggestedFix: fmt.Sprintf("remove one blocking dependency in the chain: %s", strings.Join(cyclePath, " → ")),
		})
	}

	// Check for beads with no valid rig
	for _, node := range dag.Nodes {
		if !isSlingableType(node.Type) {
			continue // epics don't need rigs
		}
		if node.Rig == "" {
			findings = append(findings, StagingFinding{
				Severity:     "error",
				Category:     "no-rig",
				BeadIDs:      []string{node.ID},
				Message:      fmt.Sprintf("bead %s has no valid rig (prefix not mapped in routes.jsonl or resolves to empty)", node.ID),
				SuggestedFix: fmt.Sprintf("add a routes.jsonl entry mapping the prefix of %s to a rig, or check that the bead ID has a valid prefix", node.ID),
			})
		}
	}

	// Sort findings by bead ID for determinism
	sort.Slice(findings, func(i, j int) bool {
		if len(findings[i].BeadIDs) == 0 || len(findings[j].BeadIDs) == 0 {
			return findings[i].Category < findings[j].Category
		}
		return findings[i].BeadIDs[0] < findings[j].BeadIDs[0]
	})

	return findings
}

// chooseStatus determines the convoy status based on analysis results.
// Returns "" if errors found (no convoy should be created).
func chooseStatus(errors, warnings []StagingFinding) string {
	if len(errors) > 0 {
		return "" // no convoy
	}
	if len(warnings) > 0 {
		return "staged_warnings"
	}
	return "staged_ready"
}

// renderErrors formats error findings for console output.
func renderErrors(findings []StagingFinding) string {
	if len(findings) == 0 {
		return ""
	}
	var buf strings.Builder
	buf.WriteString("Errors:\n")
	for i, f := range findings {
		buf.WriteString(fmt.Sprintf("  %d. [%s] %s\n", i+1, f.Category, f.Message))
		if len(f.BeadIDs) > 0 {
			buf.WriteString(fmt.Sprintf("     Affected: %s\n", strings.Join(f.BeadIDs, ", ")))
		}
		if f.SuggestedFix != "" {
			buf.WriteString(fmt.Sprintf("     Fix: %s\n", f.SuggestedFix))
		}
	}
	return buf.String()
}

// ---------------------------------------------------------------------------
// bd JSON parsing types
// ---------------------------------------------------------------------------

// bdShowResult matches the JSON output of `bd show <id> --json`.
type bdShowResult struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Status      string   `json:"status"`
	IssueType   string   `json:"issue_type"`
	Labels      []string `json:"labels"`
	Description string   `json:"description"`
}

// bdDepResult matches the JSON output of `bd dep list <id> --json`.
// Each entry is a bead that the queried issue depends on.
// The IssueID is set by the caller (it's the bead we queried).
type bdDepResult struct {
	IssueID     string `json:"-"`               // set by caller
	DependsOnID string `json:"id"`              // the dependency target
	Type        string `json:"dependency_type"` // blocks, parent-child, etc.
}

// ---------------------------------------------------------------------------
// bd shell-out helpers
// ---------------------------------------------------------------------------

func runBdJSONForBead(beadID string, args ...string) ([]byte, error) {
	return runBdJSON(resolveBeadDir(beadID), args...)
}

// bdShow runs `bd show <id> --json` and returns the parsed bead info.
// Returns error if bd exits non-zero or returns no results.
func bdShow(beadID string) (*bdShowResult, error) {
	out, err := runBdJSONForBead(beadID, "show", beadID, "--json")
	if err != nil {
		return nil, fmt.Errorf("bd show %s: %w", beadID, err)
	}

	var results []bdShowResult
	if err := json.Unmarshal(out, &results); err != nil {
		return nil, fmt.Errorf("bd show %s: parse JSON: %w (raw: %s)", beadID, err, out)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("bd show %s: no results", beadID)
	}

	return &results[0], nil
}

// bdDepList runs `bd dep list <id> --json` and returns parsed deps.
// bd dep list returns the beads that <id> depends on. Each result's
// DependsOnID is the dependency target; IssueID is set to <id> by this func.
func bdDepList(beadID string) ([]bdDepResult, error) {
	out, err := runBdJSONForBead(beadID, "dep", "list", beadID, "--json")
	if err != nil {
		return nil, fmt.Errorf("bd dep list %s: %w", beadID, err)
	}

	var results []bdDepResult
	if err := json.Unmarshal(out, &results); err != nil {
		return nil, fmt.Errorf("bd dep list %s: parse JSON: %w (raw: %s)", beadID, err, out)
	}

	// Set the IssueID on each result (bd dep list returns deps OF beadID).
	for i := range results {
		results[i].IssueID = beadID
	}

	return results, nil
}

// bdListChildren runs `bd list --parent=<id> --json` and returns child beads.
// bd list is CWD-sensitive — it only searches the beads database in the current
// directory. We resolve the correct .beads directory from the bead's prefix via
// routes.jsonl so this works regardless of the caller's working directory.
//
// When the `--parent` index returns no rows, we fall back to a direct query
// against the dependencies table (parent-child links) and resolve each child
// via bdShow. This handles the case (GH #3700) where the index used by
// `bd list --parent` doesn't see children that were added via `bd dep add ...
// --type=parent-child`. The deps table is authoritative.
func bdListChildren(parentID string) ([]bdShowResult, error) {
	out, err := runBdJSONForBead(parentID, "list", "--parent="+parentID, "--json")
	if err != nil {
		return nil, fmt.Errorf("bd list --parent=%s: %w", parentID, err)
	}

	// Handle empty output (no children) — try the deps-table fallback first.
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "[]" {
		return bdListChildrenViaDeps(parentID)
	}

	var results []bdShowResult
	if err := json.Unmarshal(out, &results); err != nil {
		return nil, fmt.Errorf("bd list --parent=%s: parse JSON: %w (raw: %s)", parentID, err, out)
	}

	return results, nil
}

// bdListChildrenViaDeps resolves children by querying the dependencies table
// directly for parent-child links, then loading each child via bdShow.
//
// Used as a fallback when `bd list --parent=<id>` returns empty even though
// children exist (GH #3700). Returns nil (not an error) when the prefix can't
// be resolved or no parent-child deps exist.
func bdListChildrenViaDeps(parentID string) ([]bdShowResult, error) {
	beadsDir := beadsDirForID(parentID)
	if beadsDir == "" {
		// Can't resolve the rig; nothing more we can do.
		return nil, nil
	}

	// Production data stores parent-child as a typed dependency target where
	// issue_id=parent. "down" returns target rows for the epic's children.
	childIDs, err := bdDepListRawIDs(beadsDir, parentID, "down", "parent-child")
	if err != nil {
		return nil, nil // best-effort — caller still gets the empty primary result
	}
	if len(childIDs) == 0 {
		return nil, nil
	}

	results := make([]bdShowResult, 0, len(childIDs))
	for _, id := range childIDs {
		child, err := bdShow(id)
		if err != nil || child == nil {
			continue
		}
		results = append(results, *child)
	}
	return results, nil
}

// ---------------------------------------------------------------------------
// collectBeads and variants
// ---------------------------------------------------------------------------

// rigFromBeadID extracts the rig name from a bead ID by looking up its prefix
// in routes.jsonl. Returns empty string if the prefix is not found or if the
// workspace cannot be resolved.
func rigFromBeadID(beadID string) string {
	prefix := beads.ExtractPrefix(beadID)
	if prefix == "" {
		return ""
	}
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return ""
	}
	return beads.GetRigNameForPrefix(townRoot, prefix)
}

// beadsDirForID resolves the .beads directory that owns a given bead ID by
// looking up its prefix in routes.jsonl. This is needed for CWD-sensitive bd
// commands like `bd list --parent=` which only search the local database.
// Returns empty string if the prefix or workspace cannot be resolved.
func beadsDirForID(beadID string) string {
	prefix := beads.ExtractPrefix(beadID)
	if prefix == "" {
		return ""
	}
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return ""
	}
	rigPath := beads.GetRigPathForPrefix(townRoot, prefix)
	if rigPath == "" {
		return ""
	}
	return beads.ResolveBeadsDir(rigPath)
}

// collectBeads gathers all beads for staging based on the input kind.
// For epic input: recursively walks parent-child tree via bd list --parent=<id> --json
// For task list input: validates each bead exists via bd show <id> --json
// For convoy input: reads tracked beads via typed dependency target columns.
// Returns BeadInfo slice and DepInfo slice for all collected beads.
func collectBeads(input *StageInput) ([]BeadInfo, []DepInfo, error) {
	switch input.Kind {
	case StageInputEpic:
		return collectEpicBeads(input.IDs[0])
	case StageInputTasks:
		return collectTaskListBeads(input.IDs)
	case StageInputConvoy:
		return collectConvoyBeads(input.IDs[0])
	}
	return nil, nil, fmt.Errorf("unknown input kind: %d", input.Kind)
}

// collectEpicBeads recursively walks an epic's parent-child tree.
// Uses bd list --parent=<id> --json for each level.
// For each bead found, also fetches its deps via bd dep list <id> --json.
func collectEpicBeads(epicID string) ([]BeadInfo, []DepInfo, error) {
	// 1. Validate the root epic exists.
	root, err := bdShow(epicID)
	if err != nil {
		return nil, nil, fmt.Errorf("epic %s: %w", epicID, err)
	}

	var allBeads []BeadInfo
	var allDeps []DepInfo
	visited := make(map[string]bool)

	// BFS queue for recursive tree walk.
	queue := []bdShowResult{*root}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if visited[current.ID] {
			continue
		}
		visited[current.ID] = true

		// Add bead info.
		allBeads = append(allBeads, BeadInfo{
			ID:     current.ID,
			Title:  current.Title,
			Type:   current.IssueType,
			Status: current.Status,
			Rig:    rigFromBeadID(current.ID),
		})

		// Fetch deps for this bead.
		deps, err := bdDepList(current.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("deps for %s: %w", current.ID, err)
		}
		for _, d := range deps {
			allDeps = append(allDeps, DepInfo{
				IssueID:     d.IssueID,
				DependsOnID: d.DependsOnID,
				Type:        d.Type,
			})
		}

		// List children and enqueue them.
		children, err := bdListChildren(current.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("children of %s: %w", current.ID, err)
		}
		for _, child := range children {
			if !visited[child.ID] {
				queue = append(queue, child)
			}
		}
	}

	return allBeads, allDeps, nil
}

// collectTaskListBeads validates and fetches info for explicit task IDs.
func collectTaskListBeads(taskIDs []string) ([]BeadInfo, []DepInfo, error) {
	var allBeads []BeadInfo
	var allDeps []DepInfo

	for _, id := range taskIDs {
		// Validate bead exists.
		result, err := bdShow(id)
		if err != nil {
			return nil, nil, fmt.Errorf("task %s: %w", id, err)
		}

		allBeads = append(allBeads, BeadInfo{
			ID:     result.ID,
			Title:  result.Title,
			Type:   result.IssueType,
			Status: result.Status,
			Rig:    rigFromBeadID(result.ID),
		})

		// Fetch deps.
		deps, err := bdDepList(id)
		if err != nil {
			return nil, nil, fmt.Errorf("deps for %s: %w", id, err)
		}
		for _, d := range deps {
			allDeps = append(allDeps, DepInfo{
				IssueID:     d.IssueID,
				DependsOnID: d.DependsOnID,
				Type:        d.Type,
			})
		}
	}

	return allBeads, allDeps, nil
}

// collectConvoyBeads reads tracked beads from an existing convoy.
func collectConvoyBeads(convoyID string) ([]BeadInfo, []DepInfo, error) {
	// 1. Validate convoy exists.
	_, err := bdShow(convoyID)
	if err != nil {
		return nil, nil, fmt.Errorf("convoy %s: %w", convoyID, err)
	}

	// 2. Read tracked IDs using the same filtered dep-list path used by status
	// and staged-convoy reconciliation. This handles id-only dep rows and
	// external:<rig>:<id> wrappers.
	townBeads, err := getTownBeadsDir()
	if err != nil {
		return nil, nil, err
	}
	trackedSet, err := convoyTrackedBeadIDs(townBeads, convoyID)
	if err != nil {
		return nil, nil, fmt.Errorf("deps for convoy %s: %w", convoyID, err)
	}
	trackedIDs := make([]string, 0, len(trackedSet))
	for id := range trackedSet {
		trackedIDs = append(trackedIDs, id)
	}
	sort.Strings(trackedIDs)

	if len(trackedIDs) == 0 {
		return nil, nil, fmt.Errorf("convoy %s tracks no beads", convoyID)
	}

	// 3. Fetch each tracked bead + its deps.
	return collectTaskListBeads(trackedIDs)
}

// ---------------------------------------------------------------------------
// DAG tree display (ASCII, epic hierarchy)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Warning detection (gt-csl.3.4)
// ---------------------------------------------------------------------------

// waveCapacityThreshold is the maximum number of tasks in a wave before a
// capacity warning is emitted.
const waveCapacityThreshold = 5

// detectWarnings runs all warning checks on the DAG.
// Returns findings with severity="warning".
func detectWarnings(dag *ConvoyDAG, input *StageInput) []StagingFinding {
	var findings []StagingFinding

	findings = append(findings, detectOrphans(dag, input)...)
	findings = append(findings, detectBlockedRigs(dag)...)
	findings = append(findings, detectCrossRig(dag)...)
	findings = append(findings, estimateCapacity(dag)...)
	findings = append(findings, detectMissingBranches(dag)...)

	// Sort findings by first bead ID for determinism.
	sort.Slice(findings, func(i, j int) bool {
		if len(findings[i].BeadIDs) == 0 || len(findings[j].BeadIDs) == 0 {
			return findings[i].Category < findings[j].Category
		}
		return findings[i].BeadIDs[0] < findings[j].BeadIDs[0]
	})

	return findings
}

// detectOrphans finds slingable tasks that are completely isolated in the
// wave graph (in-degree 0 AND out-degree 0 among slingable nodes).
// Only applies to epic input — task-list and convoy input never warn about
// orphans because isolation is expected.
func detectOrphans(dag *ConvoyDAG, input *StageInput) []StagingFinding {
	if input.Kind != StageInputEpic {
		return nil
	}

	// Build slingable set.
	slingable := make(map[string]*ConvoyDAGNode)
	for id, node := range dag.Nodes {
		if isSlingableType(node.Type) {
			slingable[id] = node
		}
	}

	var findings []StagingFinding
	for id, node := range slingable {
		// Calculate in-degree among all DAG nodes (not just slingable)
		// to avoid false orphan warnings for decision-gated tasks.
		inDeg := 0
		for _, blocker := range node.BlockedBy {
			if _, ok := dag.Nodes[blocker]; ok {
				inDeg++
			}
		}

		// Calculate out-degree among all DAG nodes.
		outDeg := 0
		for _, blocked := range node.Blocks {
			if _, ok := dag.Nodes[blocked]; ok {
				outDeg++
			}
		}

		if inDeg == 0 && outDeg == 0 {
			findings = append(findings, StagingFinding{
				Severity:     "warning",
				Category:     "orphan",
				BeadIDs:      []string{id},
				Message:      fmt.Sprintf("task %s has no blocking dependencies with other staged tasks (isolated in wave graph)", id),
				SuggestedFix: fmt.Sprintf("add a blocking dependency for %s, or verify it should be staged independently", id),
			})
		}
	}

	return findings
}

// isRigBlockedFn is a seam for tests. Production uses IsRigParkedOrDocked.
var isRigBlockedFn = func(townRoot, rigName string) (bool, string) {
	return IsRigParkedOrDocked(townRoot, rigName)
}

// detectBlockedRigs warns about slingable nodes whose target rig is parked
// or docked (gt-4owfd.1, #2120). Uses IsRigParkedOrDocked which checks both
// wisp ephemeral state and persistent bead labels.
func detectBlockedRigs(dag *ConvoyDAG) []StagingFinding {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		// Can't resolve town root — skip blocked rig detection
		return nil
	}

	// Group beads by blocked rig to consolidate warnings
	type blockedInfo struct {
		reason  string
		beadIDs []string
	}
	blockedRigs := make(map[string]*blockedInfo)
	for _, node := range dag.Nodes {
		if !isSlingableType(node.Type) {
			continue
		}
		if node.Rig == "" {
			continue // already caught by no-rig errors
		}
		if blocked, reason := isRigBlockedFn(townRoot, node.Rig); blocked {
			if info, ok := blockedRigs[node.Rig]; ok {
				info.beadIDs = append(info.beadIDs, node.ID)
			} else {
				blockedRigs[node.Rig] = &blockedInfo{reason: reason, beadIDs: []string{node.ID}}
			}
		}
	}

	// Sort rig names for deterministic output with multiple blocked rigs
	rigNames := make([]string, 0, len(blockedRigs))
	for rigName := range blockedRigs {
		rigNames = append(rigNames, rigName)
	}
	sort.Strings(rigNames)

	var findings []StagingFinding
	for _, rigName := range rigNames {
		info := blockedRigs[rigName]
		sort.Strings(info.beadIDs)
		undoCmd := "gt rig unpark"
		if info.reason == "docked" {
			undoCmd = "gt rig undock"
		}
		findings = append(findings, StagingFinding{
			Severity:     "warning",
			Category:     "blocked-rig",
			BeadIDs:      info.beadIDs,
			Message:      fmt.Sprintf("%d bead(s) target %s rig %q: %s", len(info.beadIDs), info.reason, rigName, strings.Join(info.beadIDs, ", ")),
			SuggestedFix: fmt.Sprintf("restore the rig: %s %s", undoCmd, rigName),
		})
	}
	return findings
}

// detectCrossRig finds slingable nodes that are on a different rig than the
// primary rig (most common rig among slingable nodes).
func detectCrossRig(dag *ConvoyDAG) []StagingFinding {
	// Count rigs among slingable nodes.
	rigCount := make(map[string]int)
	for _, node := range dag.Nodes {
		if !isSlingableType(node.Type) {
			continue
		}
		if node.Rig == "" {
			continue
		}
		rigCount[node.Rig]++
	}

	if len(rigCount) <= 1 {
		return nil // all same rig or no rigs
	}

	// Find primary rig (most common; tie-break alphabetically for determinism).
	primaryRig := ""
	primaryCount := 0
	for rig, count := range rigCount {
		if count > primaryCount || (count == primaryCount && rig < primaryRig) {
			primaryRig = rig
			primaryCount = count
		}
	}

	var findings []StagingFinding
	for _, node := range dag.Nodes {
		if !isSlingableType(node.Type) {
			continue
		}
		if node.Rig == "" || node.Rig == primaryRig {
			continue
		}
		findings = append(findings, StagingFinding{
			Severity:     "warning",
			Category:     "cross-rig",
			BeadIDs:      []string{node.ID},
			Message:      fmt.Sprintf("task %s is on rig %q (primary rig is %q)", node.ID, node.Rig, primaryRig),
			SuggestedFix: fmt.Sprintf("verify cross-rig routing for %s or reassign to %s", node.ID, primaryRig),
		})
	}
	return findings
}

// estimateCapacity checks each wave for task counts exceeding the threshold
// and emits an informational warning.
func estimateCapacity(dag *ConvoyDAG) []StagingFinding {
	waves, _, err := computeWaves(dag)
	if err != nil {
		return nil // no slingable tasks → nothing to warn about
	}

	var findings []StagingFinding
	for _, wave := range waves {
		if len(wave.Tasks) > waveCapacityThreshold {
			findings = append(findings, StagingFinding{
				Severity: "warning",
				Category: "capacity",
				BeadIDs:  wave.Tasks,
				Message:  fmt.Sprintf("wave %d has %d tasks (threshold: %d) — may exceed parallel capacity", wave.Number, len(wave.Tasks), waveCapacityThreshold),
			})
		}
	}
	return findings
}

// detectMissingBranches warns about sub-epics that have children but no
// integration branch metadata. This is a simple heuristic — real branch
// checking comes later.
func detectMissingBranches(dag *ConvoyDAG) []StagingFinding {
	var findings []StagingFinding
	for _, node := range dag.Nodes {
		if node.Type != "epic" {
			continue
		}
		// Skip root-level epics (no parent) — only warn for sub-epics.
		if node.Parent == "" {
			continue
		}
		if len(node.Children) > 0 {
			findings = append(findings, StagingFinding{
				Severity:     "warning",
				Category:     "missing-branch",
				BeadIDs:      []string{node.ID},
				Message:      fmt.Sprintf("sub-epic %s has %d children but no integration branch", node.ID, len(node.Children)),
				SuggestedFix: fmt.Sprintf("create an integration branch for sub-epic %s", node.ID),
			})
		}
	}
	return findings
}

// ---------------------------------------------------------------------------
// JSON output helpers (gt-csl.4.3)
// ---------------------------------------------------------------------------

func printStageWarning(format string, args ...any) {
	fmt.Printf(format, args...)
}
