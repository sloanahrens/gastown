package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var readyJSON bool
var readyRig string

var readyCmd = &cobra.Command{
	Use:     "ready",
	GroupID: GroupWork,
	Short:   "Show work ready across town",
	Long: `Display all ready work items across the town and all rigs.

Aggregates ready issues from:
- Town beads (hq-* items: convoys, cross-rig coordination)
- Each rig's beads (project-level issues, MRs)

Ready items have no blockers and can be worked immediately.
Results are sorted by priority (highest first) then by source.

Examples:
  gt ready              # Show all ready work
  gt ready --json       # Output as JSON
  gt ready --rig=gastown  # Show only one rig`,
	RunE: runReady,
}

func init() {
	readyCmd.Flags().BoolVar(&readyJSON, "json", false, "Output as JSON")
	readyCmd.Flags().StringVar(&readyRig, "rig", "", "Filter to a specific rig")
	rootCmd.AddCommand(readyCmd)
}

// ReadySource represents ready items from a single source (town or rig).
type ReadySource struct {
	Name   string         `json:"name"`   // "town" or rig name
	Issues []*beads.Issue `json:"issues"` // Ready issues from this source
	Error  string         `json:"error,omitempty"`
	// Capped is set when the source's ready query came back bounded (bd's
	// default limit of 100): Issues is a page, and TrueCount is what bd's
	// count probe proved exists beyond it (gt-m7pq).
	Capped    bool `json:"capped,omitempty"`
	TrueCount int  `json:"true_count,omitempty"`
}

// ReadyResult is the aggregated result of gt ready.
type ReadyResult struct {
	Sources  []ReadySource `json:"sources"`
	Summary  ReadySummary  `json:"summary"`
	TownRoot string        `json:"town_root,omitempty"`
}

// ReadySummary provides counts for the ready report.
type ReadySummary struct {
	Total    int            `json:"total"`
	BySource map[string]int `json:"by_source"`
	P0Count  int            `json:"p0_count"`
	P1Count  int            `json:"p1_count"`
	P2Count  int            `json:"p2_count"`
	P3Count  int            `json:"p3_count"`
	P4Count  int            `json:"p4_count"`
}

// classifyReadyErr splits a ReadyDispatchable error into what actually
// happened: a capped ready page (ErrReadyTruncated) is data, not a failure —
// ReadyDispatchable's own contract returns the page alongside the sentinel,
// so the caller must still run its filter pipeline over the returned issues.
// classifyReadyErr flags src.Capped (and src.TrueCount, when the probe proved
// one) and reports capped=true so the caller takes the "page is usable"
// branch instead of the "no data, real failure" branch (gt-m7pq).
//
// Any other error means there is no page to show at all, so src.Error is set
// and the caller must not run the filter pipeline over nil/stale issues.
func classifyReadyErr(src *ReadySource, err error) (capped bool) {
	var trunc *beads.ErrReadyTruncated
	if errors.As(err, &trunc) {
		src.Capped = true
		if trunc.TrueCount > 0 {
			src.TrueCount = trunc.TrueCount
		}
		return true
	}
	src.Error = err.Error()
	return false
}

// cappedNote renders the "this is a page, not the board" note for a source
// whose ready query came back capped. Call it only when src.Capped is set.
//
// Every capped source gets a note, including one whose true size the probe
// could not prove (TrueCount unset): a capped source that renders exactly like
// a complete one is the silent under-report, and "unknown" is not "none"
// (gt-m7pq).
func cappedNote(src ReadySource, count int) string {
	switch {
	case src.TrueCount > count:
		return "capped: " + strconv.Itoa(src.TrueCount) + " exist"
	case count == 0:
		return "capped: more exist"
	default:
		return "capped: more than " + strconv.Itoa(count) + " exist"
	}
}

