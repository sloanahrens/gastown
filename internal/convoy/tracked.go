package convoy

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// ConvoyLabel marks a bead as a convoy; OwnedLabel marks a convoy whose owner
// paces its own dispatch.
const (
	ConvoyLabel = "gt:convoy"
	OwnedLabel  = "gt:owned"
)

// HasLabel reports whether labels contains target.
func HasLabel(labels []string, target string) bool { //nolint:unparam // target is a label constant today but the API is intentionally general
	for _, l := range labels {
		if l == target {
			return true
		}
	}
	return false
}

// IsConvoyIssue reports whether a bead of issueType with labels is a convoy.
func IsConvoyIssue(issueType string, labels []string) bool {
	return issueType == "convoy" || HasLabel(labels, ConvoyLabel)
}

// ListedConvoy is one convoy as bd list returns it.
type ListedConvoy struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Status      string   `json:"status"`
	CreatedAt   string   `json:"created_at"`
	Description string   `json:"description"`
	IssueType   string   `json:"issue_type"`
	Labels      []string `json:"labels"`
}

// ListConvoys lists convoys in the given status (all statuses when status is
// empty and all is set). It also finds legacy convoys, typed "convoy" without
// the gt:convoy label.
func (t Town) ListConvoys(status string, all bool) ([]ListedConvoy, error) {
	args := []string{"list", "--label=" + ConvoyLabel, "--json", "--limit=0"}
	if status != "" {
		args = append(args, "--status="+status)
	} else if all {
		args = append(args, "--all")
	}

	args = beads.InjectFlatForListJSON(args)
	convoys, err := t.readConvoyIssues(args...)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(convoys))
	for _, convoy := range convoys {
		seen[convoy.ID] = true
	}

	legacyArgs := []string{"list", "--json", "--limit=0"}
	if status != "" {
		legacyArgs = append(legacyArgs, "--status="+status)
	} else if all {
		legacyArgs = append(legacyArgs, "--all")
	}
	legacyArgs = beads.InjectFlatForListJSON(legacyArgs)
	legacy, err := t.readConvoyIssues(legacyArgs...)
	if err != nil {
		return nil, err
	}
	for _, issue := range legacy {
		if seen[issue.ID] || issue.IssueType != "convoy" {
			continue
		}
		convoys = append(convoys, issue)
		seen[issue.ID] = true
	}
	return convoys, nil
}

func (t Town) readConvoyIssues(args ...string) ([]ListedConvoy, error) {
	out, err := t.bdJSON(t.Root, args...)
	if err != nil {
		return nil, err
	}
	var issues []ListedConvoy
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, err
	}
	return issues, nil
}

// TrackedIssue is an issue a convoy tracks.
type TrackedIssue struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	Type      string   `json:"dependency_type"`
	IssueType string   `json:"issue_type"`
	Blocked   bool     `json:"blocked,omitempty"`    // True if issue currently has blockers
	Assignee  string   `json:"assignee,omitempty"`   // Assigned agent (e.g., gastown/polecats/goose)
	Labels    []string `json:"labels,omitempty"`     // Bead labels (propagated from trackedDependency)
	Worker    string   `json:"worker,omitempty"`     // Worker currently assigned (e.g., gastown/nux)
	WorkerAge string   `json:"worker_age,omitempty"` // How long worker has been on this issue
}

// trackedDependency is dep-list data enriched with fresh issue details.
type trackedDependency struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Status         string   `json:"status"`
	IssueType      string   `json:"issue_type"`
	Assignee       string   `json:"assignee"`
	DependencyType string   `json:"dependency_type"`
	Labels         []string `json:"labels"`
	Blocked        bool     `json:"-"`
}

func applyFreshIssueDetails(dep *trackedDependency, details *IssueDetails) {
	dep.Status = strings.TrimSpace(details.Status)
	if dep.Status == "" {
		dep.Status = TrackedStatusUnknown
	}
	dep.Blocked = details.IsBlocked()
	if dep.Title == "" {
		dep.Title = details.Title
	}
	if dep.Assignee == "" {
		dep.Assignee = details.Assignee
	}
	if dep.IssueType == "" {
		dep.IssueType = details.IssueType
	}
	// Always refresh labels unconditionally — bd dep list may return stale
	// labels from dependency records, but bd show returns current bead labels.
	// This ensures isReadyIssue sees accurate queue labels (gt:queued,
	// gt:queue-dispatched) for cross-rig beads. Assigning even when fresh
	// labels are empty clears stale queue labels that would otherwise
	// suppress stranded issue detection.
	dep.Labels = details.Labels
}

