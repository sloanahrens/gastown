// Package convoy provides convoy tracking operations: finding tracking convoys,
// checking completion, feeding ready issues, and dispatching via gt sling.
package convoy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/util"
)

// CheckConvoysForIssue finds any convoys tracking the given issue and triggers
// convoy completion checks. If the convoy is not complete, it reactively feeds
// the next ready issue to keep the convoy progressing without waiting for
// polling-based patrol cycles.
//
// The check is idempotent - running it multiple times for the same issue is safe.
// The underlying `gt convoy check` handles already-closed convoys gracefully.
//
// Parameters:
//   - ctx: context for storage operations
//   - store: beads storage for dependency/issue queries (nil skips convoy checks)
//   - townRoot: path to the town root directory
//   - issueID: the issue ID that was just closed
//   - caller: identifier for logging (e.g., "Convoy")
//   - logger: optional logger function (can be nil)
//   - gtPath: resolved path to the gt binary (e.g. from exec.LookPath or daemon config)
//   - resolver: optional StoreResolver for cross-database issue resolution (nil falls back to subprocess)
//
// Returns the convoy IDs that were checked (may be empty if issue isn't tracked).
func CheckConvoysForIssue(ctx context.Context, store beadsdk.Storage, townRoot, issueID, caller string, logger func(format string, args ...interface{}), gtPath string, isRigParked func(string) bool, resolver ...*StoreResolver) []string {
	if logger == nil {
		logger = func(format string, args ...interface{}) {} // no-op
	}
	if isRigParked == nil {
		isRigParked = func(string) bool { return false }
	}
	if store == nil {
		return nil
	}

	// Extract optional resolver (variadic for backward compatibility)
	var res *StoreResolver
	if len(resolver) > 0 {
		res = resolver[0]
	}

	// Find convoys tracking this issue
	convoyIDs := getTrackingConvoys(ctx, store, issueID, logger)
	if len(convoyIDs) == 0 {
		return nil
	}

	logger("%s: %s tracked by %d convoy(s): %v", caller, issueID, len(convoyIDs), convoyIDs)

	// Run convoy check for each tracking convoy
	// Note: gt convoy check is idempotent and handles already-closed convoys
	for _, convoyID := range convoyIDs {
		if isConvoyClosed(ctx, store, convoyID) {
			logger("%s: convoy %s already closed, skipping", caller, convoyID)
			continue
		}

		if isConvoyStaged(ctx, store, convoyID) {
			logger("%s: convoy %s is staged (not yet launched), skipping", caller, convoyID)
			continue
		}

		logger("%s: checking convoy %s", caller, convoyID)
		if err := runConvoyCheck(ctx, townRoot, convoyID, gtPath); err != nil {
			logger("%s: convoy %s check failed: %s", caller, convoyID, util.FirstLine(err.Error()))
		}

		// Continuation feed: if convoy is still open after the completion check,
		// reactively dispatch the next ready issue. This makes convoy feeding
		// event-driven instead of relying on polling-based patrol cycles.
		if !isConvoyClosed(ctx, store, convoyID) {
			feedNextReadyIssue(ctx, store, townRoot, convoyID, caller, logger, gtPath, isRigParked, res)
		}
	}

	return convoyIDs
}

// getTrackingConvoys returns convoy IDs that track the given issue.
// Uses SDK GetDependentsWithMetadata filtered by type "tracks".
func getTrackingConvoys(ctx context.Context, store beadsdk.Storage, issueID string, logger func(format string, args ...interface{})) []string {
	dependents, err := store.GetDependentsWithMetadata(ctx, issueID)
	if err != nil {
		if logger != nil {
			logger("Convoy: getTrackingConvoys(%s) store error: %v", issueID, err)
		}
		return nil
	}

	convoyIDs := make([]string, 0)
	for _, d := range dependents {
		if string(d.DependencyType) == "tracks" {
			convoyIDs = append(convoyIDs, d.ID)
		}
	}
	return convoyIDs
}

// isConvoyClosed checks if a convoy is already closed.
func isConvoyClosed(ctx context.Context, store beadsdk.Storage, convoyID string) bool {
	issue, err := store.GetIssue(ctx, convoyID)
	if err != nil || issue == nil {
		return false
	}
	return string(issue.Status) == "closed"
}