func runReady(cmd *cobra.Command, args []string) error {
	// Find town root
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Load rigs config
	rigsConfigPath := constants.MayorRigsPath(townRoot)
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}

	// Create rig manager and discover rigs
	g := git.NewGit(townRoot)
	mgr := rig.NewManager(townRoot, rigsConfig, g)
	rigs, err := mgr.DiscoverRigs()
	if err != nil {
		return fmt.Errorf("discovering rigs: %w", err)
	}

	// Filter rigs if --rig flag provided
	if readyRig != "" {
		var filtered []*rig.Rig
		for _, r := range rigs {
			if r.Name == readyRig {
				filtered = append(filtered, r)
				break
			}
		}
		if len(filtered) == 0 {
			return fmt.Errorf("rig not found: %s", readyRig)
		}
		rigs = filtered
	}

	// Collect results from all sources in parallel
	var wg sync.WaitGroup
	var mu sync.Mutex
	sources := make([]ReadySource, 0, len(rigs)+1)

	// Fetch town beads (only if not filtering to a specific rig)
	if readyRig == "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			townBeadsPath := beads.GetTownBeadsPath(townRoot)
			townBeads := beads.New(townBeadsPath)
			issues, err := townBeads.ReadyDispatchable()

			mu.Lock()
			defer mu.Unlock()
			src := ReadySource{Name: "town"}
			if err == nil || classifyReadyErr(&src, err) {
				// Filter out formula scaffolds (gt-579)
				formulaNames := getFormulaNames(townBeadsPath)
				filtered := filterFormulaScaffolds(issues, formulaNames)
				// Defense-in-depth: also filter wisps that shouldn't appear in ready work
				wispIDs := getWispIDs(townBeadsPath)
				filtered = filterWisps(filtered, wispIDs)
				// Only show work whose ID routes back to the source that reported it.
				// Otherwise the dashboard can render a row that `gt sling <same-id>`
				// cannot resolve because routes.jsonl points that prefix elsewhere.
				filtered = filterReadyIssuesByRoute(townRoot, "town", filtered)
				// Filter identity beads (agents, roles, rigs) - not actionable work
				filtered = filterIdentityBeads(filtered)
				// Filter the remaining bookkeeping families - rows nobody can pick up
				src.Issues = filterNonDispatchableBeads(filtered)
			}
			sources = append(sources, src)
		}()
	}

	// Fetch from each rig in parallel
	for _, r := range rigs {
		wg.Add(1)
		go func(r *rig.Rig) {
			defer wg.Done()
			// Use rig root path where rig-level beads are stored
			// BeadsPath returns rig root; redirect system handles mayor/rig routing
			rigBeads := beads.New(r.BeadsPath())
			issues, err := rigBeads.ReadyDispatchable()

			mu.Lock()
			defer mu.Unlock()
			src := ReadySource{Name: r.Name}
			if err == nil || classifyReadyErr(&src, err) {
				// Filter out formula scaffolds (gt-579)
				formulaNames := getFormulaNames(r.BeadsPath())
				filtered := filterFormulaScaffolds(issues, formulaNames)
				// Defense-in-depth: also filter wisps that shouldn't appear in ready work
				wispIDs := getWispIDs(r.BeadsPath())
				filtered = filterWisps(filtered, wispIDs)
				// Only show work whose ID routes back to this rig. This keeps the
				// Ready Across Rigs surface honest: every displayed ID must be
				// usable by the stock `gt sling <id> <rig>` command.
				filtered = filterReadyIssuesByRoute(townRoot, r.Name, filtered)
				// Filter identity beads (agents, roles, rigs) - not actionable work
				filtered = filterIdentityBeads(filtered)
				// Filter the remaining bookkeeping families - rows nobody can pick up
				src.Issues = filterNonDispatchableBeads(filtered)
			}
			sources = append(sources, src)
		}(r)
	}

	wg.Wait()

	// Sort sources: town first, then rigs alphabetically
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].Name == "town" {
			return true
		}
		if sources[j].Name == "town" {
			return false
		}
		return sources[i].Name < sources[j].Name
	})

	// Sort issues within each source by priority (lower number = higher priority)
	for i := range sources {
		sort.Slice(sources[i].Issues, func(a, b int) bool {
			return sources[i].Issues[a].Priority < sources[i].Issues[b].Priority
		})
	}

	// Build summary
	summary := ReadySummary{
		BySource: make(map[string]int),
	}
	for _, src := range sources {
		count := len(src.Issues)
		summary.Total += count
		summary.BySource[src.Name] = count
		for _, issue := range src.Issues {
			switch issue.Priority {
			case 0:
				summary.P0Count++
			case 1:
				summary.P1Count++
			case 2:
				summary.P2Count++
			case 3:
				summary.P3Count++
			case 4:
				summary.P4Count++
			}
		}
	}

	result := ReadyResult{
		Sources:  sources,
		Summary:  summary,
		TownRoot: townRoot,
	}

	// Check for source errors
	var failedSources []string
	for _, src := range sources {
		if src.Error != "" {
			failedSources = append(failedSources, src.Name)
		}
	}

	// Output
	if readyJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	if err := printReadyHuman(result); err != nil {
		return err
	}

	// Surface source errors to the user
	if len(failedSources) > 0 {
		if len(failedSources) == len(sources) {
			return fmt.Errorf("all sources failed to load: %s", strings.Join(failedSources, ", "))
		}
		style.PrintWarning("some sources failed to load: %s (results may be incomplete)", strings.Join(failedSources, ", "))
	}

	return nil
}

