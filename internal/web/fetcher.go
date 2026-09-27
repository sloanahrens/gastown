package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/activity"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// runCmd executes a command with a timeout and returns stdout.
// Returns empty buffer on timeout or error.
// Security: errors from this function are logged server-side only (via log.Printf
// in callers) and never included in HTTP responses. The handler renders templates
// with whatever data was successfully fetched; fetch failures result in empty panels.
func runCmd(timeout time.Duration, name string, args ...string) (*bytes.Buffer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("%s timed out after %v", name, timeout)
		}
		return nil, err
	}
	return &stdout, nil
}

// subprocessConcurrency bounds the dashboard's short bd/gt reads — the
// fetcher's runBdCmd and the merge-queue snapshot's bd list seam. bd/Dolt
// contention is fsync-bound, not CPU-bound (gt-05vk): concurrent bd children
// queue behind a single-threaded store and only slow each other down. Long
// commands use userCommandConcurrency instead, on a separate pool, so they
// cannot hold every slot this one guards and starve the render (gt-d5xr).
const subprocessConcurrency = 4

// userCommandConcurrency sizes each handler's long-command pool: the
// fetcher's userCmdSem (the background 90s `gt polecat list` inventory
// refresh) and APIHandler's userCmdSem (the user-driven /api/run up to
// maxRunTimeout, gh via runGhCommand, and rig add). The two channels are not
// shared with each other, only sized the same (gt-d5xr). tmux calls stay
// unbounded.
const userCommandConcurrency = 4

// acquireCmdSlot blocks until a slot in sem is free or ctx is done,
// whichever comes first. A nil sem means no bound is configured (e.g. a
// fetcher built directly in a test) and the call proceeds unthrottled.
func acquireCmdSlot(ctx context.Context, sem chan struct{}) error {
	if sem == nil {
		return nil
	}
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseCmdSlot releases a slot acquired via acquireCmdSlot. Safe to call
// with a nil sem (no-op, mirrors acquireCmdSlot's nil handling).
func releaseCmdSlot(sem chan struct{}) {
	if sem == nil {
		return
	}
	<-sem
}

// runTmuxCmd runs a tmux command using the per-town socket.
// Without -L, tmux queries the default socket which has no Gas Town sessions.
func (f *LiveConvoyFetcher) runTmuxCmd(args ...string) (*bytes.Buffer, error) {
	fullArgs := []string{}
	if f.tmuxSocket != "" {
		fullArgs = append(fullArgs, "-L", f.tmuxSocket)
	}
	fullArgs = append(fullArgs, args...)
	return fetcherRunCmd(f.tmuxCmdTimeout, "tmux", fullArgs...)
}

var fetcherRunCmd = runCmd
var fetcherGetSessionEnv = func(sessionName, key string) (string, error) {
	return tmux.NewTmux().GetEnvironment(sessionName, key)
}

// waitBudget bounds how long a call queues for a subprocess slot, kept
// separate from execTimeout so a queued call still gets the full timeout
// once it starts running. Defaults to execTimeout: a fixed budget shorter
// than that turned slow-but-successful bd reads into hard failures under
// exactly the Dolt contention this bound exists for (gt-d5xr).
// slotWaitBudget overrides the default so a test's pool-full case can
// resolve in milliseconds instead of waiting out a realistic timeout.
func (f *LiveConvoyFetcher) waitBudget(execTimeout time.Duration) time.Duration {
	if f.slotWaitBudget > 0 {
		return f.slotWaitBudget
	}
	return execTimeout
}

// runBdCmd executes a bd command with the configured cmdTimeout in the specified beads directory.
func (f *LiveConvoyFetcher) runBdCmd(beadsDir string, args ...string) (*bytes.Buffer, error) {
	// bd v0.59+ requires --flat for list --json to produce JSON output
	args = beads.InjectFlatForListJSON(args)

	// The slot wait has its own budget, so cmdTimeout bounds only execution.
	waitCtx, cancelWait := context.WithTimeout(context.Background(), f.waitBudget(f.cmdTimeout))
	if err := acquireCmdSlot(waitCtx, f.cmdSem); err != nil {
		cancelWait()
		return nil, fmt.Errorf("bd: waiting for subprocess slot: %w", err)
	}
	cancelWait()
	defer releaseCmdSlot(f.cmdSem)

	timeout := f.cmdTimeout
	if f.bdTimeoutFor != nil {
		timeout = f.bdTimeoutFor(args)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	bin := f.bdBin
	if bin == "" {
		bin = "bd"
	}
	cmd := beads.CommandContextWithPath(ctx, bin, beadsDir, nil, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Run()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("bd timed out after %v", timeout)
		}
		// If we got some output, return it anyway (bd may exit non-zero with warnings)
		if stdout.Len() > 0 {
			return &stdout, nil
		}
		return nil, err
	}
	return &stdout, nil
}

// fetchCircuitBreaker tracks consecutive failures for a fetch operation
// and applies exponential backoff to prevent process storms.
type fetchCircuitBreaker struct {
	mu          sync.Mutex
	failures    int
	lastAttempt time.Time
	backoff     time.Duration
	inFlight    bool
}

// maxBackoff is the maximum backoff duration for the circuit breaker.
const maxBackoff = 5 * time.Minute

// allow returns true if enough time has passed since the last failure to permit
// a new attempt, and reserves that attempt so concurrent callers do not all
// stampede through when backoff opens.
func (cb *fetchCircuitBreaker) allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.inFlight {
		return false
	}
	if cb.failures == 0 {
		cb.inFlight = true
		return true
	}
	if time.Since(cb.lastAttempt) < cb.backoff {
		return false
	}
	cb.inFlight = true
	return true
}

// recordFailure increments the failure count and sets exponential backoff.
// Backoff doubles from 10s up to maxBackoff.
func (cb *fetchCircuitBreaker) recordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures++
	cb.lastAttempt = time.Now()
	cb.inFlight = false
	// Exponential backoff: 10s, 20s, 40s, 80s, 160s, capped at maxBackoff
	cb.backoff = time.Duration(1<<min(cb.failures, 10)) * 5 * time.Second
	if cb.backoff > maxBackoff {
		cb.backoff = maxBackoff
	}
}

// recordSuccess resets the circuit breaker on a successful fetch.
func (cb *fetchCircuitBreaker) recordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
	cb.backoff = 0
	cb.inFlight = false
}

// LiveConvoyFetcher fetches convoy data from beads.
type LiveConvoyFetcher struct {
	townRoot  string
	townBeads string

	// bdBin is the bd binary name or path. Defaults to "bd" if empty.
	bdBin string

	// gtBin is the gt binary name or path. Defaults to "gt" if empty.
	gtBin string

	// registry is a prefix registry built from the town's rigs.json.
	// Used for parsing tmux session names instead of relying on the
	// package-level DefaultRegistry, which may not be initialized in
	// the dashboard process context.
	registry *session.PrefixRegistry

	// Configurable timeouts (from TownSettings.WebTimeouts)
	cmdTimeout     time.Duration
	ghCmdTimeout   time.Duration
	tmuxCmdTimeout time.Duration

	// Configurable worker status thresholds (from TownSettings.WorkerStatus)
	staleThreshold          time.Duration
	stuckThreshold          time.Duration
	heartbeatFreshThreshold time.Duration
	mayorActiveThreshold    time.Duration

	// tmuxSocket is the per-town tmux socket name (e.g., "dipgt-651c6b").
	// All tmux commands must use -L with this socket; the default socket
	// has no Gas Town sessions.
	tmuxSocket string

	// Circuit breaker for FetchConvoys — prevents process storms when
	// bd list by convoy label fails persistently (e.g., schema mismatch).
	convoyBreaker fetchCircuitBreaker

	// Merge-queue snapshot, published by a background refresh. Doubles as the
	// breaker's single-flight guard: the derivation is far too slow to run
	// inside a render (gt-r65r).
	mqMu        sync.Mutex
	mqSnapshot  TownMergeQueue
	mqFetchedAt time.Time
	mqBreaker   fetchCircuitBreaker

	// listMRs derives one rig's merge queue; nil means listRigMergeRequests.
	listMRs mrListerFunc

	// rigOpState derives one rig's operational state; nil means
	// rig.GetOpState. Seam for the Rigs and Merge Queue panels' parked
	// markers.
	rigOpState rigOpStateFunc

	// polecatIndex is the `gt polecat list --all --json` snapshot behind the
	// Polecats panel. The panel joins it onto its tmux sessions for the AGENT
	// and MR columns, and mints a row from it for a polecat whose session is
	// gone but whose MR is still in flight (gt-ppja). The list costs a bd query
	// and a bulk merge-request join per rig, so it follows the merge-queue
	// snapshot's shape: a render reads the cache, and a background refresh
	// replaces it once stale (gt-kqi2).
	polecatMu        sync.Mutex
	polecatIndex     polecatIndex
	polecatFetchedAt time.Time
	polecatBreaker   fetchCircuitBreaker

	// listPolecats derives the town's polecats; nil means listTownPolecats.
	listPolecats polecatLister

	// Merges-last-6h tile: its own cache, and its own counter so tests can
	// count merges without a git subprocess.
	mergesMu    sync.Mutex
	mergesTile  []RigMergeCount
	mergesTotal int
	mergesAt    time.Time
	countMerges countMergesFunc

	// Local Pool panel: localServerBreaker holds the poll off while the model
	// server is down. listPoolAgents overrides the tmux seat read in tests.
	localServerBreaker fetchCircuitBreaker
	listPoolAgents     poolAgentLister

	// cmdSem bounds this fetcher's bd reads (see subprocessConcurrency).
	// Left nil by struct literals built directly in tests, which
	// acquireCmdSlot/releaseCmdSlot treat as "no bound configured".
	cmdSem chan struct{}

	// userCmdSem bounds this fetcher's long-running gt children — the
	// background polecat inventory refresh — against userCommandConcurrency,
	// separate from cmdSem's bd reads (gt-d5xr). Same nil semantics as cmdSem.
	userCmdSem chan struct{}

	// slotWaitBudget overrides waitBudget's default for every cmdSem/userCmdSem
	// acquire this fetcher makes. Zero (the default) means "use the call's own
	// exec timeout"; tests set it short so a full-pool case resolves fast.
	slotWaitBudget time.Duration

	// bdTimeoutFor overrides cmdTimeout's execution deadline per bd call.
	// Nil (the default) means every call gets cmdTimeout. Test seam: a test
	// that proves one call's timeout path can give that call a short
	// deadline while its fast neighbors keep a generous one, instead of one
	// shared budget tight enough that a loaded host kills the fast calls too.
	bdTimeoutFor func(args []string) time.Duration
}

// NewLiveConvoyFetcher creates a fetcher for the current workspace.
// Loads timeout and threshold config from TownSettings; falls back to defaults if missing.
func NewLiveConvoyFetcher() (*LiveConvoyFetcher, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	webCfg := config.DefaultWebTimeoutsConfig()
	workerCfg := config.DefaultWorkerStatusConfig()
	if ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot)); err == nil {
		// Replace entire defaults — individual fields fall back via ParseDurationOrDefault
		// (empty string → hardcoded default). Add explicit zero-value guards for non-duration fields.
		if ts.WebTimeouts != nil {
			webCfg = ts.WebTimeouts
		}
		if ts.WorkerStatus != nil {
			workerCfg = ts.WorkerStatus
		}
	}

	// Build a local prefix registry from the town's rigs.json so session
	// name parsing works regardless of whether the package-level
	// DefaultRegistry was initialized (gt-y24).
	registry, regErr := session.BuildPrefixRegistryFromTown(townRoot)
	if regErr != nil {
		log.Printf("dashboard: failed to build prefix registry: %v (falling back to default)", regErr)
		registry = session.DefaultRegistry()
	}

	fetcher := &LiveConvoyFetcher{
		townRoot:                townRoot,
		townBeads:               filepath.Join(townRoot, ".beads"),
		registry:                registry,
		tmuxSocket:              tmux.GetDefaultSocket(),
		cmdTimeout:              config.ParseDurationOrDefault(webCfg.CmdTimeout, 15*time.Second),
		ghCmdTimeout:            config.ParseDurationOrDefault(webCfg.GhCmdTimeout, 10*time.Second),
		tmuxCmdTimeout:          config.ParseDurationOrDefault(webCfg.TmuxCmdTimeout, 2*time.Second),
		staleThreshold:          config.ParseDurationOrDefault(workerCfg.StaleThreshold, 5*time.Minute),
		stuckThreshold:          config.ParseDurationOrDefault(workerCfg.StuckThreshold, constants.GUPPViolationTimeout),
		heartbeatFreshThreshold: config.ParseDurationOrDefault(workerCfg.HeartbeatFreshThreshold, 5*time.Minute),
		mayorActiveThreshold:    config.ParseDurationOrDefault(workerCfg.MayorActiveThreshold, 5*time.Minute),
		cmdSem:                  make(chan struct{}, subprocessConcurrency),
		userCmdSem:              make(chan struct{}, userCommandConcurrency),
	}

	// Start the merge-queue refresh at startup rather than at the first page
	// load: the derivation takes tens of seconds, so a dashboard whose first
	// render triggers it shows an empty panel for the rest of that cycle.
	fetcher.FetchTownMergeQueue()
	// Same for the polecat inventory behind the Polecats panel's AGENT and MR
	// columns, which is slower still (gt-kqi2).
	fetcher.workerPolecatIndex()
	return fetcher, nil
}

