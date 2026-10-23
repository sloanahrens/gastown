package cmd

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
)

// DAGNode represents a node in the dependency graph.
type DAGNode struct {
	ID           string     `json:"id"`
	Title        string     `json:"title"`
	Status       string     `json:"status"`
	Parallel     bool       `json:"parallel,omitempty"`
	Dependencies []string   `json:"dependencies,omitempty"`
	Dependents   []string   `json:"dependents,omitempty"`
	Tier         int        `json:"tier"` // Execution tier (0 = root, higher = later)
	Children     []*DAGNode `json:"children,omitempty"`
}

// DAGInfo contains the full DAG information for a molecule.
type DAGInfo struct {
	RootID       string              `json:"root_id"`
	RootTitle    string              `json:"root_title"`
	TotalNodes   int                 `json:"total_nodes"`
	Tiers        int                 `json:"tiers"`
	CriticalPath []string            `json:"critical_path,omitempty"`
	Nodes        map[string]*DAGNode `json:"nodes"`
	TierGroups   [][]string          `json:"tier_groups"` // Nodes grouped by tier
}

var moleculeDagCmd = &cobra.Command{
	Use:   "dag <molecule-id>",
	Short: "Visualize molecule dependency DAG",
	Long: `Display the dependency DAG (Directed Acyclic Graph) for a molecule.

Shows the dependency structure with execution tiers and status:
  ✓ done        - Step completed
  ⧖ in_progress - Step being worked on
  ○ ready       - Step ready to execute (all deps met)
  ◌ blocked     - Step waiting on dependencies

Examples:
  gt mol dag gs-wisp-abc     # Show DAG for molecule
  gt mol dag gs-wisp-abc --json  # JSON output
  gt mol dag gs-wisp-abc --tree  # Tree view (default)
  gt mol dag gs-wisp-abc --tiers # Group by execution tier`,
	Args: cobra.ExactArgs(1),
	RunE: runMoleculeDag,
}

var (
	dagShowTiers bool
	dagTreeView  bool
)

func init() {
	moleculeDagCmd.Flags().BoolVar(&dagShowTiers, "tiers", false, "Group output by execution tier")
	moleculeDagCmd.Flags().BoolVar(&dagTreeView, "tree", true, "Show tree view (default)")
	moleculeDagCmd.Flags().BoolVar(&moleculeJSON, "json", false, "Output as JSON")
}

func runMoleculeDag(cmd *cobra.Command, args []string) error {
	rootID := args[0]

	workDir, err := findLocalBeadsDir()
	if err != nil {
		return fmt.Errorf("not in a beads workspace: %w", err)
	}

	b := beads.New(workDir)

	// Get the root issue
	root, err := b.Show(rootID)
	if err != nil {
		return fmt.Errorf("getting root issue: %w", err)
	}

	// Find all children of the root issue
	children, err := b.List(beads.ListOptions{
		Parent:   rootID,
		Status:   "all",
		Priority: -1,
	})
	if err != nil {
		return fmt.Errorf("listing children: %w", err)
	}

	if len(children) == 0 {
		return fmt.Errorf("no steps found for %s (not a molecule root?)", rootID)
	}

	// Build the DAG
	dag, err := buildDAG(b, root, children)
	if err != nil {
		return fmt.Errorf("building DAG: %w", err)
	}

	// JSON output
	if moleculeJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(dag)
	}

	// Human-readable output
	if dagShowTiers {
		return outputDAGTiers(dag)
	}
	return outputDAGTree(dag)
}

// buildDAG constructs the DAG from molecule children.
func buildDAG(b *beads.Beads, root *beads.Issue, children []*beads.Issue) (*DAGInfo, error) {
	dag := &DAGInfo{
		RootID:    root.ID,
		RootTitle: root.Title,
		Nodes:     make(map[string]*DAGNode),
	}

	// Get IDs for batch fetch
	var stepIDs []string
	for _, child := range children {
		stepIDs = append(stepIDs, child.ID)
	}

	// Fetch full details for all steps
	stepsMap, err := b.ShowMultiple(stepIDs)
	if err != nil {
		return nil, fmt.Errorf("fetching step details: %w", err)
	}

	// Build closed set for status checking
	closedIDs := make(map[string]bool)
	for _, child := range children {
		if child.Status == "closed" {
			closedIDs[child.ID] = true
		}
	}

	// Create nodes
	for _, child := range children {
		step := stepsMap[child.ID]
		if step == nil {
			step = child
		}

		node := &DAGNode{
			ID:     child.ID,
			Title:  child.Title,
			Status: child.Status,
		}

		// Extract dependencies (all blocking types)
		for _, dep := range step.Dependencies {
			if isBlockingDepType(dep.DependencyType) {
				node.Dependencies = append(node.Dependencies, dep.ID)
			}
		}

		// Check if parallel flag is set (from description)
		if strings.Contains(step.Description, "parallel: true") ||
			strings.Contains(step.Description, "parallel=true") {
			node.Parallel = true
		}

		// Compute ready status for open steps
		if child.Status == "open" {
			allDepsClosed := true
			for _, depID := range node.Dependencies {
				if !closedIDs[depID] {
					allDepsClosed = false
					break
				}
			}
			if allDepsClosed {
				node.Status = "ready"
			} else {
				node.Status = "blocked"
			}
		}

		dag.Nodes[child.ID] = node
		dag.TotalNodes++
	}

	// Build dependents (reverse edges)
	for id, node := range dag.Nodes {
		for _, depID := range node.Dependencies {
			if depNode, ok := dag.Nodes[depID]; ok {
				depNode.Dependents = append(depNode.Dependents, id)
			}
		}
	}

	// Compute tiers using topological sort
	computeTiers(dag)

	// Find critical path
	dag.CriticalPath = findCriticalPath(dag)

	return dag, nil
}