func printReadyHuman(result ReadyResult) error {
	if result.Summary.Total == 0 {
		fmt.Println("No ready work across town.")
		return nil
	}

	fmt.Printf("%s Ready work across town:\n\n", style.Bold.Render("📋"))

	for _, src := range result.Sources {
		// A capped page is data, not a failure: print the page it is, and
		// say how much more exists so the number next to the name is not
		// mistaken for the whole board (gt-m7pq).
		if src.Error != "" && !src.Capped {
			fmt.Printf("%s %s\n", style.Dim.Render(src.Name+"/"), style.Warning.Render("(error: "+src.Error+")"))
			continue
		}

		count := len(src.Issues)
		if count == 0 {
			if src.Capped {
				fmt.Printf("%s %s\n", style.Dim.Render(src.Name+"/"), style.Warning.Render("("+cappedNote(src, 0)+")"))
			} else {
				fmt.Printf("%s %s\n", style.Dim.Render(src.Name+"/"), style.Dim.Render("(none)"))
			}
			continue
		}

		header := fmt.Sprintf("%s (%d items", style.Bold.Render(src.Name+"/"), count)
		if src.Capped {
			header += ", " + cappedNote(src, count)
		}
		header += ")"
		fmt.Println(header)
		for _, issue := range src.Issues {
			priorityStr := fmt.Sprintf("P%d", issue.Priority)
			var priorityStyled string
			switch issue.Priority {
			case 0:
				priorityStyled = style.Error.Render(priorityStr) // P0 is critical
			case 1:
				priorityStyled = style.Error.Render(priorityStr)
			case 2:
				priorityStyled = style.Warning.Render(priorityStr)
			default:
				priorityStyled = style.Dim.Render(priorityStr)
			}

			// Truncate title if too long
			title := issue.Title
			if len(title) > 60 {
				title = title[:57] + "..."
			}

			fmt.Printf("  [%s] %s %s\n", priorityStyled, style.Dim.Render(issue.ID), title)
		}
		fmt.Println()
	}

	// Summary line
	parts := []string{}
	if result.Summary.P0Count > 0 {
		parts = append(parts, fmt.Sprintf("%d P0", result.Summary.P0Count))
	}
	if result.Summary.P1Count > 0 {
		parts = append(parts, fmt.Sprintf("%d P1", result.Summary.P1Count))
	}
	if result.Summary.P2Count > 0 {
		parts = append(parts, fmt.Sprintf("%d P2", result.Summary.P2Count))
	}
	if result.Summary.P3Count > 0 {
		parts = append(parts, fmt.Sprintf("%d P3", result.Summary.P3Count))
	}
	if result.Summary.P4Count > 0 {
		parts = append(parts, fmt.Sprintf("%d P4", result.Summary.P4Count))
	}

	if len(parts) > 0 {
		fmt.Printf("Total: %d items ready (%s)\n", result.Summary.Total, strings.Join(parts, ", "))
	} else {
		fmt.Printf("Total: %d items ready\n", result.Summary.Total)
	}

	return nil
}