// errConvoyBreakerOpen marks a FetchConvoys call that was skipped because the
// breaker is backed off. It is a distinct sentinel (not a generic list-read
// error) so the handler can render "unreadable" without re-logging a fresh
// failure for every backed-off request during the cooldown (gt-jwf7).
var errConvoyBreakerOpen = errors.New("convoy list breaker open (backed off after repeated failures)")

// FetchConvoys fetches all open convoys with their activity data.
// Uses a circuit breaker to avoid hammering bd/dolt when listing fails
// persistently (e.g., "invalid issue type: convoy" schema mismatch). A
// backed-off call and a failed list read both return a non-nil error rather
// than (nil, nil): an outage must not read as a genuinely empty town
// (gt-jwf7).
func (f *LiveConvoyFetcher) FetchConvoys() ([]ConvoyRow, error) {
	if !f.convoyBreaker.allow() {
		return nil, errConvoyBreakerOpen
	}

	// List all open issues and filter locally so legacy type=convoy beads remain visible.
	stdout, err := f.runBdCmd(f.townRoot, "list", "--status=open", "--json", "--limit=0")
	if err != nil {
		f.convoyBreaker.recordFailure()
		return nil, fmt.Errorf("listing convoys: %w", err)
	}

	var convoys []struct {
		ID        string   `json:"id"`
		Title     string   `json:"title"`
		Status    string   `json:"status"`
		CreatedAt string   `json:"created_at"`
		IssueType string   `json:"issue_type"`
		Labels    []string `json:"labels"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &convoys); err != nil {
		f.convoyBreaker.recordFailure()
		return nil, fmt.Errorf("parsing convoy list: %w", err)
	}

	// Build convoy rows with activity data
	rows := make([]ConvoyRow, 0, len(convoys))
	for _, c := range convoys {
		if c.IssueType != "convoy" && !webConvoyHasLabel(c.Labels, "gt:convoy") {
			continue
		}
		row := ConvoyRow{
			ID:     c.ID,
			Title:  c.Title,
			Status: c.Status,
		}

		// Get tracked issues for progress and activity calculation
		tracked, err := f.getTrackedIssues(c.ID)
		if err != nil {
			// The convoy is open — only its detail read failed. Dropping the
			// row made a failed read render as a convoy that is not there, so
			// the panel showed a smaller count than reality with nothing to
			// say it was partial (gt-huzu). Emit the row with DetailErr set:
			// it counts, and the panel marks its progress unreadable.
			log.Printf("warning: convoy %s: detail unavailable: %v", c.ID, err)
			row.DetailErr = convoyDetailUnavailable
			row.Progress = "—"
			row.LastActivity = activity.Info{
				FormattedAge: convoyDetailUnavailable,
				ColorClass:   activity.ColorUnknown,
			}
			rows = append(rows, row)
			continue
		}
		row.Total = len(tracked)

		var mostRecentActivity time.Time
		var mostRecentUpdated time.Time
		var hasAssignee bool
		assigneeSet := make(map[string]struct{})
		for _, t := range tracked {
			if t.Status == "closed" {
				row.Completed++
			} else if t.Assignee != "" {
				row.InProgress++
			} else {
				row.ReadyBeads++
			}
			// Track most recent activity from workers
			if t.LastActivity.After(mostRecentActivity) {
				mostRecentActivity = t.LastActivity
			}
			// Track most recent updated_at as fallback
			if t.UpdatedAt.After(mostRecentUpdated) {
				mostRecentUpdated = t.UpdatedAt
			}
			if t.Assignee != "" {
				hasAssignee = true
				assigneeSet[t.Assignee] = struct{}{}
			}
		}

		// Collect unique assignees (sorted for stable display order)
		row.Assignees = make([]string, 0, len(assigneeSet))
		for a := range assigneeSet {
			row.Assignees = append(row.Assignees, a)
		}
		sort.Strings(row.Assignees)

		row.Progress = fmt.Sprintf("%d/%d", row.Completed, row.Total)
		if row.Total > 0 {
			row.ProgressPct = (row.Completed * 100) / row.Total
		}

		// Calculate activity info from most recent worker activity
		if !mostRecentActivity.IsZero() {
			// Have active tmux session activity from assigned workers
			row.LastActivity = activity.Calculate(mostRecentActivity)
		} else if !hasAssignee {
			// No assignees found in beads - a convoy nobody is actively
			// working stays "unassigned"/"waiting". Never derive its status
			// from another worker's tmux session: any running polecat's
			// idle time used to get attributed to this convoy, rendering it
			// STUCK for no reason (gt-q0is). Pin ColorClass to Unknown so
			// calculateWorkStatus always reports "waiting" here, while still
			// showing the age of the most recent tracked-issue update for
			// context.
			info := activity.Info{FormattedAge: "unassigned", ColorClass: activity.ColorUnknown}
			if !mostRecentUpdated.IsZero() {
				info.FormattedAge = activity.Calculate(mostRecentUpdated).FormattedAge + " (unassigned)"
			}
			row.LastActivity = info
		} else {
			// Has assignee but no active session
			row.LastActivity = activity.Info{
				FormattedAge: "idle",
				ColorClass:   activity.ColorUnknown,
			}
		}

		// Calculate work status based on progress and activity
		row.WorkStatus = calculateWorkStatus(row.Completed, row.Total, row.LastActivity.ColorClass)

		// Get tracked issues for expandable view
		row.TrackedIssues = make([]TrackedIssue, len(tracked))
		for i, t := range tracked {
			row.TrackedIssues[i] = TrackedIssue{
				ID:       t.ID,
				Title:    t.Title,
				Status:   t.Status,
				Assignee: t.Assignee,
			}
		}

		rows = append(rows, row)
	}

	f.convoyBreaker.recordSuccess()
	return rows, nil
}

func webConvoyHasLabel(labels []string, target string) bool {
	for _, label := range labels {
		if label == target {
			return true
		}
	}
	return false
}

// trackedIssueInfo holds info about an issue being tracked by a convoy.
type trackedIssueInfo struct {
	ID           string
	Title        string
	Status       string
	Assignee     string
	LastActivity time.Time
	UpdatedAt    time.Time // Fallback for activity when no assignee
}

// depRef is a raw dependency edge's target ID as returned by bd.
type depRef struct {
	ID string `json:"id"`
}

// getTrackedIssues fetches tracked issues for a convoy.
func (f *LiveConvoyFetcher) getTrackedIssues(convoyID string) ([]trackedIssueInfo, error) {
	// Query tracked dependencies using bd's raw-edge form (see rawTrackedDeps).
	deps, err := f.rawTrackedDeps(convoyID)
	if err != nil {
		// The raw-edge form (ID passed twice) is newer than the single-ID form.
		// Older bd builds reject it — fall back so an upgrade path never takes
		// the whole convoy panel down.
		log.Printf("dashboard: convoy %s: raw dep query failed (%v), falling back to bd dep list", convoyID, err)
		deps, err = f.depListTrackedDeps(convoyID)
		if err != nil {
			return nil, fmt.Errorf("querying tracked issues for %s: %w", convoyID, err)
		}
	}

	// Final fallback: bd show's dependency array. Unlike the dep queries above
	// this never joins against the issues table, so cross-database edges
	// survive — but bd only populates the array when the targets resolve
	// locally, and emits "dependencies": null when every edge is external
	// (gt-44z1). Kept for bd builds that lack the raw-edge form.
	if len(deps) == 0 {
		deps, err = f.bdShowTrackedDeps(convoyID)
		if err != nil {
			return nil, fmt.Errorf("fallback show for tracked deps of %s: %w", convoyID, err)
		}
	}

	// Collect resolved issue IDs, unwrapping external:prefix:id format.
	// Unparseable targets are skipped rather than failing the row: a convoy
	// with one garbage edge (e.g. "external:om:om-gate coverage: om", a title
	// used as an ID) must still render the rest of its progress (gt-44z1).
	issueIDs := make([]string, 0, len(deps))
	seenIDs := make(map[string]struct{}, len(deps))
	for _, dep := range deps {
		id := beads.ExtractIssueID(dep.ID)
		if !isResolvableIssueID(id) {
			log.Printf("dashboard: convoy %s: skipping unparseable tracked edge %q", convoyID, dep.ID)
			continue
		}
		if _, dup := seenIDs[id]; dup {
			continue
		}
		seenIDs[id] = struct{}{}
		issueIDs = append(issueIDs, id)
	}

	// Batch fetch issue details
	details, err := f.getIssueDetailsBatch(issueIDs)
	if err != nil {
		return nil, fmt.Errorf("fetching tracked issue details for %s: %w", convoyID, err)
	}

	// Get worker activity from tmux sessions based on assignees
	workers := f.getWorkersFromAssignees(details)

	// Build result
	result := make([]trackedIssueInfo, 0, len(issueIDs))
	for _, id := range issueIDs {
		info := trackedIssueInfo{ID: id}

		if d, ok := details[id]; ok {
			info.Title = d.Title
			info.Status = d.Status
			info.Assignee = d.Assignee
			info.UpdatedAt = d.UpdatedAt
		} else {
			info.Title = "(external)"
			info.Status = "unknown"
		}

		if w, ok := workers[id]; ok && w.LastActivity != nil {
			info.LastActivity = *w.LastActivity
		}

		result = append(result, info)
	}

	return result, nil
}

// rawTrackedDeps returns a convoy's "tracks" edges via `bd dep list <convoyID>
// <convoyID> --json` — the same ID passed twice, which is the raw-edge form bd
// itself points callers to.
//
// With a single ID, `bd dep list` joins the dependency records against the
// local issues table. That join silently drops every cross-database edge
// (external:<rig>:<id>, how a town-level convoy tracks a bead living in
// another rig's Dolt database) and only mentions it on stderr, which this
// fetcher discards — so convoys tracking cross-rig beads rendered 0/0
// (gt-q0is, gt-44z1; GH #2624, #2832). The batch form returns the raw records
// with depends_on_id intact, which also carries the target for edges whose
// target row is not in this database.
//
// Returned IDs are unwrapped; the caller validates them. An empty slice means
// "try the next strategy", never "this convoy tracks nothing" — so a payload
// this function cannot read is an error, not an empty slice (gt-r12y).
func (f *LiveConvoyFetcher) rawTrackedDeps(convoyID string) ([]depRef, error) {
	stdout, err := f.runBdCmd(f.townRoot, "dep", "list", convoyID, convoyID, "--json")
	if err != nil {
		return nil, err
	}

	edges, err := parseRawEdgeRecords(stdout.Bytes(), convoyID)
	if err != nil {
		return nil, err
	}

	deps := make([]depRef, 0, len(edges))
	for _, edge := range edges {
		if edge.Type == nil || *edge.Type != rawEdgeTracks || edge.DependsOnID == nil {
			continue
		}
		deps = append(deps, depRef{ID: beads.ExtractIssueID(*edge.DependsOnID)})
	}
	return deps, nil
}

// rawEdgeTracks is the dependency type bd gives a convoy's tracked issues.
const rawEdgeTracks = "tracks"

// rawEdgeRecord is one record of bd's raw-edge form, `bd dep list <convoyID>
// <convoyID> --json`. The pointers keep a field bd renamed distinguishable from
// one it omitted, which is what parseRawEdgeRecords reports.
type rawEdgeRecord struct {
	DependsOnID *string `json:"depends_on_id"`
	Type        *string `json:"type"`
}

// parseRawEdgeRecords decodes bd's raw-edge form and reports a payload whose
// records stopped carrying depends_on_id or type. Without that check a renamed
// field decodes as an empty string and the convoy reads as tracking nothing:
// the caller falls through to the join-based strategies, which drop
// cross-database edges, restoring the 0/0 render (gt-44z1) with nothing in the
// log to say why.
func parseRawEdgeRecords(payload []byte, convoyID string) ([]rawEdgeRecord, error) {
	var records []rawEdgeRecord
	if err := json.Unmarshal(payload, &records); err != nil {
		return nil, fmt.Errorf("parsing raw deps for %s: %w", convoyID, err)
	}
	if len(records) == 0 {
		return nil, nil
	}

	var hasTarget, hasType bool
	for _, rec := range records {
		hasTarget = hasTarget || rec.DependsOnID != nil
		hasType = hasType || rec.Type != nil
	}
	if !hasTarget {
		return nil, fmt.Errorf("bd's raw-edge form returned %d record(s) for %s, none carrying depends_on_id: the CLI's record schema changed, so tracked edges can no longer be resolved", len(records), convoyID)
	}
	if !hasType {
		return nil, fmt.Errorf("bd's raw-edge form returned %d record(s) for %s, none carrying type: the CLI's record schema changed, so tracked edges can no longer be resolved", len(records), convoyID)
	}
	return records, nil
}

// depListTrackedDeps is the single-ID `bd dep list --type=tracks` query. It is
// the pre-raw-edge code path, retained only as a compatibility fallback for bd
// builds that reject the batch form (see rawTrackedDeps): its join drops
// cross-database edges, so it is never the first choice.
func (f *LiveConvoyFetcher) depListTrackedDeps(convoyID string) ([]depRef, error) {
	stdout, err := f.runBdCmd(f.townRoot, "dep", "list", convoyID, "-t", "tracks", "--json")
	if err != nil {
		return nil, err
	}

	var deps []depRef
	if err := json.Unmarshal(stdout.Bytes(), &deps); err != nil {
		return nil, fmt.Errorf("parsing tracked issues for %s: %w", convoyID, err)
	}
	return deps, nil
}

// isResolvableIssueID reports whether id looks like a bead ID rather than a
// mangled edge target. Cross-rig edges are stored as external:<rig>:<id>, and
// a malformed one ("external:om:om-gate coverage: om" — a title where an ID
// belongs) unwraps to something with spaces that no bd query can resolve.
// Such an edge is dropped so it cannot blank out an otherwise healthy row.
func isResolvableIssueID(id string) bool {
	if id == "" {
		return false
	}
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
			// Allowed, but not as the first character: every bead ID starts
			// with its alphabetic prefix (gt-, hq-, om-).
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// bdShowTrackedDeps falls back to `bd show <convoyID> --json` and extracts
// "tracks" dependency edges from the convoy's raw dependency list. Unlike
// `bd dep list --type=tracks`, this avoids the join against the issues
// table that silently drops cross-database (external:<rig>:<id>) edges
// (see GH #2624, #2832). bd emits "dependencies": null when none of the
// convoy's edges resolve locally, so this is a last resort, not the fix
// for cross-rig convoys (gt-44z1).
func (f *LiveConvoyFetcher) bdShowTrackedDeps(convoyID string) ([]depRef, error) {
	stdout, err := f.runBdCmd(f.townRoot, "show", convoyID, "--json")
	if err != nil {
		return nil, err
	}

	var results []struct {
		Dependencies []struct {
			ID             string `json:"id"`
			DependencyType string `json:"dependency_type"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil {
		return nil, fmt.Errorf("parsing show for %s: %w", convoyID, err)
	}
	if len(results) == 0 {
		return nil, nil
	}

	var deps []depRef
	for _, dep := range results[0].Dependencies {
		if dep.DependencyType != "tracks" {
			continue
		}
		deps = append(deps, depRef{ID: dep.ID})
	}
	return deps, nil
}

// issueDetail holds basic issue info.
type issueDetail struct {
	ID        string
	Title     string
	Status    string
	Assignee  string
	UpdatedAt time.Time
}

// getIssueDetailsBatch fetches details for multiple issues.
//
// Runs from the town root, not the dashboard's own working directory: bd
// discovers the beads database relative to cwd, so a server started outside
// the town cannot resolve any of these IDs (gt-80o, gt-44z1). From the town
// root, prefix routing in routes.jsonl reaches the rig that owns each bead,
// including the cross-rig targets a convoy tracks.
func (f *LiveConvoyFetcher) getIssueDetailsBatch(issueIDs []string) (map[string]*issueDetail, error) {
	result := make(map[string]*issueDetail)
	if len(issueIDs) == 0 {
		return result, nil
	}

	args := append([]string{"show"}, issueIDs...)
	args = append(args, "--json")

	stdout, err := f.runBdCmd(f.townRoot, args...)
	if err != nil {
		return nil, fmt.Errorf("bd show failed (issue_count=%d): %w", len(issueIDs), err)
	}

	var issues []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Status    string `json:"status"`
		Assignee  string `json:"assignee"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &issues); err != nil {
		return nil, fmt.Errorf("bd show returned invalid JSON (issue_count=%d): %w", len(issueIDs), err)
	}

	for _, issue := range issues {
		detail := &issueDetail{
			ID:       issue.ID,
			Title:    issue.Title,
			Status:   issue.Status,
			Assignee: issue.Assignee,
		}
		// Parse updated_at timestamp
		if issue.UpdatedAt != "" {
			if t, err := time.Parse(time.RFC3339, issue.UpdatedAt); err == nil {
				detail.UpdatedAt = t
			}
		}
		result[issue.ID] = detail
	}

	return result, nil
}

// workerDetail holds worker info including last activity.
type workerDetail struct {
	Worker       string
	LastActivity *time.Time
}

// getWorkersFromAssignees gets worker activity from tmux sessions based on issue assignees.
// Assignees are in format "rigname/polecats/polecatname" which maps to tmux session "gt-rigname-polecatname".
func (f *LiveConvoyFetcher) getWorkersFromAssignees(details map[string]*issueDetail) map[string]*workerDetail {
	result := make(map[string]*workerDetail)

	// Collect unique assignees and map them to issue IDs
	assigneeToIssues := make(map[string][]string)
	for issueID, detail := range details {
		if detail == nil || detail.Assignee == "" {
			continue
		}
		assigneeToIssues[detail.Assignee] = append(assigneeToIssues[detail.Assignee], issueID)
	}

	if len(assigneeToIssues) == 0 {
		return result
	}

	// For each unique assignee, look up tmux session activity
	for assignee, issueIDs := range assigneeToIssues {
		activity := f.getSessionActivityForAssignee(assignee)
		if activity == nil {
			continue
		}

		// Apply this activity to all issues assigned to this worker
		for _, issueID := range issueIDs {
			result[issueID] = &workerDetail{
				Worker:       assignee,
				LastActivity: activity,
			}
		}
	}

	return result
}

// getSessionActivityForAssignee looks up tmux session activity for an assignee.
// Assignee format: "rigname/polecats/polecatname" -> session "gt-rigname-polecatname"
func (f *LiveConvoyFetcher) getSessionActivityForAssignee(assignee string) *time.Time {
	// Parse assignee: "roxas/polecats/dag" -> rig="roxas", polecat="dag"
	parts := strings.Split(assignee, "/")
	if len(parts) != 3 || parts[1] != "polecats" {
		return nil
	}
	rig := parts[0]
	polecat := parts[2]

	// Construct session name
	sessionName := session.PolecatSessionName(session.PrefixFor(rig), polecat)

	// Query tmux for the window's activity clock (unix seconds); the session
	// clock freezes at creation for a detached session (gt-vcfs).
	stdout, err := f.runTmuxCmd("list-sessions", "-F", "#{session_name}|#{window_activity}",
		"-f", fmt.Sprintf("#{==:#{session_name},%s}", sessionName))
	if err != nil {
		return nil
	}

	output := strings.TrimSpace(stdout.String())
	if output == "" {
		return nil
	}

	// Parse output: "gt-roxas-dag|1704312345"
	outputParts := strings.Split(output, "|")
	if len(outputParts) < 2 {
		return nil
	}

	var activityUnix int64
	if _, err := fmt.Sscanf(outputParts[1], "%d", &activityUnix); err != nil || activityUnix == 0 {
		return nil
	}

	activity := time.Unix(activityUnix, 0)
	return &activity
}

// calculateWorkStatus determines the work status based on progress and activity.
// Returns: "complete", "active", "stale", "stuck", or "waiting"
func calculateWorkStatus(completed, total int, activityColor string) string {
	// Check if all work is done
	if total > 0 && completed == total {
		return "complete"
	}

	// Determine status based on activity color
	switch activityColor {
	case activity.ColorGreen:
		return "active"
	case activity.ColorYellow:
		return "stale"
	case activity.ColorRed:
		return "stuck"
	default:
		return "waiting"
	}
}

// FetchMergeQueue fetches open PRs from registered rigs.
func (f *LiveConvoyFetcher) FetchMergeQueue() ([]MergeQueueRow, error) {
	// Load registered rigs from config
	rigsConfigPath := filepath.Join(f.townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading rigs config: %w", err)
	}

	var result []MergeQueueRow

	for rigName, entry := range rigsConfig.Rigs {
		// Convert git URL to owner/repo format for gh CLI
		repoPath := gitURLToRepoPath(entry.GitURL)
		if repoPath == "" {
			continue
		}

		prs, err := f.fetchPRsForRepo(repoPath, rigName)
		if err != nil {
			// Non-fatal: continue with other repos
			continue
		}
		result = append(result, prs...)
	}

	return result, nil
}

// gitURLToRepoPath converts a git URL to owner/repo format.
// Supports HTTPS (https://github.com/owner/repo.git) and
// SSH (git@github.com:owner/repo.git) formats.
func gitURLToRepoPath(gitURL string) string {
	// Handle HTTPS format: https://github.com/owner/repo.git
	if strings.HasPrefix(gitURL, "https://github.com/") {
		path := strings.TrimPrefix(gitURL, "https://github.com/")
		path = strings.TrimSuffix(path, ".git")
		return path
	}

	// Handle SSH format: git@github.com:owner/repo.git
	if strings.HasPrefix(gitURL, "git@github.com:") {
		path := strings.TrimPrefix(gitURL, "git@github.com:")
		path = strings.TrimSuffix(path, ".git")
		return path
	}

	// Unsupported format
	return ""
}

// prResponse represents the JSON response from gh pr list.
type prResponse struct {
	Number            int    `json:"number"`
	Title             string `json:"title"`
	URL               string `json:"url"`
	Mergeable         string `json:"mergeable"`
	StatusCheckRollup []struct {
		State      string `json:"state"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"statusCheckRollup"`
}

// fetchPRsForRepo fetches open PRs for a single repo.
func (f *LiveConvoyFetcher) fetchPRsForRepo(repoFull, repoShort string) ([]MergeQueueRow, error) {
	stdout, err := runCmd(f.ghCmdTimeout, "gh", "pr", "list",
		"--repo", repoFull,
		"--state", "open",
		"--json", "number,title,url,mergeable,statusCheckRollup")
	if err != nil {
		return nil, fmt.Errorf("fetching PRs for %s: %w", repoFull, err)
	}

	var prs []prResponse
	if err := json.Unmarshal(stdout.Bytes(), &prs); err != nil {
		return nil, fmt.Errorf("parsing PRs for %s: %w", repoFull, err)
	}

	result := make([]MergeQueueRow, 0, len(prs))
	for _, pr := range prs {
		row := MergeQueueRow{
			Number: pr.Number,
			Repo:   repoShort,
			Title:  pr.Title,
			URL:    pr.URL,
		}

		// Determine CI status from statusCheckRollup
		row.CIStatus = determineCIStatus(pr.StatusCheckRollup)

		// Determine mergeable status
		row.Mergeable = determineMergeableStatus(pr.Mergeable)

		// Determine color class based on overall status
		row.ColorClass = determineColorClass(row.CIStatus, row.Mergeable)

		result = append(result, row)
	}

	return result, nil
}

// determineCIStatus evaluates the overall CI status from status checks.
func determineCIStatus(checks []struct {
	State      string `json:"state"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}) string {
	if len(checks) == 0 {
		return "pending"
	}

	hasFailure := false
	hasPending := false

	for _, check := range checks {
		// Check conclusion first (for completed checks)
		switch check.Conclusion {
		case "failure", "cancelled", "timed_out", "action_required": //nolint:misspell // GitHub API returns "cancelled" (British spelling)
			hasFailure = true
		case "success", "skipped", "neutral":
			// Pass
		default:
			// Check status for in-progress checks
			switch check.Status {
			case "queued", "in_progress", "waiting", "pending", "requested":
				hasPending = true
			}
			// Also check state field
			switch check.State {
			case "FAILURE", "ERROR":
				hasFailure = true
			case "PENDING", "EXPECTED":
				hasPending = true
			}
		}
	}

	if hasFailure {
		return "fail"
	}
	if hasPending {
		return "pending"
	}
	return "pass"
}

// determineMergeableStatus converts GitHub's mergeable field to display value.
func determineMergeableStatus(mergeable string) string {
	switch strings.ToUpper(mergeable) {
	case "MERGEABLE":
		return "ready"
	case "CONFLICTING":
		return "conflict"
	default:
		return "pending"
	}
}

// determineColorClass determines the row color based on CI and merge status.
func determineColorClass(ciStatus, mergeable string) string {
	if ciStatus == "fail" || mergeable == "conflict" {
		return "mq-red"
	}
	if ciStatus == "pending" || mergeable == "pending" {
		return "mq-yellow"
	}
	if ciStatus == "pass" && mergeable == "ready" {
		return "mq-green"
	}
	return "mq-yellow"
}

const (
	// mergeRequestLabel is the label every MR submitted by `gt mq submit` carries.
	mergeRequestLabel = "gt:merge-request"

	// townMergeQueueTTL is how long a merge-queue snapshot is served before a
	// background refresh replaces it.
	townMergeQueueTTL = 3 * time.Minute

	// mergesTileTTL caches the merges-last-6h tile. The tile's window moves an
	// order of magnitude slower than a dashboard refresh, and each count is a
	// git subprocess per rig.
	mergesTileTTL = 5 * time.Minute

	// mergesWindow is the tile's lookback window.
	mergesWindow = 6 * time.Hour

	// mergesCountTimeout bounds one `git rev-list --count` per rig.
	mergesCountTimeout = 10 * time.Second
)

// rigOpProbeTimeout bounds one render's whole operational-state probe (see
// rigOpLabels). internal/beads caps a single bd subprocess at 60s, which is
// longer than a render may take. A var so the deadline test does not have to
// sit out the real one.
var rigOpProbeTimeout = 5 * time.Second

// mrListerFunc derives one rig's merge-request wisps. It is the seam tests
// replace so the derivation runs without spawning bd.
type mrListerFunc func(beadsDir string, opts beads.ListOptions) ([]*beads.Issue, error)

// rigOpStateFunc derives one rig's operational state. The seam tests replace
// so the panels can render a parked rig without a town.
type rigOpStateFunc func(rigName string) (rig.OpState, string)

// rigOpLabels returns the parked/docked marker for each named rig, and no
// entry for a rig that accepts work.
//
// A parked rig used to be invisible to the dashboard, which is how a
// five-day-old READY MR in a parked rig sat at the top of the queue reading
// as a refinery stall. The state is probed at render time rather than baked
// into the three-minute merge-queue snapshot: a marker older than the panel
// it decorates is the same lie in a different font.
//
// The probe is cheap exactly where it matters. `gt rig park` writes the rig's
// wisp config, a local file read that spawns nothing, so a parked rig is
// marked however loaded the town is. Only a rig whose wisp says nothing
// reaches for its identity bead — one `bd show`, measured at ~50ms warm —
// which is the persistent fallback that keeps a rig parked after wisp
// cleanup (#2079).
//
// The fan-out is bounded all the same, because a wedged Dolt would otherwise
// hold a render for beads' 60s subprocess cap and undo the merge queue's
// careful off-render derivation. Rigs whose probe misses the deadline go
// unmarked — the panel's behavior before it knew about parked rigs — and the
// next render tries again. Fast probes have already landed by then: a parked
// rig answers in the time it takes to read one small file.
func (f *LiveConvoyFetcher) rigOpLabels(rigNames []string) map[string]string {
	probe := f.rigOpState
	if probe == nil {
		probe = func(rigName string) (rig.OpState, string) {
			return rig.GetOpState(f.townRoot, rigName)
		}
	}

	type probeResult struct {
		rig   string
		state rig.OpState
	}
	// Buffered to one result per rig so a probe that outlives the deadline
	// still has somewhere to send, and no goroutine leaks blocked on a
	// channel nobody is left to read.
	resCh := make(chan probeResult, len(rigNames))
	for _, name := range rigNames {
		go func(name string) {
			state, _ := probe(name)
			resCh <- probeResult{rig: name, state: state}
		}(name)
	}

	labels := make(map[string]string, len(rigNames))
	timeout := time.NewTimer(rigOpProbeTimeout)
	defer timeout.Stop()
	for range rigNames {
		select {
		case res := <-resCh:
			if label := res.state.Label(); label != "" {
				labels[res.rig] = label
			}
		case <-timeout.C:
			return labels
		}
	}
	return labels
}

// listRigMergeRequests is the production lister: the `gt mq list` derivation,
// which searches both the issues and the wisps table and hydrates each MR so
// its blockers are visible.
func listRigMergeRequests(beadsDir string, opts beads.ListOptions) ([]*beads.Issue, error) {
	return beads.New(beadsDir).ListMergeRequests(opts)
}

// countMergesFunc counts merges landed on a repo's main branch since a window.
type countMergesFunc func(repoPath string, window time.Duration) (int, error)

// FetchTownMergeQueue returns the town's merge-request wisps and the
// merges-last-6h tile.
//
// It does not block on bd. One derivation costs three bd subprocesses per rig
// (list, wisp query, hydration) and bd startup alone runs to seconds under
// town load, so a synchronous fetch would add that to every render — and would
// spawn that many processes on every poll tick. The call therefore returns the
// snapshot the last background refresh produced and starts a refresh when that
// snapshot is stale (gt-r65r).
func (f *LiveConvoyFetcher) FetchTownMergeQueue() TownMergeQueue {
	f.mqMu.Lock()
	snapshot := f.mqSnapshot
	stale := time.Since(f.mqFetchedAt) >= townMergeQueueTTL
	f.mqMu.Unlock()

	// The breaker both single-flights the refresh and backs it off after
	// failures, so a town whose rigs cannot be read does not re-derive on
	// every render.
	if stale && f.mqBreaker.allow() {
		go f.refreshTownMergeQueue()
	}
	return f.markParkedRigs(snapshot)
}

// markParkedRigs stamps each MR row with the operational state of the rig
// that owns it, and counts them for the panel header.
//
// The stamp is applied here, on the render path, and deliberately not in
// townMergeQueueSnapshot: that snapshot is served for up to
// townMergeQueueTTL, and a parked marker three minutes behind the rig it
// describes is the staleness this marker exists to expose.
func (f *LiveConvoyFetcher) markParkedRigs(snapshot TownMergeQueue) TownMergeQueue {
	if len(snapshot.Rows) == 0 {
		return snapshot
	}

	// Copy before stamping: snapshot.Rows aliases the cached mqSnapshot's
	// backing array, and writing labels into it would pin them there — a rig
	// unparked later would keep its marker until the next derivation.
	rows := make([]TownMergeQueueRow, len(snapshot.Rows))
	copy(rows, snapshot.Rows)
	snapshot.Rows = rows

	// Every MR is labeled by its rig, so probe each rig once.
	rigNames := make([]string, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if !seen[row.Rig] {
			seen[row.Rig] = true
			rigNames = append(rigNames, row.Rig)
		}
	}
	opLabels := f.rigOpLabels(rigNames)

	snapshot.ParkedCount = 0
	for i := range rows {
		rows[i].RigOpState = opLabels[rows[i].Rig]
		if rows[i].RigOpState != "" {
			snapshot.ParkedCount++
		}
	}
	return snapshot
}

// refreshTownMergeQueue republishes the snapshot on success. A failed refresh
// keeps the previous one: an empty panel is worse than a stale one, and the
// breaker decides when to try again.
func (f *LiveConvoyFetcher) refreshTownMergeQueue() {
	snapshot, err := f.townMergeQueueSnapshot()
	if err != nil {
		f.mqBreaker.recordFailure()
		log.Printf("dashboard: town merge queue refresh failed: %v", err)
		return
	}
	f.mqBreaker.recordSuccess()

	f.mqMu.Lock()
	f.mqSnapshot = snapshot
	f.mqFetchedAt = time.Now()
	f.mqMu.Unlock()
}

// townMergeQueueSnapshot derives the merge queue for every rig laid out in
// this town. MRs are wisps in each rig's own database, so the derivation is
// per rig: no single query spans them.
func (f *LiveConvoyFetcher) townMergeQueueSnapshot() (TownMergeQueue, error) {
	rigsConfig, err := config.LoadRigsConfig(filepath.Join(f.townRoot, "mayor", "rigs.json"))
	if err != nil {
		return TownMergeQueue{}, fmt.Errorf("loading rigs config: %w", err)
	}

	list := f.listMRs
	if list == nil {
		list = listRigMergeRequests
	}

	now := time.Now()
	var rows []TownMergeQueueRow
	var firstErr error
	answered := 0
	rigNames := make([]string, 0, len(rigsConfig.Rigs))
	for rigName := range rigsConfig.Rigs {
		rigPath := filepath.Join(f.townRoot, rigName)
		if _, statErr := os.Stat(rigPath); statErr != nil {
			continue // registered in rigs.json but not laid out on this machine
		}
		rigNames = append(rigNames, rigName)

		// list ultimately shells out to bd via internal/beads, a different
		// runner than runBdCmd's, so it acquires its own slot against the
		// bd-read pool (gt-d5xr).
		slotCtx, cancel := context.WithTimeout(context.Background(), f.waitBudget(f.cmdTimeout))
		slotErr := acquireCmdSlot(slotCtx, f.cmdSem)
		cancel()
		if slotErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: waiting for subprocess slot: %w", rigName, slotErr)
			}
			continue
		}
		listErr := func() error {
			defer releaseCmdSlot(f.cmdSem) // released on every return path, panic included
			issues, err := list(rigPath, beads.ListOptions{
				Label:    mergeRequestLabel,
				Status:   "open",
				Priority: -1, // no priority filter; 0 would mean P0 only
				Rig:      rigName,
			})
			if err == nil {
				answered++
				for _, issue := range issues {
					rows = append(rows, townMergeQueueRow(issue, rigName, now))
				}
			}
			return err
		}()
		if listErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", rigName, listErr)
			}
			continue
		}
	}
	// A rig that answered with nothing is an answer. Only a town where no read
	// succeeded is worth failing the whole refresh over, because that is the
	// case where an empty panel would be a lie rather than a fact.
	if answered == 0 && firstErr != nil {
		return TownMergeQueue{}, firstErr
	}
	if firstErr != nil {
		log.Printf("dashboard: merge queue incomplete: %v", firstErr)
	}

	sort.SliceStable(rows, func(i, j int) bool {
		iReady := rows[i].Status == "ready"
		jReady := rows[j].Status == "ready"
		if iReady != jReady {
			return iReady // what the refinery can take now, before what it cannot
		}
		if rows[i].Priority != rows[j].Priority {
			return rows[i].Priority < rows[j].Priority
		}
		return rows[i].createdAt.Before(rows[j].createdAt) // oldest first: starvation shows up as age
	})

	snapshot := TownMergeQueue{Loaded: true, Rows: rows}
	for _, row := range rows {
		if row.Status == "ready" {
			snapshot.ReadyCount++
		}
	}
	snapshot.Merges6h, snapshot.Merges6hTotal = f.mergesLast6h(rigNames)
	return snapshot, nil
}

// townMergeQueueRow renders one MR wisp, deriving ready/blocked the way
// `gt mq list` does so the dashboard and the CLI cannot disagree.
func townMergeQueueRow(issue *beads.Issue, rigName string, now time.Time) TownMergeQueueRow {
	createdAt, err := time.Parse(time.RFC3339, issue.CreatedAt)
	if err != nil {
		createdAt = now
	}

	row := TownMergeQueueRow{
		ID:        issue.ID,
		Priority:  issue.Priority,
		Rig:       rigName,
		Status:    issue.Status,
		Age:       formatMergeAge(now.Sub(createdAt)),
		createdAt: createdAt,
	}
	if fields := beads.ParseMRFields(issue); fields != nil {
		row.Branch = fields.Branch
		row.Assignee = fields.Worker
	}
	if row.Branch == "" {
		row.Branch = "(unset)"
	}
	if row.Assignee == "" {
		// MR wisps carry the worker in their description; fall back to the
		// bead's assignee, then to whoever submitted it.
		row.Assignee = formatAgentAddress(firstNonEmpty(issue.Assignee, issue.CreatedBy))
	}

	if issue.Status == "open" {
		if beads.HasUnresolvedBlockers(issue) {
			row.Status = "blocked"
		} else {
			row.Status = "ready"
		}
	}
	switch row.Status {
	case "ready":
		row.ColorClass = "mq-green"
	case "blocked":
		row.ColorClass = "mq-red"
	default:
		row.ColorClass = "mq-yellow"
	}
	return row
}

// formatMergeAge renders an age the way the `gt mq list` AGE column does.
func formatMergeAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// mergesLast6h counts merges that landed on each rig's main branch in the
// last six hours, for the panel's throughput tile.
func (f *LiveConvoyFetcher) mergesLast6h(rigNames []string) ([]RigMergeCount, int) {
	f.mergesMu.Lock()
	if time.Since(f.mergesAt) < mergesTileTTL {
		tile, total := f.mergesTile, f.mergesTotal
		f.mergesMu.Unlock()
		return tile, total
	}
	f.mergesMu.Unlock()

	count := f.countMerges
	if count == nil {
		count = countMergesOnMain
	}

	var tile []RigMergeCount
	total := 0
	for _, rigName := range rigNames {
		repoPath := filepath.Join(f.townRoot, rigName, "mayor", "rig")
		n, err := count(repoPath, mergesWindow)
		if err != nil {
			continue
		}
		tile = append(tile, RigMergeCount{Rig: rigName, Count: n})
		total += n
	}
	sort.Slice(tile, func(i, j int) bool { return tile[i].Rig < tile[j].Rig })

	// A total miss (git missing, clones absent) is retried on the next
	// refresh rather than parked for a whole TTL.
	if len(tile) == 0 && len(rigNames) > 0 {
		return tile, 0
	}

	f.mergesMu.Lock()
	f.mergesTile, f.mergesTotal, f.mergesAt = tile, total, time.Now()
	f.mergesMu.Unlock()
	return tile, total
}

// countMergesOnMain counts merge commits on origin/main newer than the window.
// origin/main, not main, so a clone that has not checked out the tip still
// counts what landed upstream.
//
// The window is spelled "N hours ago" deliberately: git's approxidate drops an
// unparseable --since value and counts every merge in history instead, so the
// obvious --since=6h renders as the repo's lifetime total (gt-r65r).
func countMergesOnMain(repoPath string, window time.Duration) (int, error) {
	since := fmt.Sprintf("--since=%d hours ago", int(window.Hours()))
	stdout, err := fetcherRunCmd(mergesCountTimeout, "git", "-C", repoPath,
		"rev-list", "--count", "--merges", since, "origin/main")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(stdout.String()))
	if err != nil {
		return 0, fmt.Errorf("parsing rev-list count: %w", err)
	}
	return n, nil
}

// polecatIndexTTL is how long a `gt polecat list` snapshot is served before a
// background refresh replaces it. The agent a session runs and its merge
// request both move on the scale of a spawn or a merge, not a render, and a
// refresh costs tens of seconds (see polecatListTimeout) — so the TTL is long
// enough that the list does not run continuously, and short enough that a
// spawned polecat names its agent well before its MR moves.
const polecatIndexTTL = 3 * time.Minute

// polecatListTimeout bounds `gt polecat list --all --json`. The list reads each
// polecat's tmux session for GT_AGENT and runs a bulk merge-request query per
// rig; measured 2026-09-20 it took 45s for 52 polecats across 5 rigs, so the
// fetcher's generic cmdTimeout (15s default) would never see an answer. The
// call runs only in the background refresh, where the budget costs a render
// nothing.
const polecatListTimeout = 90 * time.Second

// polecatListItem is the slice of `gt polecat list --json` the Polecats panel
// renders (gt-2540). Everything else in that output — verdict, blockers,
// reuse status — belongs to the workstate panels, not this one.
type polecatListItem struct {
	Rig      string `json:"rig"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Issue    string `json:"issue"`
	Agent    string `json:"agent"`
	MRID     string `json:"mr_id"`
	MRStatus string `json:"mr_status"`
}

// polecatIndex answers "which agent is this polecat running, and what happened
// to its MR" by rig and polecat name. A missing rig or name means no row: the
// panel shows an empty cell rather than inventing a status.
type polecatIndex map[string]map[string]polecatListItem

const (
	// workStatusMRPending is the Polecats panel status for a finished polecat
	// whose merge request has not landed yet (gt-ppja).
	workStatusMRPending = "mr-pending"

	// polecatStateDone is the `gt polecat list` state of a polecat that
	// submitted its work and has no session left.
	polecatStateDone = "done"
)

// mrStillPending reports whether an MR queue state still owes the town a merge.
// The vocabulary is `gt polecat list`'s (internal/cmd/polecat_inventory.go):
// merged and rejected are terminal, and missing means no bead backs the
// pointer — none of the three is a reason to keep showing the row.
func mrStillPending(status string) bool {
	switch status {
	case "open", "ready", "blocked":
		return true
	default:
		return false
	}
}

// polecatLister derives the town's polecat inventory. It is the seam tests
// replace so the panel renders without spawning gt.
type polecatLister func() ([]byte, error)

// listTownPolecats is the production lister: `gt polecat list --all --json`,
// one call for every rig. Listing per rig would multiply the bd subprocesses
// the list already pays for, and measured per-rig times add up to the same
// total, so one process doing them in sequence beats five concurrent ones.
func (f *LiveConvoyFetcher) listTownPolecats() ([]byte, error) {
	stdout, err := f.runGtCmd(polecatListTimeout, "polecat", "list", "--all", "--json")
	if err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// runGtCmd executes a gt subcommand from the town root. The fetcher's other
// subprocess runners are bd- and tmux-specific; the polecat inventory is
// gt's, and it is the fetcher's long-running child, so it draws its slot
// from userCmdSem (gt-d5xr).
func (f *LiveConvoyFetcher) runGtCmd(timeout time.Duration, args ...string) (*bytes.Buffer, error) {
	waitCtx, cancelWait := context.WithTimeout(context.Background(), f.waitBudget(timeout))
	if err := acquireCmdSlot(waitCtx, f.userCmdSem); err != nil {
		cancelWait()
		return nil, fmt.Errorf("gt %s: waiting for subprocess slot: %w", strings.Join(args, " "), err)
	}
	cancelWait()
	defer releaseCmdSlot(f.userCmdSem)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	bin := f.gtBin
	if bin == "" {
		bin = "gt"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = f.townRoot
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Run()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("gt %s timed out after %v", strings.Join(args, " "), timeout)
		}
		// gt may exit non-zero after writing usable JSON.
		if stdout.Len() > 0 {
			return &stdout, nil
		}
		return nil, err
	}
	return &stdout, nil
}

// workerPolecatIndex returns the cached polecat snapshot, starting a background
// refresh when it is stale. No render ever pays for the list, which is what
// keeps the polecat inventory off the per-poll process budget: before gt-kqi2
// the panel's agent and MR cells had no source at all, and the obvious source
// (`gt polecat list` inline) runs for the better part of a minute.
//
// The cost of that is a blank AGENT and MR cell until the first refresh lands.
// A blank column is the honest reading — the inventory is not loaded yet — and
// the alternative, blocking a render on the list, would hold the page for as
// long as the list takes.
func (f *LiveConvoyFetcher) workerPolecatIndex() polecatIndex {
	f.polecatMu.Lock()
	index, fetchedAt := f.polecatIndex, f.polecatFetchedAt
	f.polecatMu.Unlock()

	if (fetchedAt.IsZero() || time.Since(fetchedAt) >= polecatIndexTTL) && f.polecatBreaker.allow() {
		go f.refreshPolecatIndex()
	}
	return index
}

// refreshPolecatIndex republishes the snapshot on success. A failed refresh
// keeps the previous one — a stale agent name beats a blank column — and the
// breaker decides when to try again.
func (f *LiveConvoyFetcher) refreshPolecatIndex() {
	index, err := f.polecatIndexSnapshot()
	if err != nil {
		f.polecatBreaker.recordFailure()
		log.Printf("dashboard: polecat inventory refresh failed: %v", err)
		return
	}
	f.polecatBreaker.recordSuccess()

	f.polecatMu.Lock()
	f.polecatIndex = mergePolecatIndexes(f.polecatIndex, index)
	f.polecatFetchedAt = time.Now()
	f.polecatMu.Unlock()
}

// mergePolecatIndexes carries a rig's previous rows into a fresh snapshot that
// has nothing for it. `gt polecat list --all` warns and exits 0 when one rig's
// database is unreadable, so that rig arrives absent rather than empty — and an
// absent rig would blank its Agent and MR cells for a whole TTL, which is the
// outcome keeping the previous snapshot is meant to avoid.
//
// Carrying rows forward cannot show a wrong agent: a carried row is only read
// for a polecat that has a live session right now, which is the case where the
// previous read is the best answer available.
func mergePolecatIndexes(previous, fresh polecatIndex) polecatIndex {
	if len(previous) == 0 {
		return fresh
	}
	merged := make(polecatIndex, len(fresh)+1)
	for rig, rows := range fresh {
		merged[rig] = rows
	}
	for rig, rows := range previous {
		if _, ok := merged[rig]; !ok {
			merged[rig] = rows
		}
	}
	return merged
}

// polecatIndexSnapshot runs the list and indexes its rows. A rig that answers
// with no polecats is an answer; only an unreadable list is an error.
func (f *LiveConvoyFetcher) polecatIndexSnapshot() (polecatIndex, error) {
	list := f.listPolecats
	if list == nil {
		list = f.listTownPolecats
	}

	raw, err := list()
	if err != nil {
		return nil, fmt.Errorf("gt polecat list: %w", err)
	}

	var items []polecatListItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("parsing gt polecat list output: %w", err)
	}

	index := make(polecatIndex, len(items))
	for _, item := range items {
		if item.Rig == "" || item.Name == "" {
			continue
		}
		if index[item.Rig] == nil {
			index[item.Rig] = make(map[string]polecatListItem)
		}
		index[item.Rig][item.Name] = item
	}
	return index, nil
}

// FetchWorkers fetches all running worker sessions (polecats and refinery) with activity data.
func (f *LiveConvoyFetcher) FetchWorkers() ([]WorkerRow, error) {
	// Load registered rigs to filter sessions
	rigsConfigPath := filepath.Join(f.townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading rigs config: %w", err)
	}

	// Build set of registered rig names
	registeredRigs := make(map[string]bool)
	for rigName := range rigsConfig.Rigs {
		registeredRigs[rigName] = true
	}

	// Pre-fetch assigned issues map: assignee -> (issueID, title)
	assignedIssues := f.getAssignedIssuesMap()

	// Pre-fetch the polecat inventory that carries each worker's coding agent
	// and merge request (gt-kqi2). One list covers every rig.
	polecats := f.workerPolecatIndex()

	// Query all tmux sessions with window_activity for more accurate timing
	stdout, err := f.runTmuxCmd("list-sessions", "-F", "#{session_name}|#{window_activity}")
	if err != nil {
		// tmux not running or no sessions
		return nil, nil
	}

	// Pre-fetch merge queue count to determine refinery idle status
	mergeQueueCount := f.getMergeQueueCount()

	var workers []WorkerRow
	// rendered records the rig/name pairs the tmux pass already covers, so the
	// inventory pass below adds a row only for a polecat with no session.
	rendered := make(map[string]bool)
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")

	for _, line := range lines {
		if line == "" {
			continue
		}

		parts := strings.Split(line, "|")
		if len(parts) < 2 {
			continue
		}

		sessionName := parts[0]

		// Parse session name using the fetcher's own registry to avoid
		// dependency on global DefaultRegistry initialization (gt-y24).
		// A parse failure just means this tmux session isn't a Gas Town
		// agent (a dev shell, editor, or the dashboard's own "hq-dashboard"
		// session when the dashboard itself runs under the agent tmux
		// server) — expected and common, not worth logging (gt-978i).
		identity, err := session.ParseSessionNameWithRegistry(sessionName, f.registry)
		if err != nil {
			continue
		}

		rig := identity.Rig

		// Skip rigs not registered in this workspace
		if !registeredRigs[rig] {
			continue
		}

		// Skip non-worker sessions (witness, mayor, deacon, boot)
		switch identity.Role {
		case session.RoleMayor, session.RoleDeacon, session.RoleWitness:
			continue
		}

		// Determine agent type and worker name
		workerName := identity.Name
		agentType := constants.RolePolecat // Default for ephemeral sessions (polecats, crew)
		if identity.Role == session.RoleRefinery {
			agentType = constants.RoleRefinery
			// Refinery identities carry no per-agent Name (there's one
			// refinery per rig, so AgentIdentity.Name is always empty for
			// this role) - without this the Polecats panel rendered an
			// unlabeled "refinery" row, and the workerName == "refinery"
			// check below (for status hints) could never match.
			workerName = "refinery"
		}

		// Parse activity timestamp
		var activityUnix int64
		if _, err := fmt.Sscanf(parts[1], "%d", &activityUnix); err != nil || activityUnix == 0 {
			continue
		}
		activityTime := time.Unix(activityUnix, 0)
		activityAge := time.Since(activityTime)

		// Get status hint - special handling for refinery
		var statusHint string
		if workerName == "refinery" {
			statusHint = f.getRefineryStatusHint(mergeQueueCount)
		} else {
			statusHint = f.getWorkerStatusHint(sessionName)
		}

		// Look up assigned issue for this worker
		// Assignee format: "rigname/polecats/workername"
		assignee := fmt.Sprintf("%s/polecats/%s", rig, workerName)
		var issueID, issueTitle string
		if issue, ok := assignedIssues[assignee]; ok {
			issueID = issue.ID
			issueTitle = issue.Title
			// Keep full title - CSS handles overflow
		}

		// The inventory is keyed by rig and polecat name, so only real polecats
		// may read it. A crew session shares the polecat name space — a crew
		// member named for a polecat in the same rig would otherwise wear that
		// polecat's agent and, worse, its "Idle (merged)" — and the refinery for
		// a rig has no inventory row at all.
		var polecat polecatListItem
		hasPolecat := identity.Role == session.RolePolecat
		if hasPolecat {
			polecat, hasPolecat = polecats[rig][workerName]
		}

		// The hq map only sees town-root beads in in_progress; a slung
		// polecat's bead is in the rig DB in status hooked, so it never
		// matches. The inventory row carries the hooked issue (gt-bcfc).
		if issueID == "" && hasPolecat && polecat.Issue != "" {
			issueID = polecat.Issue
		}

		// Calculate work status based on activity age and issue assignment
		workStatus := calculateWorkerWorkStatus(activityAge, issueID, workerName, f.staleThreshold, f.stuckThreshold)

		worker := WorkerRow{
			Name:         workerName,
			Rig:          rig,
			SessionID:    sessionName,
			LastActivity: activity.Calculate(activityTime),
			StatusHint:   statusHint,
			IssueID:      issueID,
			IssueTitle:   issueTitle,
			WorkStatus:   workStatus,
			AgentType:    agentType,
		}
		if hasPolecat {
			worker.Agent = polecat.Agent
			worker.MRID = polecat.MRID
			worker.MRStatus = polecat.MRStatus
		}
		workers = append(workers, worker)
		rendered[workerKey(rig, workerName)] = true
	}

	workers = append(workers, pendingMRWorkers(polecats, rendered)...)

	return workers, nil
}

// pendingMRWorkers mints a row for every done polecat whose merge request is
// still in flight and which no tmux session covers (gt-ppja).
//
// The tmux pass can only see live sessions, and a polecat's session is gone by
// the time its MR is the thing the town is waiting on — so the one worker with
// work outstanding was the one the panel hid, and the MR column never had a
// row to fill. The inventory already answers both halves of the question.
//
// The row disappears the way the merge does: mrStillPending goes false once
// the MR is terminal, and nuking the polecat drops its inventory row entirely.
func pendingMRWorkers(polecats polecatIndex, rendered map[string]bool) []WorkerRow {
	var workers []WorkerRow
	for rig, rows := range polecats {
		for name, polecat := range rows {
			if rendered[workerKey(rig, name)] || !mrStillPending(polecat.MRStatus) {
				continue
			}
			if polecat.State != polecatStateDone {
				continue
			}
			workers = append(workers, WorkerRow{
				Name: name,
				Rig:  rig,
				// No session means no activity clock to read. The zero time
				// renders as "unknown", the same reading an MR with no bead
				// behind it gets, rather than an empty cell.
				LastActivity: activity.Calculate(time.Time{}),
				IssueID:      polecat.Issue,
				WorkStatus:   workStatusMRPending,
				AgentType:    constants.RolePolecat,
				Agent:        polecat.Agent,
				MRID:         polecat.MRID,
				MRStatus:     polecat.MRStatus,
			})
		}
	}
	// Deterministic order: polecatIndex is a map, and a panel whose rows
	// reshuffle between renders is unreadable next to the sorted panels.
	sort.Slice(workers, func(i, j int) bool {
		if workers[i].Rig != workers[j].Rig {
			return workers[i].Rig < workers[j].Rig
		}
		return workers[i].Name < workers[j].Name
	})
	return workers
}

// workerKey identifies one polecat within its rig. A polecat name is unique
// per rig, so the rig must be part of the key.
func workerKey(rig, name string) string {
	return rig + "/" + name
}

// assignedIssue holds issue info for the assigned issues map.
type assignedIssue struct {
	ID    string
	Title string
}

// getAssignedIssuesMap returns a map of assignee -> assigned issue.
// Queries beads for all in_progress issues with assignees.
func (f *LiveConvoyFetcher) getAssignedIssuesMap() map[string]assignedIssue {
	result := make(map[string]assignedIssue)

	// Query all in_progress issues (these are the ones being worked on)
	stdout, err := f.runBdCmd(f.townRoot, "list", "--status=in_progress", "--json")
	if err != nil {
		log.Printf("warning: bd list in_progress failed: %v", err)
		return result
	}

	var issues []struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		Assignee string `json:"assignee"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &issues); err != nil {
		log.Printf("warning: parsing bd list output: %v", err)
		return result
	}

	for _, issue := range issues {
		if issue.Assignee != "" {
			result[issue.Assignee] = assignedIssue{
				ID:    issue.ID,
				Title: issue.Title,
			}
		}
	}

	return result
}

// calculateWorkerWorkStatus determines the worker's work status based on activity and assignment.
// Returns: "working", "stale", "stuck", or "idle"
func calculateWorkerWorkStatus(activityAge time.Duration, issueID, workerName string, staleThreshold, stuckThreshold time.Duration) string {
	// Refinery has special handling - it's always "working" if it has PRs
	if workerName == "refinery" {
		return "working"
	}

	// No issue assigned = idle
	if issueID == "" {
		return "idle"
	}

	// Has issue - determine status based on activity
	switch {
	case activityAge < staleThreshold:
		return "working" // Active recently
	case activityAge < stuckThreshold:
		return "stale" // Might be thinking or stuck
	default:
		return "stuck" // Likely stuck - no activity for threshold+ minutes
	}
}

// getWorkerStatusHint captures the last non-empty line from a worker's pane.
func (f *LiveConvoyFetcher) getWorkerStatusHint(sessionName string) string {
	stdout, err := f.runTmuxCmd("capture-pane", "-t", sessionName, "-p", "-J")
	if err != nil {
		return ""
	}

	// Get last non-empty line
	lines := strings.Split(stdout.String(), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			// Truncate long lines
			if len(line) > 60 {
				line = line[:57] + "..."
			}
			return line
		}
	}
	return ""
}

// getMergeQueueCount returns the total number of open PRs across all repos.
func (f *LiveConvoyFetcher) getMergeQueueCount() int {
	mergeQueue, err := f.FetchMergeQueue()
	if err != nil {
		return 0
	}
	return len(mergeQueue)
}

// getRefineryStatusHint returns appropriate status for refinery based on merge queue.
func (f *LiveConvoyFetcher) getRefineryStatusHint(mergeQueueCount int) string {
	if mergeQueueCount == 0 {
		return "Idle - Waiting for PRs"
	}
	if mergeQueueCount == 1 {
		return "Processing 1 PR"
	}
	return fmt.Sprintf("Processing %d PRs", mergeQueueCount)
}

// parseActivityTimestamp parses a Unix timestamp string from tmux.
// Returns (0, false) for invalid or zero timestamps.
func parseActivityTimestamp(s string) (int64, bool) {
	var unix int64
	if _, err := fmt.Sscanf(s, "%d", &unix); err != nil || unix <= 0 {
		return 0, false
	}
	return unix, true
}

// FetchMail fetches recent mail messages from the beads database.
func (f *LiveConvoyFetcher) FetchMail() ([]MailRow, error) {
	// List all message issues (mail)
	stdout, err := f.runBdCmd(f.townRoot, "list", "--label=gt:message", "--json", "--limit=50")
	if err != nil {
		return nil, fmt.Errorf("listing mail: %w", err)
	}

	var messages []struct {
		ID        string   `json:"id"`
		Title     string   `json:"title"`
		Status    string   `json:"status"`
		CreatedAt string   `json:"created_at"`
		Priority  int      `json:"priority"`
		Assignee  string   `json:"assignee"`   // "to" address stored here
		CreatedBy string   `json:"created_by"` // "from" address
		Labels    []string `json:"labels"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &messages); err != nil {
		return nil, fmt.Errorf("parsing mail list: %w", err)
	}

	rows := make([]MailRow, 0, len(messages))
	for _, m := range messages {
		// Parse timestamp
		var timestamp time.Time
		var age string
		var sortKey int64
		if m.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
				timestamp = t
				age = formatTimestamp(t)
				sortKey = t.Unix()
			}
		}

		// Determine priority string
		priorityStr := "normal"
		switch m.Priority {
		case 0:
			priorityStr = "urgent"
		case 1:
			priorityStr = "high"
		case 2:
			priorityStr = "normal"
		case 3, 4:
			priorityStr = "low"
		}

		// Determine message type from labels
		msgType := "notification"
		for _, label := range m.Labels {
			if label == "task" || label == "reply" || label == "scavenge" {
				msgType = label
				break
			}
		}

		// Format from/to addresses for display
		from := formatAgentAddress(m.CreatedBy)
		to := formatAgentAddress(m.Assignee)

		rows = append(rows, MailRow{
			ID:        m.ID,
			From:      from,
			FromRaw:   m.CreatedBy,
			To:        to,
			Subject:   m.Title,
			Timestamp: timestamp.Local().Format("15:04"),
			Age:       age,
			Priority:  priorityStr,
			Type:      msgType,
			Read:      m.Status == "closed",
			SortKey:   sortKey,
		})
	}

	// Sort by timestamp, newest first
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].SortKey > rows[j].SortKey
	})

	return rows, nil
}