// isConvoyStaged checks if a convoy is in a staged state (not yet launched).
// Staged convoys have statuses like "staged_ready" or "staged_warnings".
// They should not be fed until they are launched (transitioned to "open").
func isConvoyStaged(ctx context.Context, store beadsdk.Storage, convoyID string) bool {
	issue, err := store.GetIssue(ctx, convoyID)
	if err != nil || issue == nil {
		return false // fail-open: if we can't read, assume not staged
	}
	return strings.HasPrefix(string(issue.Status), "staged_")
}

// runConvoyCheck runs `gt convoy check <convoy-id>` to check a specific convoy.
// This is idempotent and handles already-closed convoys gracefully.
// The context parameter enables cancellation on daemon shutdown.
// gtPath is the resolved path to the gt binary.
func runConvoyCheck(ctx context.Context, townRoot, convoyID, gtPath string) error {
	cmd := exec.CommandContext(ctx, gtPath, "convoy", "check", convoyID)
	cmd.Dir = townRoot
	util.SetProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, stderr.String())
	}

	return nil
}

// trackedIssue holds basic info about an issue tracked by a convoy.
type trackedIssue struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Assignee  string `json:"assignee"`
	Priority  int    `json:"priority"`
	IssueType string `json:"issue_type"`
}

// slingableTypes are bead types that can be dispatched via gt sling.
// Only leaf work items are slingable — containers (epic) and non-work types
// (decision, message, event) are excluded. Unknown/empty types are treated
// as slingable (beads default to "task" when IssueType is empty).
var slingableTypes = map[string]bool{
	"task":    true,
	"bug":     true,
	"feature": true,
	"chore":   true,
	"":        true, // Empty type defaults to task
}

// IsSlingableType reports whether a bead type can be dispatched via gt sling.
// Exported for use by cmd/convoy.go stranded scan path.
func IsSlingableType(issueType string) bool {
	return slingableTypes[issueType]
}

// blockingDepTypes are dependency types that prevent an issue from being
// dispatched. parent-child is intentionally excluded — a child task is
// dispatchable even if its parent epic is open (consistent with molecule
// step behavior in internal/cmd/molecule_step.go).
var blockingDepTypes = map[string]bool{
	"blocks":             true,
	"conditional-blocks": true,
	"waits-for":          true,
	"merge-blocks":       true,
}

// isIssueBlocked reports whether issueID has a blocking dependency that is
// not satisfied; BlockReason says which.
func isIssueBlocked(ctx context.Context, store beadsdk.Storage, issueID string, resolver *StoreResolver) bool {
	return BlockReason(ctx, store, issueID, resolver) != ""
}

// BlockReason returns why issueID may not be dispatched because of its
// dependencies, or "" when none blocks it. A blocks, conditional-blocks,
// waits-for or merge-blocks dependency blocks unless its target is closed or
// tombstoned; a merge-blocks target must also carry a CloseReason starting
// with "Merged in ", so work is not dispatched against un-merged code (#1893).
// parent-child does not block.
//
// The edges come from the issue's raw dependency records in its home store
// (the resolver redirects a rig bead there). The joined view,
// GetDependenciesWithMetadata, drops every edge whose target is not in that
// store — every blocker in another rig, stored as "external:<prefix>:<id>" —
// and read that way a bead blocked by another rig's open bead was fed
// (gt-j02xy). Each blocker's status is then read in the rig that owns its
// prefix, falling back to the home store.
//
// This fails safe. Each of these counts as blocking, with a reason naming it:
//   - the bead's own rig store cannot be reached (reading the town store
//     instead finds none of its edges and would say "not blocked");
//   - the dependency records cannot be read, or the store cannot give raw
//     records at all;
//   - a blocker's rig store cannot be opened or read ("unreadable");
//   - no store has the blocker ("unresolved in any rig").
//
// The one exception is no store at all, which is the town-level gap the store
// alert reports (FeedHold makes the same call).
//
// store is the caller's town store. It answers for hq when the resolver holds
// no hq store.
func BlockReason(ctx context.Context, store beadsdk.Storage, issueID string, resolver *StoreResolver) string {
	return BlockOf(ctx, store, issueID, resolver).Reason
}