// getFormulaNames reads the formulas directory and returns a set of formula names.
// Formula names are derived from filenames by removing the ".formula.toml" suffix.
func getFormulaNames(beadsPath string) map[string]bool {
	formulasDir := filepath.Join(beadsPath, "formulas")
	entries, err := os.ReadDir(formulasDir)
	if err != nil {
		return nil
	}

	names := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".formula.toml") {
			// Remove suffix to get formula name
			formulaName := strings.TrimSuffix(name, ".formula.toml")
			names[formulaName] = true
		}
	}
	return names
}

// filterFormulaScaffolds removes formula scaffold issues from the list.
// Formula scaffolds are issues whose ID matches a formula name exactly
// or starts with "<formula-name>." (step scaffolds).
func filterFormulaScaffolds(issues []*beads.Issue, formulaNames map[string]bool) []*beads.Issue {
	if formulaNames == nil || len(formulaNames) == 0 {
		return issues
	}

	filtered := make([]*beads.Issue, 0, len(issues))
	for _, issue := range issues {
		// Check if this is a formula scaffold (exact match)
		if formulaNames[issue.ID] {
			continue
		}

		// Check if this is a step scaffold (formula-name.step-id)
		if idx := strings.Index(issue.ID, "."); idx > 0 {
			prefix := issue.ID[:idx]
			if formulaNames[prefix] {
				continue
			}
		}

		filtered = append(filtered, issue)
	}
	return filtered
}

// getWispIDs queries Dolt for wisp IDs that shouldn't appear in ready work.
// Wisps are ephemeral issues (wisp/ephemeral flag) used for operational workflows.
// This is a defense-in-depth exclusion - bd ready should already filter wisps,
// but we double-check at the display layer to ensure operational work doesn't leak.
func getWispIDs(beadsPath string) map[string]bool {
	output, err := BdCmd("mol", "wisp", "list", "--json").
		Dir(beadsPath).
		StripBeadsDir().
		Stderr(io.Discard).
		Output()
	if err != nil {
		return nil // Wisp table may not exist or Dolt unavailable
	}

	// bd mol wisp list --json returns {"wisps": [...], "count": N, ...}
	var wrapper struct {
		Wisps []struct {
			ID string `json:"id"`
		} `json:"wisps"`
	}
	if err := json.Unmarshal(output, &wrapper); err != nil {
		return nil
	}

	wispIDs := make(map[string]bool, len(wrapper.Wisps))
	for _, w := range wrapper.Wisps {
		wispIDs[w.ID] = true
	}
	return wispIDs
}