// formatMailAge returns a human-readable age string.
func formatMailAge(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// formatTimestamp formats a time as "Jan 26, 3:45 PM" (or "Jan 26 2006, 3:45 PM" if different year).
// The input may carry any zone (e.g. UTC from time.Parse on an RFC3339 "Z" value);
// it is converted to local time so it renders consistently with timestamps built
// via time.Unix(), which are already local.
func formatTimestamp(t time.Time) string {
	t = t.Local()
	now := time.Now()
	if t.Year() != now.Year() {
		return t.Format("Jan 2 2006, 3:04 PM")
	}
	return t.Format("Jan 2, 3:04 PM")
}

// formatAgentAddress shortens agent addresses for display.
// "gastown/polecats/Toast" -> "Toast (gastown)"
// "mayor/" -> "Mayor"
func formatAgentAddress(addr string) string {
	if addr == "" {
		return "—"
	}
	if addr == "mayor/" || addr == "mayor" {
		return "Mayor"
	}

	parts := strings.Split(addr, "/")
	if len(parts) >= 3 && parts[1] == "polecats" {
		return fmt.Sprintf("%s (%s)", parts[2], parts[0])
	}
	if len(parts) >= 3 && parts[1] == "crew" {
		return fmt.Sprintf("%s (%s/crew)", parts[2], parts[0])
	}
	if len(parts) >= 2 {
		return fmt.Sprintf("%s/%s", parts[0], parts[len(parts)-1])
	}
	return addr
}

// countLivePolecatsByRig returns per-rig counts of live polecat tmux sessions.
// The Rigs panel's polecat count must reflect the actual running pool, not
// leftover worktree directories under <rig>/polecats/: those persist after a
// polecat finishes (gt done) or crashes without full cleanup, which inflated
// the displayed count well beyond the live pool.
func (f *LiveConvoyFetcher) countLivePolecatsByRig() map[string]int {
	stdout, err := f.runTmuxCmd("list-sessions", "-F", "#{session_name}")
	if err != nil {
		// tmux not running or no sessions - no live polecats anywhere.
		return nil
	}

	counts := make(map[string]int)
	for _, sessionName := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if sessionName == "" {
			continue
		}
		identity, err := session.ParseSessionNameWithRegistry(sessionName, f.registry)
		if err != nil || identity.Role != session.RolePolecat {
			continue
		}
		counts[identity.Rig]++
	}
	return counts
}