// BlockCause says why a bead is held when the hold is a failure to know rather
// than a blocker that is simply still open.
type BlockCause string

const (
	// BlockOpen is the ordinary hold: a blocker was read and is not closed.
	BlockOpen BlockCause = ""

	// BlockUnresolved is a blocker that no store has: a dangling external edge.
	// Nothing in beads removes one when its target is hard-deleted, because
	// external edges have no foreign key, so it holds the bead until an
	// operator drops the edge.
	BlockUnresolved BlockCause = "unresolved"

	// BlockUnreadable is a store that would not answer: the bead's own, or the
	// one that owns a blocker. It clears by itself when the store comes back.
	BlockUnreadable BlockCause = "unreadable"
)

// Block is BlockReason's verdict with the parts a caller can act on.
type Block struct {
	// Reason is the BlockReason text; "" when nothing blocks the bead.
	Reason string

	// Cause is BlockOpen for an open blocker, and otherwise says which
	// fail-safe hold this is.
	Cause BlockCause

	// BlockerID names the blocker behind an unresolved or unreadable hold. It
	// is "" when the read that failed was the bead's own, which has no blocker
	// to name yet.
	BlockerID string
}

// Held reports whether the verdict is a fail-safe hold, which says nothing
// about the blockers themselves, rather than an open blocker or no block.
func (b Block) Held() bool {
	return b.Cause == BlockUnresolved || b.Cause == BlockUnreadable
}

// BlockOf is BlockReason with the hold's cause and blocker id alongside the
// reason, for a caller that escalates a fail-safe hold.
func BlockOf(ctx context.Context, store beadsdk.Storage, issueID string, resolver *StoreResolver) Block {
	storeFor := func(name string) (beadsdk.Storage, error) {
		if resolver == nil {
			return store, nil
		}
		found, err := resolver.storeByName(name)
		if found != nil {
			return found, nil
		}
		if name == "hq" && store != nil {
			return store, nil
		}
		return nil, err
	}

	home := store
	if resolver != nil {
		name := resolver.storeForID(issueID)
		var err error
		if home, err = storeFor(name); err != nil {
			return Block{Reason: fmt.Sprintf("store of rig %s unavailable (%s)", name, util.FirstLine(err.Error())), Cause: BlockUnreadable}
		}
	}
	if home == nil {
		return Block{}
	}

	reader, ok := home.(dependencyRecordReader)
	if !ok {
		return Block{Reason: fmt.Sprintf("store %T cannot read raw dependency records, so cross-rig blockers are unknown", home), Cause: BlockUnreadable}
	}
	records, err := reader.GetDependencyRecords(ctx, issueID)
	if err != nil {
		return Block{Reason: "dependency records unreadable (" + util.FirstLine(err.Error()) + ")", Cause: BlockUnreadable}
	}

	type blocker struct{ id, depType string }
	var blockers []blocker
	for _, d := range records {
		depType := string(d.Type)
		if !blockingDepTypes[depType] {
			continue
		}
		blockers = append(blockers, blocker{id: extractIssueID(d.DependsOnID), depType: depType})
	}
	if len(blockers) == 0 {
		return Block{}
	}

	found := make(map[string]*beadsdk.Issue, len(blockers))
	readErr := make(map[string]string)
	lookup := func(s beadsdk.Storage, ids []string) error {
		issues, err := s.GetIssuesByIDs(ctx, ids)
		if err != nil {
			return err
		}
		for _, iss := range issues {
			if iss != nil {
				found[iss.ID] = iss
			}
		}
		return nil
	}

	// Each blocker is read in the store that owns its prefix.
	byStore := make(map[string][]string)
	for _, b := range blockers {
		name := ""
		if resolver != nil {
			name = resolver.storeForID(b.id)
		}
		byStore[name] = append(byStore[name], b.id)
	}
	for name, ids := range byStore {
		owner, err := storeFor(name)
		if err == nil && owner == nil {
			err = fmt.Errorf("no store for %s", name)
		}
		if err == nil {
			err = lookup(owner, ids)
		}
		if err != nil {
			for _, id := range ids {
				readErr[id] = util.FirstLine(err.Error())
			}
		}
	}
	// A blocker its owner did not produce may still sit beside the bead or in
	// the town store.
	for _, fallback := range []beadsdk.Storage{home, store} {
		var missing []string
		for _, b := range blockers {
			if found[b.id] == nil {
				missing = append(missing, b.id)
			}
		}
		if len(missing) == 0 || fallback == nil {
			continue
		}
		if err := lookup(fallback, missing); err != nil {
			for _, id := range missing {
				if readErr[id] == "" {
					readErr[id] = util.FirstLine(err.Error())
				}
			}
		}
	}

	for _, b := range blockers {
		iss := found[b.id]
		if iss == nil {
			if msg := readErr[b.id]; msg != "" {
				return Block{Reason: fmt.Sprintf("%s blocker %s unreadable (%s)", b.depType, b.id, msg), Cause: BlockUnreadable, BlockerID: b.id}
			}
			return Block{Reason: fmt.Sprintf("%s blocker %s unresolved in any rig", b.depType, b.id), Cause: BlockUnresolved, BlockerID: b.id}
		}
		switch status := string(iss.Status); status {
		case "tombstone":
			continue
		case "closed":
			if b.depType == "merge-blocks" && !strings.HasPrefix(iss.CloseReason, "Merged in ") {
				return Block{Reason: fmt.Sprintf("merge-blocks %s closed without a merge", b.id)}
			}
			continue
		default:
			return Block{Reason: fmt.Sprintf("%s %s (%s)", b.depType, b.id, status)}
		}
	}
	return Block{}
}