// filterIdentityBeads removes agent, role, and rig identity beads from the list.
// These are status trackers, not actionable work items.
//
// Since bd ready --json doesn't include labels, we filter by:
//   - issue_type "agent" (agent lifecycle beads)
//   - Labels if present (gt:agent, gt:role, gt:rig)
//   - ID suffix "-role" (role definition beads like hq-crew-role)
//   - ID prefix matching "<prefix>-rig-" (rig identity beads like gt-rig-gastown)
func filterIdentityBeads(issues []*beads.Issue) []*beads.Issue {
	identityLabels := map[string]bool{
		"gt:agent": true,
		"gt:role":  true,
		"gt:rig":   true,
	}

	filtered := make([]*beads.Issue, 0, len(issues))
	for _, issue := range issues {
		// Filter by issue_type (agent beads)
		if beads.IsAgentBead(issue) {
			continue
		}

		// Filter by labels (when available)
		skip := false
		for _, label := range issue.Labels {
			if identityLabels[label] {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		// Filter role definition beads (IDs ending in "-role")
		if strings.HasSuffix(issue.ID, "-role") {
			continue
		}

		// Filter rig identity beads (IDs containing "-rig-")
		if strings.Contains(issue.ID, "-rig-") {
			continue
		}

		// Filter alert records (gt-vwry)
		if isAlertRecord(issue) {
			continue
		}

		filtered = append(filtered, issue)
	}
	return filtered
}

// isAlertRecord reports whether an issue is a record of an alert rather than
// work someone can pick up: an escalation, an escalation delivery copy, or a
// stale record left by an alert producer before those producers keyed and
// auto-closed their alerts (gt-vwry).
//
// These are excluded from the dashboard Ready list for the same reason agent
// and rig identity beads are: they have no owner and nobody can "do" them.
// They outrank real work (a critical escalation is a P0), so when a recurring
// condition fired on every patrol cycle they crowded the top of the list and
// the mayor, reading it as a queue of urgent work, declined to dispatch for
// hours. Escalations remain visible where they belong — `gt escalate list`,
// the mailbox, and the source bead's comments.
//
// Detection is by label where labels are present, falling back to the
// escalation title envelope ("[HIGH] ..."), because bd ready --json does not
// always populate labels and the records predating the label already exist.
func isAlertRecord(issue *beads.Issue) bool {
	for _, label := range issue.Labels {
		switch {
		case label == "gt:escalation",
			label == "msg-type:escalation",
			strings.HasPrefix(label, "escalation-fp:"):
			return true
		}
	}
	return isEscalationTitle(issue.Title)
}

// isEscalationTitle matches the title envelope gt escalate puts on both the
// escalation bead and its delivery mail copy: "[<SEVERITY>] <description>".
func isEscalationTitle(title string) bool {
	for _, sev := range []string{"CRITICAL", "HIGH", "MEDIUM", "LOW"} {
		if strings.HasPrefix(title, "["+sev+"] ") {
			return true
		}
	}
	return false
}

// filterReadyIssuesByRoute keeps only issues whose prefix route matches the
// source that reported them. Ready rows are actionable: the dashboard renders a
// Sling button for each row, so the displayed ID must resolve through the same
// routes.jsonl path that produced it.
func filterReadyIssuesByRoute(townRoot, source string, issues []*beads.Issue) []*beads.Issue {
	if townRoot == "" {
		return issues
	}

	filtered := make([]*beads.Issue, 0, len(issues))
	for _, issue := range issues {
		if readyIssueRoutesToSource(townRoot, source, issue.ID) {
			filtered = append(filtered, issue)
		}
	}
	return filtered
}

func readyIssueRoutesToSource(townRoot, source, issueID string) bool {
	prefix := beads.ExtractPrefix(issueID)
	if prefix == "" {
		return false
	}

	routePath := beads.GetRigPathForPrefix(townRoot, prefix)
	if routePath == "" {
		return false
	}

	if source == "town" {
		return routePath == townRoot
	}

	return beads.GetRigNameForPrefix(townRoot, prefix) == source
}

// filterNonDispatchableBeads removes the bead families that record town
// runtime rather than work: mail, escalations, identity, merge requests and
// slots, queues, and the deacon's event records (gt-b9wq).
//
// A ready row is a button, not a status. The dashboard draws a Sling control
// beside every ID this list emits, so a row that no polecat can take is a
// button that cannot work. Membership is beads.IsNonDispatchableBead, shared
// with `gt daemon dispatch-check`, so the board and the nudge it triggers
// cannot disagree about what work is.
//
// This is a backstop, not the primary enforcement: both callers already
// fetch through Beads.ReadyDispatchable(), which asks bd to exclude the same
// label/type families server-side before it ever builds the response, so
// this loop does not depend on labels surviving into what the caller
// received. It still matters on its own — the title-prefix families
// (Compaction Report, HANDOFF, the merge-slot title) have no label at all,
// so bd's --exclude-label/--exclude-type cannot catch them.
func filterNonDispatchableBeads(issues []*beads.Issue) []*beads.Issue {
	filtered := make([]*beads.Issue, 0, len(issues))
	for _, issue := range issues {
		if beads.IsNonDispatchableBead(issue) {
			continue
		}
		filtered = append(filtered, issue)
	}
	return filtered
}

// filterWisps removes wisp issues from the list.
// Wisps are ephemeral operational work that shouldn't appear in ready work.
func filterWisps(issues []*beads.Issue, wispIDs map[string]bool) []*beads.Issue {
	if wispIDs == nil || len(wispIDs) == 0 {
		return issues
	}

	filtered := make([]*beads.Issue, 0, len(issues))
	for _, issue := range issues {
		if !wispIDs[issue.ID] {
			filtered = append(filtered, issue)
		}
	}
	return filtered
}