// TrackedIssues gets issues tracked by a convoy with fresh cross-rig details.
// Returns issue details including status, type, and worker info.
//
// Prefers raw SQL query against the dependencies table (DepListRawIDs) which
// avoids the JOIN with the issues table that silently drops cross-database
// dependencies (see GH #2624, #2832). Falls back to bd dep list and bd show
// for older bd versions that don't support bd sql.
// Then fetches fresh issue details via bd show with prefix routing.
func (t Town) TrackedIssues(convoyID string) ([]TrackedIssue, error) {
	trackedIDs, err := t.DepListRawIDs(t.Root, convoyID, "down", "tracks")
	if err != nil {
		// bd sql not supported (older bd) — fall back to bd dep list.
		trackedIDs, err = t.depListTracked(convoyID)
		if err != nil {
			return nil, fmt.Errorf("querying tracked issues for %s: %w", convoyID, err)
		}
	}

	// Fallback: when dep queries return empty (common for cross-database deps
	// on older bd where the JOIN fails), try parsing from bd show output.
	if len(trackedIDs) == 0 {
		trackedIDs, err = t.showTrackedDeps(convoyID)
		if err != nil {
			return nil, fmt.Errorf("fallback show for tracked deps of %s: %w", convoyID, err)
		}
	}

	// Drop tracked edges whose target is not a bead ID: no query can resolve
	// one, so it came back as TrackedStatusUnknown and held the convoy open
	// forever (gt-gsky). The dashboard drops the same edge (gt-44z1).
	//
	// A well-formed cross-rig target that is merely unreachable stays unknown
	// and still blocks auto-close — the gt-bs6 contract.
	if len(trackedIDs) > 0 {
		resolvable := make([]string, 0, len(trackedIDs))
		for _, id := range trackedIDs {
			if !beads.IsBeadIDToken(id) {
				t.warnf("convoy %s: ignoring tracked edge to %q (not a bead ID)", convoyID, id)
				continue
			}
			resolvable = append(resolvable, id)
		}
		trackedIDs = resolvable
	}

	if len(trackedIDs) == 0 {
		return nil, nil
	}

	// Fetch fresh issue details via bd show (uses prefix routing for cross-rig).
	// Pinned to the town root rather than ambient/cwd discovery so this agrees
	// with the routing used above for trackedIDs (gt-80o).
	freshDetails := t.IssueDetailsBatch(trackedIDs)

	// Build tracked dependency structs from fresh details. When fresh details
	// are missing (cross-rig DB unreachable, missing, parked, or unroutable
	// from town root), mark the dep with TrackedStatusUnknown so callers can
	// distinguish it from a legitimately open bead. (gt-bs6)
	var deps []trackedDependency
	for _, id := range trackedIDs {
		dep := trackedDependency{
			ID:             id,
			DependencyType: "tracks",
		}
		if details, ok := freshDetails[id]; ok {
			applyFreshIssueDetails(&dep, details)
		} else {
			dep.Status = TrackedStatusUnknown
		}
		deps = append(deps, dep)
	}

	// Collect non-closed issue IDs for worker lookup
	openIssueIDs := make([]string, 0, len(deps))
	for _, dep := range deps {
		if dep.Status != "closed" {
			openIssueIDs = append(openIssueIDs, dep.ID)
		}
	}
	workersMap := t.workersForIssues(openIssueIDs)

	// Build result
	var tracked []TrackedIssue
	for _, dep := range deps {
		info := TrackedIssue{
			ID:        dep.ID,
			Title:     dep.Title,
			Status:    dep.Status,
			Type:      dep.DependencyType,
			IssueType: dep.IssueType,
			Blocked:   dep.Blocked,
			Assignee:  dep.Assignee,
			Labels:    dep.Labels,
		}

		// Add worker info if available
		if worker, ok := workersMap[dep.ID]; ok {
			info.Worker = worker.Worker
			info.WorkerAge = worker.Age
		}

		tracked = append(tracked, info)
	}

	return tracked, nil
}