// feedNextReadyIssue finds the next ready issue in a convoy and dispatches it
// via gt sling. A ready issue is one that is open, with no assignee, and not
// blocked by unclosed dependencies. This provides reactive (event-driven)
// convoy feeding instead of waiting for polling-based patrol cycles.
//
// Only one issue is dispatched per call. When that issue completes, the
// next close event triggers another feed cycle.
// gtPath is the resolved path to the gt binary.
func feedNextReadyIssue(ctx context.Context, store beadsdk.Storage, townRoot, convoyID, caller string, logger func(format string, args ...interface{}), gtPath string, isRigParked func(string) bool, resolver *StoreResolver) {
	// The operator's town-wide hold parks every automatic dispatcher
	// (gt-ifijm). Checked before the store is read: nothing below matters
	// while the town is held, and the next close event after the hold lifts
	// feeds the convoy.
	if reason := dispatch.OperatorHold(townRoot); reason != "" {
		logger("%s: convoy %s: not feeding: %s", caller, convoyID, reason)
		return
	}

	tracked := getConvoyTrackedIssues(ctx, store, convoyID, townRoot, resolver, logger)
	if len(tracked) == 0 {
		return
	}

	// Extract base_branch, agent, and formula from convoy description fields
	var baseBranch, convoyAgent, convoyFormula string
	if convoy, err := store.GetIssue(ctx, convoyID); err == nil && convoy != nil {
		if cf := beads.ParseConvoyFields(&beads.Issue{Description: convoy.Description}); cf != nil {
			baseBranch = cf.BaseBranch
		}
		convoyAgent = AgentFromConvoyDescription(convoy.Description)
		convoyFormula = FormulaFromConvoyDescription(convoy.Description)
	}

	// Sort by priority (lower = higher) then by ID for deterministic tie-breaking.
	sort.Slice(tracked, func(i, j int) bool {
		if tracked[i].Priority != tracked[j].Priority {
			return tracked[i].Priority < tracked[j].Priority
		}
		return tracked[i].ID < tracked[j].ID
	})

	// Find the first ready issue (open, no assignee, not blocked).
	for _, issue := range tracked {
		if issue.Status != "open" || issue.Assignee != "" {
			continue
		}

		// Filter non-slingable types: only leaf work items (task, bug,
		// feature, chore) can be dispatched. Epics, convoys, and other
		// container types are skipped.
		if !IsSlingableType(issue.IssueType) {
			logger("%s: convoy %s: %s has non-slingable type %q, skipping", caller, convoyID, issue.ID, issue.IssueType)
			continue
		}

		// Check blocking dependencies: blocks and conditional-blocks with
		// non-closed targets prevent dispatch. parent-child is NOT treated
		// as blocking (consistent with molecule step behavior).
		if reason := BlockReason(ctx, store, issue.ID, resolver); reason != "" {
			logger("%s: convoy %s: %s is blocked (%s), skipping", caller, convoyID, issue.ID, reason)
			continue
		}

		// Determine target rig from issue prefix
		rig := rigForIssue(townRoot, issue.ID)
		if rig == "" {
			logger("%s: convoy %s: cannot determine rig for issue %s, skipping", caller, convoyID, issue.ID)
			continue
		}

		if isRigParked(rig) {
			logger("%s: convoy %s: rig %s is parked, skipping %s", caller, convoyID, rig, issue.ID)
			continue
		}

		// A per-rig ESTOP holds this rig's issues; the town hold was
		// answered at the top (gt-ifijm).
		if reason := dispatch.RigHold(townRoot, rig); reason != "" {
			logger("%s: convoy %s: not feeding %s: %s", caller, convoyID, issue.ID, reason)
			continue
		}

		// A hold recorded on the bead takes it off this path too: the
		// continuation feed would sling it with the rig default agent, which is
		// what a routing label or a keep-off decision forbids (gt-tq6l). A
		// merge rejection on record holds it as well: a rejected bead is
		// reopened to exactly the open, unassigned state this loop feeds, and
		// its redispatch is the deacon's (gt-ghyfx).
		if hold := FeedHold(ctx, store, issue.ID, resolver); hold.Reason != "" {
			logger("%s: convoy %s: %s not dispatched: %s", caller, convoyID, issue.ID, hold.Reason)
			continue
		}

		// A sling that just failed at session start left this bead in the
		// state this loop feeds; retrying at once fails the same way and
		// crowds out the operator's own retry (gt-wacl).
		if reason := dispatch.StartupBackoff(townRoot, issue.ID); reason != "" {
			logger("%s: convoy %s: %s not dispatched: %s", caller, convoyID, issue.ID, reason)
			continue
		}

		agent, agentDesc := FeedDispatchAgent(convoyAgent, townRoot, rig)
		logger("%s: convoy %s: feeding next ready issue %s to %s (%s)", caller, convoyID, issue.ID, rig, agentDesc)
		if convoyFormula != "" {
			logger("%s: convoy %s: feeding %s with formula %q recorded on convoy at sling time", caller, convoyID, issue.ID, convoyFormula)
		}
		if err := dispatchIssue(ctx, townRoot, issue.ID, rig, gtPath, baseBranch, agent, convoyFormula); err != nil {
			logger("%s: convoy %s: dispatch %s failed: %s", caller, convoyID, issue.ID, util.FirstLine(err.Error()))
			continue // Try next issue on dispatch failure
		}
		return // Successfully dispatched one issue
	}

	logger("%s: convoy %s: no ready issues to feed", caller, convoyID)
}