// computeTiers assigns execution tiers to each node.
// Tier 0 = nodes with no dependencies, higher tiers depend on lower ones.
//
// Only dependencies on other steps of the molecule order the tiers: a blocker
// outside dag.Nodes is never processed here, so counting it would keep its
// dependent from ever reaching in-degree 0. Nodes that still have unmet
// in-molecule dependencies when no tier can be formed (a dependency cycle) go
// into one trailing tier, so every node is in exactly one tier group.
func computeTiers(dag *DAGInfo) {
	// Calculate in-degrees. Each edge counts once per Dependents entry, so
	// the decrement below matches it.
	inDegree := make(map[string]int)
	for id, node := range dag.Nodes {
		inDegree[id] = 0
		for _, depID := range node.Dependencies {
			if _, ok := dag.Nodes[depID]; ok {
				inDegree[id]++
			}
		}
	}

	// Kahn's algorithm for tier assignment
	currentTier := 0
	processed := 0
	tierGroups := [][]string{}

	for processed < dag.TotalNodes {
		// Find all nodes with in-degree 0 (current tier)
		var tierNodes []string
		for id, degree := range inDegree {
			if degree == 0 {
				tierNodes = append(tierNodes, id)
			}
		}

		if len(tierNodes) == 0 {
			// Cycle detected (shouldn't happen with validated molecules):
			// place the remaining nodes together rather than drop them.
			for id := range inDegree {
				tierNodes = append(tierNodes, id)
			}
			sort.Strings(tierNodes)
			tierGroups = append(tierGroups, tierNodes)
			for _, id := range tierNodes {
				dag.Nodes[id].Tier = currentTier
			}
			currentTier++
			break
		}

		// Sort for deterministic output
		sort.Strings(tierNodes)
		tierGroups = append(tierGroups, tierNodes)

		// Assign tier and remove from graph
		for _, id := range tierNodes {
			dag.Nodes[id].Tier = currentTier
			delete(inDegree, id)
			processed++

			// Decrement in-degree of dependents
			for _, depID := range dag.Nodes[id].Dependents {
				if _, ok := inDegree[depID]; ok {
					inDegree[depID]--
				}
			}
		}

		currentTier++
	}

	dag.Tiers = currentTier
	dag.TierGroups = tierGroups
}

// findCriticalPath finds the longest path through the DAG.
//
// A molecule is validated as acyclic, but a cycle stored in the beads data
// still reaches here, and a plain memo-on-return DFS recurses without bound on
// it. onStack maps each node of the walk in progress to its depth: an edge back
// to one of them closes there instead of descending again, so every path
// returned is finite. A longer branch of a node that also edges into the cycle
// still wins the comparison, so an acyclic DAG draws exactly the path it drew
// before.
//
// A path cut short by a back edge, whether to the node itself or to one above
// it, holds only for the walk that cut it, so it is not memoized: a later visit
// from outside the cycle recomputes it in full. A node whose walk never hit a
// back edge to itself or above it is memoized as before.
func findCriticalPath(dag *DAGInfo) []string {
	// DFS to find longest path
	memo := make(map[string][]string)
	onStack := make(map[string]int)

	// dfs returns the longest path from id and the shallowest stack depth a
	// back edge on it reached (noBackEdge when none did).
	const noBackEdge = math.MaxInt
	var dfs func(id string) ([]string, int)
	dfs = func(id string) ([]string, int) {
		if path, ok := memo[id]; ok {
			return path, noBackEdge
		}
		if depth, ok := onStack[id]; ok {
			// The edge back to id closes a cycle; end the walk at the node
			// that opened it and keep the nodes leading in.
			return nil, depth
		}

		node := dag.Nodes[id]
		if node == nil {
			return nil, noBackEdge
		}

		depth := len(onStack)
		onStack[id] = depth
		reach := noBackEdge
		longestSuffix := []string{}
		for _, depID := range node.Dependents {
			suffix, r := dfs(depID)
			if r < reach {
				reach = r
			}
			if len(suffix) > len(longestSuffix) {
				longestSuffix = suffix
			}
		}
		delete(onStack, id)

		path := append([]string{id}, longestSuffix...)
		if reach > depth {
			memo[id] = path
		}
		return path, reach
	}

	// Find longest path starting from tier 0 nodes
	var criticalPath []string
	for _, tierNodes := range dag.TierGroups {
		for _, id := range tierNodes {
			if dag.Nodes[id].Tier == 0 {
				path, _ := dfs(id)
				if len(path) > len(criticalPath) {
					criticalPath = path
				}
			}
		}
		break // Only check tier 0
	}

	return criticalPath
}