// FetchRigs returns all registered rigs with their agent counts.
func (f *LiveConvoyFetcher) FetchRigs() ([]RigRow, error) {
	// Load rigs config from mayor/rigs.json
	rigsConfigPath := filepath.Join(f.townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading rigs config: %w", err)
	}

	livePolecatCounts := f.countLivePolecatsByRig()

	// A parked rig keeps its witness and refinery icons from the last session
	// state, so the counts alone cannot tell "paused" from "running" — and
	// the dashboard exists to catch the difference.
	rigNames := make([]string, 0, len(rigsConfig.Rigs))
	for name := range rigsConfig.Rigs {
		rigNames = append(rigNames, name)
	}
	opLabels := f.rigOpLabels(rigNames)

	var rows []RigRow
	for name, entry := range rigsConfig.Rigs {
		row := RigRow{
			Name:         name,
			GitURL:       entry.GitURL,
			PolecatCount: livePolecatCounts[name],
			OpState:      opLabels[name],
		}

		rigPath := filepath.Join(f.townRoot, name)

		// Count crew
		crewDir := filepath.Join(rigPath, "crew")
		if entries, err := os.ReadDir(crewDir); err == nil {
			for _, e := range entries {
				if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
					row.CrewCount++
				}
			}
		}

		// Check for witness
		witnessPath := filepath.Join(rigPath, "witness")
		if _, err := os.Stat(witnessPath); err == nil {
			row.HasWitness = true
		}

		// Check for refinery
		refineryPath := filepath.Join(rigPath, "refinery", "rig")
		if _, err := os.Stat(refineryPath); err == nil {
			row.HasRefinery = true
		}

		rows = append(rows, row)
	}

	// Sort by name
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Name < rows[j].Name
	})

	return rows, nil
}