// getConvoyTrackedIssues returns issues tracked by a convoy with fresh status.
// Reads the convoy's raw tracks dependency records, then GetIssuesByIDs for
// current status of the ones in this store. When a StoreResolver is provided,
// cross-rig beads are resolved via direct store queries. Otherwise falls back
// to bd show subprocess via fetchCrossRigBeadStatus.
//
// The raw records, not GetDependenciesWithMetadata: that joins each target to
// an issue row in this store and silently drops every target that has none,
// which is every cross-rig bead ("external:<prefix>:<id>") a town convoy
// tracks. With it the cross-rig resolution below could never run, and such a
// convoy fed nothing.
func getConvoyTrackedIssues(ctx context.Context, store beadsdk.Storage, convoyID, townRoot string, resolver *StoreResolver, logger func(format string, args ...interface{})) []trackedIssue {
	ids, err := trackedIDs(ctx, store, convoyID)
	if err != nil {
		logger("convoy %s: cannot read tracked beads: %s", convoyID, util.FirstLine(err.Error()))
		return nil
	}
	if len(ids) == 0 {
		return nil
	}

	// Refresh status via GetIssuesByIDs for cross-rig accuracy
	freshIssues, err := store.GetIssuesByIDs(ctx, ids)
	if err != nil {
		freshIssues = nil
	}

	freshMap := make(map[string]*beadsdk.Issue)
	for _, iss := range freshIssues {
		if iss != nil {
			freshMap[iss.ID] = iss
		}
	}

	// Cross-rig resolution: for beads not found in the local store (e.g., ds-*
	// beads when convoys live in hq), resolve via the StoreResolver which
	// queries the appropriate rig store directly. Falls back to bd show
	// subprocess if no resolver is available. See GH #2624.
	var missingIDs []string
	for _, id := range ids {
		if _, ok := freshMap[id]; !ok {
			missingIDs = append(missingIDs, id)
		}
	}
	if len(missingIDs) > 0 {
		if resolver != nil {
			// Direct store queries — faster, no subprocess, no bd dependency
			crossRigFresh := resolver.ResolveIssues(ctx, missingIDs)
			for id, fresh := range crossRigFresh {
				freshMap[id] = fresh
			}
		} else if townRoot != "" {
			// Legacy fallback: subprocess bd show per rig
			crossRigFresh := fetchCrossRigBeadStatus(townRoot, missingIDs)
			for id, fresh := range crossRigFresh {
				freshMap[id] = fresh
			}
		}
	}

	result := make([]trackedIssue, 0, len(ids))
	for _, id := range ids {
		t := trackedIssue{ID: id}
		if fresh := freshMap[id]; fresh != nil {
			t.Status = string(fresh.Status)
			t.Assignee = fresh.Assignee
			t.Priority = fresh.Priority
			t.IssueType = string(fresh.IssueType)
		}
		result = append(result, t)
	}

	return result
}