// depListTracked runs `bd dep list <convoyID> --direction=down --type=tracks --json`
// and returns the tracked issue IDs (unwrapped from external: prefixes).
// Uses --allow-stale for consistency with sling's other bd calls (verifyBeadExists,
// bdShowBead) — without it, a jsonl write that straddles a second boundary causes
// "database out of sync" errors in CI and fast-turnaround production workflows.
func (t Town) depListTracked(convoyID string) ([]string, error) {
	out, err := t.bdJSONAllowStale(t.Root, "dep", "list", convoyID, "--direction=down", "--type=tracks", "--json")
	if err != nil {
		return nil, err
	}

	var results []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &results); err != nil {
		return nil, fmt.Errorf("parsing dep list for %s: %w", convoyID, err)
	}

	seen := make(map[string]bool, len(results))
	var ids []string
	for _, r := range results {
		id := beads.ExtractIssueID(r.ID)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// showTrackedDeps falls back to `bd show <convoyID> --json` and extracts
// tracked dependency IDs from the convoy's dependencies array.
// This handles cross-database dependencies where bd dep list returns empty.
func (t Town) showTrackedDeps(convoyID string) ([]string, error) {
	out, err := t.bdJSON(t.Root, "show", convoyID, "--json")
	if err != nil {
		return nil, err
	}

	var results []struct {
		Dependencies []issueDependency `json:"dependencies"`
	}
	if err := json.Unmarshal(out, &results); err != nil {
		return nil, fmt.Errorf("parsing show for %s: %w", convoyID, err)
	}
	if len(results) == 0 {
		return nil, nil
	}

	seen := make(map[string]bool)
	var ids []string
	for _, dep := range results[0].Dependencies {
		if dep.DependencyType != "tracks" {
			continue
		}
		id := beads.ExtractIssueID(dep.ID)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}

type issueDependency struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	DependencyType string `json:"dependency_type"`
}

// IssueDetails holds basic issue info.
type IssueDetails struct {
	ID             string
	Title          string
	Status         string
	IssueType      string
	Assignee       string
	Labels         []string
	BlockedBy      []string
	BlockedByCount int
	Dependencies   []issueDependency
}

// IsBlocked reports whether the issue has an open blocker.
func (d IssueDetails) IsBlocked() bool {
	if d.BlockedByCount > 0 || len(d.BlockedBy) > 0 {
		return true
	}

	// bd show can omit blocked_by_count; fall back to live dependency edges.
	for _, dep := range d.Dependencies {
		if dep.DependencyType == "blocks" && dep.Status != "closed" && dep.Status != "tombstone" {
			return true
		}
	}

	return false
}

// IssueDetailsBatch fetches details through the central routed beads lookup,
// pinned to the town root instead of the ambient/cwd town, which can silently
// diverge from the caller's town under test isolation or multi-town use.
// Missing or invalid issues are omitted from the map.
func (t Town) IssueDetailsBatch(issueIDs []string) map[string]*IssueDetails {
	result := make(map[string]*IssueDetails, len(issueIDs))
	if len(issueIDs) == 0 {
		return result
	}

	client := t.issueClient()

	issues, err := client.ShowMultiple(issueIDs)
	for id, issue := range issues {
		if details := issueToDetails(issue); details != nil {
			result[id] = details
		}
	}
	if err == nil {
		return result
	}

	// If a grouped batch fails because one ID is missing or stale, keep the
	// previous best-effort behavior and recover any IDs that still resolve.
	for _, id := range issueIDs {
		if result[id] != nil {
			continue
		}
		if details := issueDetailsWithClient(client, id); details != nil {
			result[id] = details
		}
	}

	return result
}

// IssueDetails fetches one issue's details through the routed beads lookup.
func (t Town) IssueDetails(issueID string) *IssueDetails {
	return issueDetailsWithClient(t.issueClient(), issueID)
}

// issueClient returns a beads client pinned to the town root.
func (t Town) issueClient() *beads.Beads {
	// Some callers pass a .beads directory rather than its parent — normalize
	// the same way beads.ResolveBeadsDir does, since beads.Beads uses this as
	// the process cwd for bd invocations, not just for computing the beads dir.
	root := t.rootDir()
	// Ambient discovery effectively resolves symlinks: os.Getwd() after
	// os.Chdir returns the kernel-resolved path on macOS (where the temp dir
	// is a /private symlink). Match that here so an explicit root behaves
	// identically to the ambient path, and bd subprocess env (e.g. BEADS_DIR)
	// agrees with callers that resolved symlinks upfront.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if t.Run != nil {
		return beads.NewWithBeadsDirAndRunner(root, beads.ResolveBeadsDir(root), t.Run)
	}
	return beads.New(root)
}

func issueDetailsWithClient(client *beads.Beads, issueID string) *IssueDetails {
	issue, err := client.Show(issueID)
	if err != nil {
		return nil
	}
	return issueToDetails(issue)
}