// FetchDogs returns all dogs in the kennel with their state.
func (f *LiveConvoyFetcher) FetchDogs() ([]DogRow, error) {
	kennelPath := filepath.Join(f.townRoot, "deacon", "dogs")

	entries, err := os.ReadDir(kennelPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No kennel yet
		}
		return nil, fmt.Errorf("reading kennel: %w", err)
	}

	var rows []DogRow
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}

		// Read dog state file
		stateFile := filepath.Join(kennelPath, name, ".dog.json")
		data, err := os.ReadFile(stateFile)
		if err != nil {
			continue // Not a valid dog
		}

		var state struct {
			Name       string            `json:"name"`
			State      string            `json:"state"`
			LastActive time.Time         `json:"last_active"`
			Work       string            `json:"work,omitempty"`
			Worktrees  map[string]string `json:"worktrees,omitempty"`
		}
		if err := json.Unmarshal(data, &state); err != nil {
			continue
		}

		rows = append(rows, DogRow{
			Name:       state.Name,
			State:      state.State,
			Work:       state.Work,
			LastActive: formatTimestamp(state.LastActive),
			RigCount:   len(state.Worktrees),
		})
	}

	// Sort by name
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Name < rows[j].Name
	})

	return rows, nil
}

// FetchEscalations returns open escalations needing attention.
//
// Escalations are created as ephemeral wisps (gt-fcsf), which bd list hides
// by default — without --include-infra this silently returned zero rows
// while open escalations sat invisible on the dashboard.
func (f *LiveConvoyFetcher) FetchEscalations() ([]EscalationRow, error) {
	// List open escalations
	stdout, err := f.runBdCmd(f.townRoot, "list", "--label=gt:escalation", "--status=open", "--include-infra", "--json")
	if err != nil {
		return nil, nil // No escalations or bd not available
	}

	var issues []struct {
		ID          string   `json:"id"`
		Title       string   `json:"title"`
		CreatedAt   string   `json:"created_at"`
		CreatedBy   string   `json:"created_by"`
		Labels      []string `json:"labels"`
		Description string   `json:"description"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &issues); err != nil {
		return nil, fmt.Errorf("parsing escalations: %w", err)
	}

	var rows []EscalationRow
	for _, issue := range issues {
		// Escalation mail-delivery beads carry the same gt:escalation label
		// (see mail.Router.buildLabels) so the routed notification can be
		// found by ack/close, but they aren't escalation wisps themselves —
		// skip them so the dashboard counts open escalations, not deliveries.
		isDelivery := false
		for _, label := range issue.Labels {
			if label == "gt:message" {
				isDelivery = true
				break
			}
		}
		if isDelivery {
			continue
		}

		row := EscalationRow{
			ID:          issue.ID,
			Title:       issue.Title,
			EscalatedBy: formatAgentAddress(issue.CreatedBy),
			Severity:    "medium", // default
		}

		// Parse severity from labels
		for _, label := range issue.Labels {
			if strings.HasPrefix(label, "severity:") {
				row.Severity = strings.TrimPrefix(label, "severity:")
			}
			if label == "acked" {
				row.Acked = true
			}
		}

		// Calculate age
		if issue.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, issue.CreatedAt); err == nil {
				row.Age = formatTimestamp(t)
			}
		}

		rows = append(rows, row)
	}

	// Sort by severity (critical first), then by age
	severityOrder := map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}
	sort.Slice(rows, func(i, j int) bool {
		si, sj := severityOrder[rows[i].Severity], severityOrder[rows[j].Severity]
		return si < sj
	})

	return rows, nil
}

// FetchHealth returns system health status.
func (f *LiveConvoyFetcher) FetchHealth() (*HealthRow, error) {
	row := &HealthRow{}

	// Read deacon heartbeat
	heartbeatFile := filepath.Join(f.townRoot, "deacon", "heartbeat.json")
	if data, err := os.ReadFile(heartbeatFile); err == nil {
		var hb struct {
			LastHeartbeat   time.Time `json:"timestamp"`
			Cycle           int64     `json:"cycle"`
			HealthyAgents   int       `json:"healthy_agents"`
			UnhealthyAgents int       `json:"unhealthy_agents"`
		}
		if err := json.Unmarshal(data, &hb); err == nil {
			row.DeaconCycle = hb.Cycle
			row.HealthyAgents = hb.HealthyAgents
			row.UnhealthyAgents = hb.UnhealthyAgents
			if !hb.LastHeartbeat.IsZero() {
				age := time.Since(hb.LastHeartbeat)
				row.DeaconHeartbeat = formatTimestamp(hb.LastHeartbeat)
				row.HeartbeatFresh = age < f.heartbeatFreshThreshold
			} else {
				row.DeaconHeartbeat = "no timestamp"
			}
		}
	} else {
		row.DeaconHeartbeat = "no heartbeat"
	}

	// Check pause state
	pauseFile := filepath.Join(f.townRoot, ".runtime", "deacon", "paused.json")
	if data, err := os.ReadFile(pauseFile); err == nil {
		var pause struct {
			Paused bool   `json:"paused"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(data, &pause); err == nil {
			row.IsPaused = pause.Paused
			row.PauseReason = pause.Reason
		}
	}

	return row, nil
}