// dependencyRecordReader reads a bead's raw dependency edges, whatever their
// target. beadsdk.Storage does not carry it; the Dolt store every beadsdk.Open
// returns does.
type dependencyRecordReader interface {
	GetDependencyRecords(ctx context.Context, issueID string) ([]*beadsdk.Dependency, error)
}

// trackedIDs returns the IDs of the beads convoyID tracks, cross-rig ones
// included, with any external:<prefix>: wrapper stripped.
func trackedIDs(ctx context.Context, store beadsdk.Storage, convoyID string) ([]string, error) {
	reader, ok := store.(dependencyRecordReader)
	if !ok {
		// The joined view is no substitute: it lists local targets alone and
		// would silently drop every cross-rig tracked bead.
		return nil, fmt.Errorf("store %T cannot read raw dependency records", store)
	}
	var ids []string
	deps, err := reader.GetDependencyRecords(ctx, convoyID)
	if err != nil {
		return nil, err
	}
	for _, d := range deps {
		if string(d.Type) == "tracks" {
			ids = append(ids, extractIssueID(d.DependsOnID))
		}
	}
	return ids, nil
}

// extractIssueID strips the external:prefix:id wrapper from bead IDs.
func extractIssueID(id string) string {
	if strings.HasPrefix(id, "external:") {
		parts := strings.SplitN(id, ":", 3)
		if len(parts) == 3 {
			return parts[2]
		}
	}
	return id
}

// rigForIssue determines the rig name for an issue based on its ID prefix.
// Uses the beads routes to map prefixes to rigs.
func rigForIssue(townRoot, issueID string) string {
	prefix := beads.ExtractPrefix(issueID)
	if prefix == "" {
		return ""
	}
	return beads.GetRigNameForPrefix(townRoot, prefix)
}