func issueToDetails(issue *beads.Issue) *IssueDetails {
	if issue == nil {
		return nil
	}

	deps := make([]issueDependency, 0, len(issue.Dependencies))
	for _, dep := range issue.Dependencies {
		deps = append(deps, issueDependency{
			ID:             dep.ID,
			Status:         dep.Status,
			DependencyType: dep.DependencyType,
		})
	}

	return &IssueDetails{
		ID:             issue.ID,
		Title:          issue.Title,
		Status:         issue.Status,
		IssueType:      issue.Type,
		Assignee:       issue.Assignee,
		Labels:         issue.Labels,
		BlockedBy:      issue.BlockedBy,
		BlockedByCount: issue.BlockedByCount,
		Dependencies:   deps,
	}
}

// workerInfo holds info about a worker assigned to an issue.
type workerInfo struct {
	Worker string // Agent identity (e.g., gastown/nux)
	Age    string // How long assigned (e.g., "12m")
}

// workersForIssues finds workers currently assigned to the given issues.
// Returns a map from issue ID to worker info.
//
// Optimized to batch queries per rig (O(R) instead of O(N×R)) and
// parallelize across rigs.
func (t Town) workersForIssues(issueIDs []string) map[string]*workerInfo {
	result := make(map[string]*workerInfo)
	if len(issueIDs) == 0 {
		return result
	}

	townRoot := t.rootDir()
	if townRoot == "" {
		return result
	}

	// Build a set of target issue IDs for fast lookup
	targetIDs := make(map[string]bool, len(issueIDs))
	for _, id := range issueIDs {
		targetIDs[id] = true
	}

	// Discover rigs with beads directories
	rigDirs, _ := filepath.Glob(filepath.Join(townRoot, "*", "polecats"))
	var beadsDirs []string
	for _, polecatsDir := range rigDirs {
		rigDir := filepath.Dir(polecatsDir)
		beadsDir := filepath.Join(rigDir, "mayor", "rig", ".beads")
		if info, err := os.Stat(beadsDir); err == nil && info.IsDir() {
			beadsDirs = append(beadsDirs, filepath.Join(rigDir, "mayor", "rig"))
		}
	}

	if len(beadsDirs) == 0 {
		return result
	}

	// Query all rigs in parallel using bd list
	type rigResult struct {
		agents []struct {
			ID           string `json:"id"`
			HookBead     string `json:"hook_bead"`
			LastActivity string `json:"last_activity"`
		}
	}

	resultChan := make(chan rigResult, len(beadsDirs))
	var wg sync.WaitGroup

	for _, dir := range beadsDirs {
		wg.Add(1)
		go func(workDir string) {
			defer wg.Done()

			out, err := t.bd("list", "--label=gt:agent", "--status=open", "--include-infra", "--json", "--limit=0", "--flat").
				Dir(workDir).
				StripBeadsDir().
				Stderr(io.Discard).
				Output()
			if err != nil {
				resultChan <- rigResult{}
				return
			}

			var rr rigResult
			if err := json.Unmarshal(out, &rr.agents); err != nil {
				resultChan <- rigResult{}
				return
			}
			resultChan <- rr
		}(dir)
	}

	// Wait for all queries to complete
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Collect results from all rigs, filtering by target issue IDs
	for rr := range resultChan {
		for _, agent := range rr.agents {
			// Only include agents working on issues we care about
			if !targetIDs[agent.HookBead] {
				continue
			}

			// Skip if we already found a worker for this issue
			if _, ok := result[agent.HookBead]; ok {
				continue
			}

			// Parse agent ID to get worker identity
			workerID := parseWorkerFromAgentBead(agent.ID)
			if workerID == "" {
				continue
			}

			// Calculate age from last_activity
			age := ""
			if agent.LastActivity != "" {
				if lastActive, err := time.Parse(time.RFC3339, agent.LastActivity); err == nil {
					age = FormatWorkerAge(time.Since(lastActive))
				}
			}

			result[agent.HookBead] = &workerInfo{
				Worker: workerID,
				Age:    age,
			}
		}
	}

	return result
}

// parseWorkerFromAgentBead extracts worker identity from agent bead ID.
// Input: "gt-gastown-polecat-nux" -> Output: "gastown/polecat/nux"
// Input: "gt-beads-crew-amber" -> Output: "beads/crew/amber"
func parseWorkerFromAgentBead(agentID string) string {
	rig, role, name, ok := beads.ParseAgentBeadID(agentID)
	if !ok {
		return ""
	}

	// Build path from parsed components
	if rig == "" {
		// Town-level
		if name != "" {
			return role + "/" + name
		}
		return role
	}
	if name != "" {
		return rig + "/" + role + "/" + name
	}
	return rig + "/" + role
}

// FormatWorkerAge formats a duration as a short string (e.g., "5m", "2h", "1d")
func FormatWorkerAge(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