// FetchQueues returns work queues and their status.
func (f *LiveConvoyFetcher) FetchQueues() ([]QueueRow, error) {
	// List queue beads
	stdout, err := f.runBdCmd(f.townRoot, "list", "--label=gt:queue", "--json")
	if err != nil {
		return nil, nil // No queues or bd not available
	}

	var queues []struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		Status      string `json:"status"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &queues); err != nil {
		return nil, fmt.Errorf("parsing queues: %w", err)
	}

	var rows []QueueRow
	for _, q := range queues {
		row := QueueRow{
			Name:   q.Title,
			Status: q.Status,
		}

		// Parse counts from description (key: value format)
		// Best-effort parsing - ignore Sscanf errors as missing/malformed data is acceptable
		for _, line := range strings.Split(q.Description, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "available_count:") {
				_, _ = fmt.Sscanf(line, "available_count: %d", &row.Available)
			} else if strings.HasPrefix(line, "processing_count:") {
				_, _ = fmt.Sscanf(line, "processing_count: %d", &row.Processing)
			} else if strings.HasPrefix(line, "completed_count:") {
				_, _ = fmt.Sscanf(line, "completed_count: %d", &row.Completed)
			} else if strings.HasPrefix(line, "failed_count:") {
				_, _ = fmt.Sscanf(line, "failed_count: %d", &row.Failed)
			} else if strings.HasPrefix(line, "status:") {
				// Override with parsed status if present
				var s string
				_, _ = fmt.Sscanf(line, "status: %s", &s)
				if s != "" {
					row.Status = s
				}
			}
		}

		rows = append(rows, row)
	}

	return rows, nil
}

// FetchSessions returns active tmux sessions with role detection.
func (f *LiveConvoyFetcher) FetchSessions() ([]SessionRow, error) {
	// List tmux sessions. window_activity, not session_activity: the session
	// clock only advances for a session with an attached client, so every
	// detached agent showed its creation time (gt-vcfs). The window clock
	// advances on real pane output — a live agent's spinner counts, so it
	// distinguishes alive from dead, not busy from idle.
	stdout, err := f.runTmuxCmd("list-sessions", "-F", "#{session_name}:#{window_activity}")
	if err != nil {
		return nil, nil // tmux not running or no sessions
	}

	var rows []SessionRow
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line == "" {
			continue
		}

		// SplitN always returns >= 1 element; parts[0] is safe unconditionally
		parts := strings.SplitN(line, ":", 2)
		name := parts[0]

		// Only include Gas Town sessions
		if !session.IsKnownSession(name) {
			continue
		}

		row := SessionRow{
			Name:    name,
			IsAlive: true, // Session exists
		}

		// Parse activity timestamp
		if len(parts) > 1 {
			if ts, ok := parseActivityTimestamp(parts[1]); ok && ts > 0 {
				row.Activity = formatTimestamp(time.Unix(ts, 0))
			}
		}

		// Detect role from session name using fetcher's own registry (gt-y24)
		if identity, err := session.ParseSessionNameWithRegistry(name, f.registry); err == nil {
			row.Rig = identity.Rig
			row.Role = string(identity.Role)
			row.Worker = identity.Name
		}

		rows = append(rows, row)
	}

	// Sort by rig, then role, then worker
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Rig != rows[j].Rig {
			return rows[i].Rig < rows[j].Rig
		}
		if rows[i].Role != rows[j].Role {
			return rows[i].Role < rows[j].Role
		}
		return rows[i].Worker < rows[j].Worker
	})

	return rows, nil
}

// FetchHooks returns all hooked beads (work pinned to agents).
func (f *LiveConvoyFetcher) FetchHooks() ([]HookRow, error) {
	// Query all beads with status=hooked
	stdout, err := f.runBdCmd(f.townRoot, "list", "--status=hooked", "--json", "--limit=0")
	if err != nil {
		return nil, nil // No hooked beads or bd not available
	}

	var beads []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Assignee  string `json:"assignee"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &beads); err != nil {
		return nil, fmt.Errorf("parsing hooked beads: %w", err)
	}

	var rows []HookRow
	for _, bead := range beads {
		row := HookRow{
			ID:       bead.ID,
			Title:    bead.Title,
			Assignee: bead.Assignee,
			Agent:    formatAgentAddress(bead.Assignee),
		}

		// Keep full title - CSS handles overflow

		// Calculate age and stale status
		if bead.UpdatedAt != "" {
			if t, err := time.Parse(time.RFC3339, bead.UpdatedAt); err == nil {
				age := time.Since(t)
				row.Age = formatTimestamp(t)
				row.IsStale = age > time.Hour // Stale if hooked > 1 hour
			}
		}

		rows = append(rows, row)
	}

	// Sort by stale first (stuck work), then by age
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].IsStale != rows[j].IsStale {
			return rows[i].IsStale // Stale items first
		}
		return rows[i].Age > rows[j].Age
	})

	return rows, nil
}