// fetchCrossRigBeadStatus fetches fresh status for beads that live in other rigs.
// Groups IDs by prefix, resolves each prefix to its rig directory via routes,
// and runs `bd show --json <ids>` per rig. Pattern from batchFetchBeadInfoByIDs
// in capacity_dispatch.go.
func fetchCrossRigBeadStatus(townRoot string, ids []string) map[string]*beadsdk.Issue {
	result := make(map[string]*beadsdk.Issue)
	if len(ids) == 0 {
		return result
	}

	// Group IDs by prefix
	byPrefix := make(map[string][]string)
	for _, id := range ids {
		prefix := beads.ExtractPrefix(id)
		if prefix != "" {
			byPrefix[prefix] = append(byPrefix[prefix], id)
		}
	}

	for prefix, prefixIDs := range byPrefix {
		rigPath := beads.GetRigPathForPrefix(townRoot, prefix)
		if rigPath == "" {
			continue
		}

		args := append([]string{"show", "--json"}, prefixIDs...)
		cmd := beads.CommandWithEnv(rigPath, nil, args...)
		util.SetDetachedProcessGroup(cmd)
		out, err := cmd.Output()
		if err != nil {
			continue
		}

		var items []struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Assignee string `json:"assignee"`
			Priority int    `json:"priority"`
			Type     string `json:"issue_type"`
		}
		if err := json.Unmarshal(out, &items); err != nil {
			continue
		}
		for _, item := range items {
			result[item.ID] = &beadsdk.Issue{
				ID:        item.ID,
				Status:    beadsdk.Status(item.Status),
				Assignee:  item.Assignee,
				Priority:  item.Priority,
				IssueType: beadsdk.IssueType(item.Type),
			}
		}
	}

	return result
}

// FireCrossRigDepNotifications checks if any issues in other rigs were unblocked
// by the closure of closedIssueID. For each affected rig, sends a nudge to that
// rig's witness so it can react to the resolved dependency.
//
// Cross-rig deps are stored in the dependent issue's store as "external:<prefix>:<id>".
// To find them, this function queries each rig store using the external-wrapped form
// of the closed issue ID.
//
// This is best-effort: failures are silently logged. The closed issue's own store
// is skipped (same-rig deps don't need cross-rig notification).
func FireCrossRigDepNotifications(ctx context.Context, closedIssueID, townRoot string, stores map[string]beadsdk.Storage, logger func(format string, args ...interface{})) {
	if logger == nil {
		logger = func(format string, args ...interface{}) {}
	}
	if len(stores) == 0 || closedIssueID == "" || townRoot == "" {
		return
	}

	// Determine the home store of the closed issue so we can skip it.
	closedPrefix := beads.ExtractPrefix(closedIssueID)
	if closedPrefix == "" {
		return
	}
	closedRig := beads.GetRigNameForPrefix(townRoot, closedPrefix)
	closedStoreKey := closedRig
	if closedStoreKey == "" {
		closedStoreKey = "hq"
	}

	// Build the external-wrapped form used when storing cross-rig dep records:
	// "external:<prefix-without-trailing-dash>:<id>"
	externalID := fmt.Sprintf("external:%s:%s", strings.TrimSuffix(closedPrefix, "-"), closedIssueID)

	// Track which rigs have already been notified to avoid duplicate nudges.
	notifiedRigs := make(map[string]bool)

	for storeName, store := range stores {
		if storeName == closedStoreKey {
			continue // skip the closed issue's own store
		}

		dependents, err := store.GetDependentsWithMetadata(ctx, externalID)
		if err != nil || len(dependents) == 0 {
			continue
		}

		for _, dep := range dependents {
			if dep == nil {
				continue
			}
			depType := string(dep.DependencyType)
			if !blockingDepTypes[depType] {
				continue
			}

			// Determine the rig for the dependent issue.
			depID := extractIssueID(dep.ID)
			depPrefix := beads.ExtractPrefix(depID)
			if depPrefix == "" {
				continue
			}
			depRig := beads.GetRigNameForPrefix(townRoot, depPrefix)
			if depRig == "" || depRig == closedRig {
				continue
			}
			if notifiedRigs[depRig] {
				continue
			}
			notifiedRigs[depRig] = true

			depTitle := dep.Title
			logger("CrossRig: %s closed, unblocking %s (%s) — nudging %s/witness", closedIssueID, depID, depRig, depRig)

			msg := fmt.Sprintf("Dependency resolved: %s — External dependency %s has closed. Unblocked: %s (%s). This issue may now proceed.",
				closedIssueID, closedIssueID, depID, depTitle)
			nudgeCmd := exec.Command("gt", "nudge", depRig+"/witness", "-m", msg)
			nudgeCmd.Dir = townRoot
			if err := nudgeCmd.Run(); err != nil {
				logger("CrossRig: nudge %s/witness failed: %v", depRig, err)
			}
		}
	}
}