// outputDAGTree outputs the DAG as a tree.
func outputDAGTree(dag *DAGInfo) error {
	fmt.Printf("\n%s %s\n", style.Bold.Render("🌳 DAG:"), dag.RootTitle)
	fmt.Printf("   Root: %s\n", dag.RootID)
	fmt.Printf("   Nodes: %d | Tiers: %d\n", dag.TotalNodes, dag.Tiers)

	if len(dag.CriticalPath) > 0 {
		fmt.Printf("   Critical path: %s\n", strings.Join(dag.CriticalPath, " → "))
	}
	fmt.Println()

	// Build tree structure for display
	// Start with tier 0 nodes (roots)
	if len(dag.TierGroups) > 0 {
		for i, id := range dag.TierGroups[0] {
			isLast := i == len(dag.TierGroups[0])-1
			printNode(dag, id, "", isLast, make(map[string]bool))
		}
	}

	// Legend
	fmt.Println()
	fmt.Printf("   %s done  %s in_progress  %s ready  %s blocked\n",
		style.Bold.Render("✓"), style.Bold.Render("⧖"), style.Bold.Render("○"), style.Dim.Render("◌"))

	return nil
}

// printNode recursively prints a node and its dependents.
func printNode(dag *DAGInfo, id, prefix string, isLast bool, visited map[string]bool) {
	if visited[id] {
		return // Prevent cycles in display
	}
	visited[id] = true

	node := dag.Nodes[id]
	if node == nil {
		return
	}

	// Connector
	connector := "├─"
	if isLast {
		connector = "└─"
	}

	// Status icon
	var icon string
	switch node.Status {
	case "closed":
		icon = style.Bold.Render("✓")
	case "in_progress":
		icon = style.Bold.Render("⧖")
	case "ready":
		icon = style.Bold.Render("○")
	default:
		icon = style.Dim.Render("◌")
	}

	// Parallel marker
	parallelMark := ""
	if node.Parallel {
		parallelMark = " ∥"
	}

	// Print node
	fmt.Printf("%s%s %s %s%s\n", prefix, connector, icon, node.ID, parallelMark)

	// Child prefix
	childPrefix := prefix
	if isLast {
		childPrefix += "   "
	} else {
		childPrefix += "│  "
	}

	// Print dependents (children in the DAG)
	for i, depID := range node.Dependents {
		isLastChild := i == len(node.Dependents)-1
		printNode(dag, depID, childPrefix, isLastChild, visited)
	}
}

// outputDAGTiers outputs the DAG grouped by execution tier.
func outputDAGTiers(dag *DAGInfo) error {
	fmt.Printf("\n%s %s\n", style.Bold.Render("📊 DAG Tiers:"), dag.RootTitle)
	fmt.Printf("   Root: %s\n", dag.RootID)
	fmt.Printf("   Nodes: %d | Tiers: %d\n", dag.TotalNodes, dag.Tiers)
	fmt.Println()

	for tier, nodes := range dag.TierGroups {
		fmt.Printf("   %s Tier %d", style.Bold.Render("─"), tier)
		if tier == 0 {
			fmt.Printf(" (entry)")
		} else if tier == dag.Tiers-1 {
			fmt.Printf(" (exit)")
		}
		fmt.Println()

		for _, id := range nodes {
			node := dag.Nodes[id]
			if node == nil {
				continue
			}

			// Status icon
			var icon string
			switch node.Status {
			case "closed":
				icon = style.Bold.Render("✓")
			case "in_progress":
				icon = style.Bold.Render("⧖")
			case "ready":
				icon = style.Bold.Render("○")
			default:
				icon = style.Dim.Render("◌")
			}

			// Parallel marker
			parallelMark := ""
			if node.Parallel {
				parallelMark = " [parallel]"
			}

			// Dependencies
			depStr := ""
			if len(node.Dependencies) > 0 {
				depStr = fmt.Sprintf(" ← %s", strings.Join(node.Dependencies, ", "))
			}

			fmt.Printf("       %s %s%s%s\n", icon, id, parallelMark, depStr)
		}
		fmt.Println()
	}

	// Critical path
	if len(dag.CriticalPath) > 0 {
		fmt.Printf("   %s %s\n", style.Bold.Render("Critical path:"), strings.Join(dag.CriticalPath, " → "))
	}

	// Legend
	fmt.Println()
	fmt.Printf("   %s done  %s in_progress  %s ready  %s blocked\n",
		style.Bold.Render("✓"), style.Bold.Render("⧖"), style.Bold.Render("○"), style.Dim.Render("◌"))

	return nil
}