// FetchMayor returns the Mayor's current status.
func (f *LiveConvoyFetcher) FetchMayor() (*MayorStatus, error) {
	status := &MayorStatus{
		IsAttached: false,
	}

	// Get the actual mayor session name (e.g., "hq-mayor")
	mayorSessionName := session.MayorSessionName()

	// Check if mayor tmux session exists. window_activity: the session clock
	// only moves while a client is attached (gt-vcfs).
	stdout, err := f.runTmuxCmd("list-sessions", "-F", "#{session_name}:#{window_activity}")
	if err != nil {
		// tmux not running or no sessions
		return status, nil
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, mayorSessionName+":") {
			status.IsAttached = true
			status.SessionName = mayorSessionName

			// Parse activity timestamp
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				if activityTs, ok := parseActivityTimestamp(parts[1]); ok {
					age := time.Since(time.Unix(activityTs, 0))
					status.LastActivity = formatTimestamp(time.Unix(activityTs, 0))
					status.IsActive = age < f.mayorActiveThreshold
				}
			}
			break
		}
	}

	if status.IsAttached {
		status.Runtime = f.resolveMayorRuntime(mayorSessionName)
	}

	return status, nil
}

func (f *LiveConvoyFetcher) resolveMayorRuntime(sessionName string) string {
	if agentName, err := fetcherGetSessionEnv(sessionName, "GT_AGENT"); err == nil && strings.TrimSpace(agentName) != "" {
		agentName = strings.TrimSpace(agentName)
		rc, _, resolveErr := config.ResolveAgentConfigWithOverride(f.townRoot, "", agentName)
		if resolveErr == nil {
			return runtimeLabelForRuntimeConfig(rc, agentName)
		}
		if roleRC := config.ResolveRoleAgentConfig(constants.RoleMayor, f.townRoot, ""); roleRC != nil && strings.TrimSpace(roleRC.ResolvedAgent) == agentName {
			return runtimeLabelForRuntimeConfig(roleRC, agentName)
		}
		return agentName
	}

	return runtimeLabelForRuntimeConfig(config.ResolveRoleAgentConfig(constants.RoleMayor, f.townRoot, ""), "")
}

func runtimeLabelForRuntimeConfig(rc *config.RuntimeConfig, fallback string) string {
	if rc == nil {
		if fallback != "" {
			return fallback
		}
		return "claude"
	}
	if fallback == "" {
		fallback = rc.ResolvedAgent
	}
	return runtimeLabelFromConfig(rc.Command, rc.Args, fallback)
}

func runtimeLabelFromConfig(command string, args []string, fallback string) string {
	command = strings.TrimSpace(command)
	cmd := ""
	if command != "" {
		cmd = strings.TrimSpace(filepath.Base(command))
	}
	if cmd == "" {
		cmd = fallback
	}
	if cmd == "" {
		cmd = "claude"
	}
	if cmd == "cgroup-wrap" && len(args) > 0 {
		cmd = filepath.Base(args[0])
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if (arg == "--model" || arg == "-m") && i+1 < len(args) && strings.TrimSpace(args[i+1]) != "" {
			return cmd + "/" + stripModelSuffix(strings.TrimSpace(args[i+1]))
		}
		if strings.HasPrefix(arg, "--model=") {
			if v := strings.TrimSpace(strings.TrimPrefix(arg, "--model=")); v != "" {
				return cmd + "/" + stripModelSuffix(v)
			}
		}
		if strings.HasPrefix(arg, "-m=") {
			if v := strings.TrimSpace(strings.TrimPrefix(arg, "-m=")); v != "" {
				return cmd + "/" + stripModelSuffix(v)
			}
		}
	}

	return cmd
}

// stripModelSuffix removes bracketed context-window hints (e.g. "[1m]")
// from model names so the dashboard label stays human-readable.
// "sonnet[1m]" → "sonnet", "opus" → "opus".
func stripModelSuffix(model string) string {
	if idx := strings.Index(model, "["); idx > 0 {
		return model[:idx]
	}
	return model
}

// FetchIssues returns open issues (the backlog).
func (f *LiveConvoyFetcher) FetchIssues() ([]IssueRow, error) {
	// Query both open AND hooked issues for the Work panel
	// Open = ready to assign, Hooked = in progress.
	//
	// The type tag is "issue_type" because that is what bd emits; reading "type"
	// left every bead typed as "" (gt-b9wq).
	var allBeads []struct {
		ID        string   `json:"id"`
		Title     string   `json:"title"`
		Type      string   `json:"issue_type"`
		Priority  int      `json:"priority"`
		Labels    []string `json:"labels"`
		CreatedAt string   `json:"created_at"`
	}

	// Fetch open issues
	if stdout, err := f.runBdCmd(f.townRoot, "list", "--status=open", "--json", "--limit=50"); err == nil {
		var openBeads []struct {
			ID        string   `json:"id"`
			Title     string   `json:"title"`
			Type      string   `json:"issue_type"`
			Priority  int      `json:"priority"`
			Labels    []string `json:"labels"`
			CreatedAt string   `json:"created_at"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &openBeads); err == nil {
			allBeads = append(allBeads, openBeads...)
		}
	}

	// Fetch hooked issues (in progress)
	if stdout, err := f.runBdCmd(f.townRoot, "list", "--status=hooked", "--json", "--limit=50"); err == nil {
		var hookedBeads []struct {
			ID        string   `json:"id"`
			Title     string   `json:"title"`
			Type      string   `json:"issue_type"`
			Priority  int      `json:"priority"`
			Labels    []string `json:"labels"`
			CreatedAt string   `json:"created_at"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &hookedBeads); err == nil {
			allBeads = append(allBeads, hookedBeads...)
		}
	}

	var rows []IssueRow
	for _, bead := range allBeads {
		// Skip the bookkeeping families. This panel draws a Sling button on
		// every row it shows, so a row no polecat can take is a button that
		// cannot work — the same rule, and the same definition, as the Ready
		// lists (gt-b9wq).
		//
		// gt:keep and gt:standing-orders are excepted: ProtectedIssueLabel
		// groups them with gt:role/gt:rig for a different question (what
		// automated completion must not auto-close), but a kept or
		// standing-orders task is still ordinary work a polecat can sling —
		// the label marks it as protected from cleanup, not as internal
		// bookkeeping the way a mail or merge-slot bead is.
		if isWorkPanelNonDispatchable(bead.Type, bead.Labels, bead.Title) {
			continue
		}

		row := IssueRow{
			ID:       bead.ID,
			Title:    bead.Title,
			Type:     bead.Type,
			Priority: bead.Priority,
		}

		// Keep full title - CSS handles overflow

		// Format labels (skip internal labels)
		var displayLabels []string
		for _, label := range bead.Labels {
			if !strings.HasPrefix(label, "gt:") && !strings.HasPrefix(label, "internal:") {
				displayLabels = append(displayLabels, label)
			}
		}
		if len(displayLabels) > 0 {
			row.Labels = strings.Join(displayLabels, ", ")
			if len(row.Labels) > 25 {
				row.Labels = row.Labels[:22] + "..."
			}
		}

		// Calculate age
		if bead.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, bead.CreatedAt); err == nil {
				row.Age = formatTimestamp(t)
			}
		}

		rows = append(rows, row)
	}

	// Sort by priority (1=critical first), then by age
	sort.Slice(rows, func(i, j int) bool {
		pi, pj := rows[i].Priority, rows[j].Priority
		if pi == 0 {
			pi = 5 // Treat unset priority as low
		}
		if pj == 0 {
			pj = 5
		}
		if pi != pj {
			return pi < pj
		}
		return rows[i].Age > rows[j].Age // Older first for same priority
	})

	return rows, nil
}