// AgentFromConvoyDescription returns the runtime agent a convoy recorded at sling
// time, empty when it recorded none. It is the single parse of the 'agent:'
// convoy field, so a re-dispatch path cannot grow its own reading of it
// (gt-mxyk).
func AgentFromConvoyDescription(description string) string {
	if cf := beads.ParseConvoyFields(&beads.Issue{Description: description}); cf != nil {
		return cf.Agent
	}
	return ""
}

// FormulaFromConvoyDescription returns the formula a convoy recorded at sling
// time (--formula), empty when it recorded none. It is the single parse of the
// 'formula:' convoy field, the counterpart of AgentFromConvoyDescription, so a
// re-dispatch path cannot grow its own reading of it (gt-o9sbq).
func FormulaFromConvoyDescription(description string) string {
	if cf := beads.ParseConvoyFields(&beads.Issue{Description: description}); cf != nil {
		return strings.TrimSpace(cf.Formula)
	}
	return ""
}

// RedispatchAgent returns the agent to re-dispatch one bead of a convoy with,
// given the convoy's description, plus a description of that choice for logging.
// It is AgentFromConvoyDescription plus FeedDispatchAgent, and is the entry point
// for a caller holding the convoy record rather than an already-parsed agent.
func RedispatchAgent(convoyDescription, townRoot, rig string) (agent, description string) {
	return FeedDispatchAgent(AgentFromConvoyDescription(convoyDescription), townRoot, rig)
}

// FeedDispatchAgent decides which runtime agent a convoy feeder should
// re-dispatch an issue with. It returns the value for --agent (empty means
// "leave the choice to gt sling") and a human-readable description of where
// the choice came from, for logging.
//
// A convoy that recorded an agent at sling time re-feeds with that same agent:
// the routing decision belongs to whoever slung the bead, and falling back to
// the rig default silently overrides it (gt-yg24). With no recorded agent the
// feeder passes nothing and reports the rig default it expects gt sling to
// resolve — the fallback is logged because an invisible one is what made the
// original defect hard to see, and it is not pinned as a flag because gt
// sling's own resolution also accounts for runtime cost-tier overrides.
func FeedDispatchAgent(convoyAgent, townRoot, rig string) (agent, description string) {
	if a := strings.TrimSpace(convoyAgent); a != "" {
		return a, fmt.Sprintf("agent %q recorded on convoy at sling time", a)
	}
	if rig == "" {
		return "", "rig default agent (rig unresolved, left to gt sling)"
	}
	name, _ := config.ResolveRoleAgentName("polecat", townRoot, filepath.Join(townRoot, rig))
	return "", fmt.Sprintf("rig default agent %q (no --agent recorded on convoy)", name)
}

// dispatchIssue dispatches an issue to a rig via gt sling.
// The context parameter enables cancellation on daemon shutdown.
// gtPath is the resolved path to the gt binary.
// agent is the runtime agent to re-dispatch with; empty leaves the choice to
// gt sling's own resolution.
// formula is the formula the convoy recorded at sling time; empty leaves the
// choice to gt sling's default. Re-dispatching without it runs the bead under
// the default formula instead of the one originally asked for (gt-4lor, gt-o9sbq).
func dispatchIssue(ctx context.Context, townRoot, issueID, rig, gtPath, baseBranch, agent, formula string) error {
	args := []string{"sling", issueID, rig, "--no-boot"}
	if baseBranch != "" {
		args = append(args, "--base-branch="+baseBranch)
	}
	if agent != "" {
		args = append(args, "--agent="+agent)
	}
	if formula != "" {
		args = append(args, "--formula="+formula)
	}
	cmd := exec.CommandContext(ctx, gtPath, args...)
	cmd.Dir = townRoot
	util.SetProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}

	return nil
}