// isWorkPanelNonDispatchable reports whether an issue should be hidden from
// the Work panel: the same bookkeeping families the Ready lists hide, minus
// gt:keep and gt:standing-orders, which mark a bead protected from
// auto-close rather than internal — a bead wearing one can still be real
// work a polecat slings (gt-b9wq review).
func isWorkPanelNonDispatchable(issueType string, labels []string, title string) bool {
	for _, label := range labels {
		if label == "gt:keep" || label == "gt:standing-orders" {
			return false
		}
	}
	return beads.IsNonDispatchableBead(&beads.Issue{Type: issueType, Labels: labels, Title: title})
}

// FetchActivity returns recent activity from the event log.
func (f *LiveConvoyFetcher) FetchActivity() ([]ActivityRow, error) {
	eventsPath := filepath.Join(f.townRoot, ".events.jsonl")

	// Read events file
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		return nil, nil // No events file
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		return nil, nil
	}

	// Take last 50 events for richer timeline
	start := 0
	if len(lines) > 50 {
		start = len(lines) - 50
	}

	var rows []ActivityRow
	for i := len(lines) - 1; i >= start; i-- {
		line := lines[i]
		if line == "" {
			continue
		}

		var event struct {
			Timestamp  string                 `json:"ts"`
			Type       string                 `json:"type"`
			Actor      string                 `json:"actor"`
			Payload    map[string]interface{} `json:"payload"`
			Visibility string                 `json:"visibility"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		// Skip audit-only events
		if event.Visibility == "audit" {
			continue
		}

		row := ActivityRow{
			Type:         event.Type,
			Category:     eventCategory(event.Type),
			Actor:        formatAgentAddress(event.Actor),
			Rig:          extractRig(event.Actor),
			Icon:         eventIcon(event.Type),
			RawTimestamp: event.Timestamp,
		}

		// Calculate time ago
		if t, err := time.Parse(time.RFC3339, event.Timestamp); err == nil {
			row.Time = formatTimestamp(t)
		}

		// Generate human-readable summary
		row.Summary = eventSummary(event.Type, event.Actor, event.Payload)

		rows = append(rows, row)
	}

	return rows, nil
}

// eventCategory classifies an event type into a filter category.
func eventCategory(eventType string) string {
	switch eventType {
	case "spawn", "kill", "session_start", "session_end", "session_death", "mass_death", "nudge", "handoff":
		return "agent"
	case "sling", "hook", "unhook", "done", "merge_started", "merged", "merge_failed":
		return "work"
	case "mail", "escalation_sent", "escalation_acked", "escalation_closed":
		return "comms"
	case "boot", "halt", "patrol_started", "patrol_complete":
		return "system"
	default:
		return "system"
	}
}

// extractRig extracts the rig name from an actor address like "gastown/polecats/nux".
func extractRig(actor string) string {
	if actor == "" {
		return ""
	}
	parts := strings.SplitN(actor, "/", 2)
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

// eventIcon returns an emoji for an event type.
func eventIcon(eventType string) string {
	icons := map[string]string{
		"sling":             "🎯",
		"hook":              "🪝",
		"unhook":            "🔓",
		"done":              "✅",
		"mail":              "📬",
		"spawn":             "🦨",
		"kill":              "💀",
		"nudge":             "👉",
		"handoff":           "🤝",
		"session_start":     "▶️",
		"session_end":       "⏹️",
		"session_death":     "☠️",
		"mass_death":        "💥",
		"patrol_started":    "🔍",
		"patrol_complete":   "✔️",
		"escalation_sent":   "⚠️",
		"escalation_acked":  "👍",
		"escalation_closed": "🔕",
		"merge_started":     "🔀",
		"merged":            "✨",
		"merge_failed":      "❌",
		"boot":              "🚀",
		"halt":              "🛑",
	}
	if icon, ok := icons[eventType]; ok {
		return icon
	}
	return "📋"
}

// eventSummary generates a human-readable summary for an event.
func eventSummary(eventType, actor string, payload map[string]interface{}) string {
	shortActor := formatAgentAddress(actor)

	switch eventType {
	case "sling":
		bead, _ := payload["bead"].(string)
		target, _ := payload["target"].(string)
		return fmt.Sprintf("%s slung to %s", bead, formatAgentAddress(target))
	case "done":
		bead, _ := payload["bead"].(string)
		return fmt.Sprintf("%s completed %s", shortActor, bead)
	case "mail":
		to, _ := payload["to"].(string)
		subject, _ := payload["subject"].(string)
		if len(subject) > 25 {
			subject = subject[:22] + "..."
		}
		return fmt.Sprintf("→ %s: %s", formatAgentAddress(to), subject)
	case "spawn":
		return fmt.Sprintf("%s spawned", shortActor)
	case "kill":
		return fmt.Sprintf("%s killed", shortActor)
	case "hook":
		bead, _ := payload["bead"].(string)
		return fmt.Sprintf("%s hooked %s", shortActor, bead)
	case "unhook":
		bead, _ := payload["bead"].(string)
		return fmt.Sprintf("%s unhooked %s", shortActor, bead)
	case "merged":
		branch, _ := payload["branch"].(string)
		return fmt.Sprintf("merged %s", branch)
	case "merge_failed":
		reason, _ := payload["reason"].(string)
		if len(reason) > 30 {
			reason = reason[:27] + "..."
		}
		return fmt.Sprintf("merge failed: %s", reason)
	case "escalation_sent":
		return "escalation created"
	case "session_death":
		role, _ := payload["role"].(string)
		return fmt.Sprintf("%s session died", formatAgentAddress(role))
	case "mass_death":
		count, _ := payload["count"].(float64)
		return fmt.Sprintf("%.0f sessions died", count)
	default:
		return eventType
	}
}

// localServerTimeout bounds one poll of the model server: a page render waits
// on it, and a wedged server must not hold the page.
const localServerTimeout = 2 * time.Second

// poolAgentLister returns the GT_AGENT of every live polecat session. The
// production read goes to tmux; tests supply a fake.
type poolAgentLister func() ([]string, error)

// FetchLocalPool reports the pool's local seats and the state of the model
// server those seats are configured against. It returns nil when the town has
// no polecat_pool, which keeps the panel off the page; a failed seat or server
// read sets the matching *Err field so the panel says what is unreadable
// instead of reporting a zero.
func (f *LiveConvoyFetcher) FetchLocalPool() (*LocalPoolData, error) {
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(f.townRoot))
	if err != nil {
		return nil, fmt.Errorf("reading town settings: %w", err)
	}
	if ts == nil || ts.PolecatPool == nil {
		return nil, nil
	}
	pool := ts.PolecatPool

	data := &LocalPoolData{
		MaxLocal:      pool.MaxLocal,
		LocalAgent:    pool.LocalAgent,
		OverflowAgent: pool.OverflowAgent,
	}
	// A gap that does not parse is left out: rendered beside the real
	// settings it would read as the value in force.
	if gap := strings.TrimSpace(pool.MinSpawnGap); gap != "" {
		if _, err := time.ParseDuration(gap); err == nil {
			data.MinSpawnGap = gap
		}
	}

	agents, err := f.poolAgents()
	if err != nil {
		data.SeatsErr = "seats unreadable"
	} else {
		// An unset local_agent matches nothing: every polecat session carries
		// an empty GT_AGENT and would otherwise count as a local seat.
		for _, agent := range agents {
			if pool.LocalAgent != "" && agent == pool.LocalAgent {
				data.LocalSeats++
			}
		}
	}

	data.ServerEndpoint = localServerEndpoint(ts, pool.LocalAgent)
	probe, probeErr := f.modelServerStatus(data.ServerEndpoint)
	if data.ServerEndpoint == "" {
		// Nothing to probe: a pool whose local agent names no base URL must
		// not read as a server that is down.
		data.ServerErr = "no endpoint"
	} else if probeErr != nil {
		data.ServerErr = "down"
	} else {
		data.ServerModel = probe.model
		data.ServerKind = probe.kind
		data.ServerInFlight = probe.inFlight
		data.ServerMaxFlight = probe.maxInFlight
		data.ServerInFlightKnown = probe.inFlightKnown
	}
	return data, nil
}

// localServerEndpoint is the base URL the pool's local agent is configured
// against: ANTHROPIC_BASE_URL from the alias's env in the town settings. Empty
// when that alias sets none, which is what keeps the panel from probing an
// address nobody configured (gt-w0x3).
func localServerEndpoint(ts *config.TownSettings, alias string) string {
	if alias == "" || ts.Agents == nil {
		return ""
	}
	agent, ok := ts.Agents[alias]
	if !ok || agent == nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(agent.Env["ANTHROPIC_BASE_URL"]), "/")
}

// modelServerProbe is what a model server says about itself when asked.
type modelServerProbe struct {
	// model and kind come from GET /v1/models: the id it serves and the owner
	// it reports. kind is the server's own word, never a name we chose.
	model string
	kind  string
	// inFlight and maxInFlight come from GET /health when the server serves
	// one; inFlightKnown is false when it does not, so an unreported count
	// never renders as zero requests.
	inFlight      int
	maxInFlight   int
	inFlightKnown bool
}

// modelServerStatus probes the endpoint the local pool is configured against.
// Up is decided by GET /v1/models — every OpenAI-compatible server answers it,
// and it names the model; /health is read second for the in-flight count and
// is not required. Failures go through the breaker so a down server costs one
// request per backoff window rather than one per render.
func (f *LiveConvoyFetcher) modelServerStatus(baseURL string) (modelServerProbe, error) {
	var probe modelServerProbe
	if baseURL == "" {
		return probe, errors.New("no endpoint configured for the pool's local agent")
	}
	if !f.localServerBreaker.allow() {
		return probe, errors.New("backing off after a failed poll")
	}

	ctx, cancel := context.WithTimeout(context.Background(), localServerTimeout)
	defer cancel()
	client := &http.Client{Timeout: localServerTimeout}

	body, err := getJSON(ctx, client, baseURL+"/v1/models")
	if err != nil {
		f.localServerBreaker.recordFailure()
		return probe, err
	}
	var models struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &models); err != nil {
		f.localServerBreaker.recordFailure()
		return probe, err
	}
	f.localServerBreaker.recordSuccess()
	if len(models.Data) > 0 {
		probe.model = models.Data[0].ID
		probe.kind = models.Data[0].OwnedBy
	}

	// Best effort: a server without /health still reports up and model.
	if body, err := getJSON(ctx, client, baseURL+"/health"); err == nil {
		var health struct {
			Model          string `json:"model"`
			ActiveRequests *int   `json:"active_requests"`
			MaxActive      *int   `json:"max_active_requests"`
		}
		if json.Unmarshal(body, &health) == nil {
			if probe.model == "" {
				probe.model = health.Model
			}
			if health.ActiveRequests != nil {
				probe.inFlight = *health.ActiveRequests
				probe.inFlightKnown = true
			}
			if health.MaxActive != nil {
				probe.maxInFlight = *health.MaxActive
			}
		}
	}
	return probe, nil
}

// getJSON fetches url and returns its body, or an error for anything that is
// not a 200.
func getJSON(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// poolAgents returns the GT_AGENT of every live polecat session, or the
// lister's error. A polecat is a session whose GT_ROLE is
// "<rig>/polecats/<name>", which keeps witnesses, refineries and dogs on the
// same socket out of the seat count; GT_AGENT is written at spawn
// (SessionStartOptions.Agent / AgentEnv fallback). A polecat session without
// the variable reports an empty agent and so never counts as a local seat.
func (f *LiveConvoyFetcher) poolAgents() ([]string, error) {
	if f.listPoolAgents != nil {
		return f.listPoolAgents()
	}
	names, err := f.tmuxSessionNames()
	if err != nil {
		return nil, err
	}
	var agents []string
	for _, name := range names {
		role, err := f.tmuxSessionEnv(name, "GT_ROLE")
		if err != nil || !strings.Contains(role, "/polecats/") {
			continue
		}
		agent, _ := f.tmuxSessionEnv(name, "GT_AGENT")
		agents = append(agents, agent)
	}
	return agents, nil
}

// tmuxSessionNames lists the town socket's session names.
func (f *LiveConvoyFetcher) tmuxSessionNames() ([]string, error) {
	out, err := f.runTmuxCmd("list-sessions", "-F", "#{session_name}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range strings.Split(out.String(), "\n") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// tmuxSessionEnv reads one variable from a session's environment. tmux prints
// "KEY=value" (psmux prints every variable), so the line is split on the first
// "=" and the key compared; a variable the session does not carry is an error,
// which callers read as absent.
func (f *LiveConvoyFetcher) tmuxSessionEnv(session, key string) (string, error) {
	out, err := f.runTmuxCmd("show-environment", "-t", session, key)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if name, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok && name == key {
			return value, nil
		}
	}
	return "", fmt.Errorf("tmux: %s carries no %s", session, key)
}
