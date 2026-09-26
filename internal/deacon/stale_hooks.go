// Package deacon provides the Deacon agent infrastructure.
package deacon

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// StaleHookConfig holds configurable parameters for stale hook detection.
type StaleHookConfig struct {
	// MaxAge is how long a bead can be hooked before being considered stale.
	MaxAge time.Duration `json:"max_age"`
	// DryRun if true, only reports what would be done without making changes.
	DryRun bool `json:"dry_run"`
}

// DefaultStaleHookConfig returns the default stale hook config.
func DefaultStaleHookConfig() *StaleHookConfig {
	return &StaleHookConfig{
		MaxAge: 1 * time.Hour,
		DryRun: false,
	}
}

// StaleHookResult represents the result of processing a stale hooked bead.
type StaleHookResult struct {
	BeadID     string `json:"bead_id"`
	Title      string `json:"title"`
	Assignee   string `json:"assignee"`
	Store      string `json:"store,omitempty"` // bead store the bead was found in ("town", or the rig name)
	Age        string `json:"age"`
	AgentAlive bool   `json:"agent_alive"`
	Unhooked   bool   `json:"unhooked"`
	Error      string `json:"error,omitempty"`
	// PartialWork indicates uncommitted changes or unpushed commits were found
	// in the agent's worktree before unhooking.
	PartialWork   bool   `json:"partial_work,omitempty"`
	WorktreeDirty bool   `json:"worktree_dirty,omitempty"`
	UnpushedCount int    `json:"unpushed_count,omitempty"`
	WorktreeError string `json:"worktree_error,omitempty"`
}

// StaleHookScanResult contains the full results of a stale hook scan.
type StaleHookScanResult struct {
	ScannedAt   time.Time          `json:"scanned_at"`
	TotalHooked int                `json:"total_hooked"`
	StaleCount  int                `json:"stale_count"`
	Unhooked    int                `json:"unhooked"`
	Results     []*StaleHookResult `json:"results"`
	// StoresSearched labels every bead store the scan queried, so a zero result
	// cannot be read as "nothing is wedged anywhere" when the scan simply
	// didn't look where the bead lives.
	StoresSearched []string `json:"stores_searched,omitempty"`
	// StoreErrors records stores the scan could not query, keyed by store name.
	StoreErrors map[string]string `json:"store_errors,omitempty"`
}

// hookStore is one bead database a stale-hook scan searches.
//
// Rig-prefixed beads live in their rig's database, not the town's, and `bd
// list` does not route by prefix. A scan that queries only the town database
// reports "no hooked beads found" while the same `bd list --status=hooked` run
// from the rig returns one (gt-hdph) — the wedge stale-hooks exists to clear,
// hidden by the command meant to find it.
type hookStore struct {
	// Name labels the store in output: "town" for hq, otherwise the rig name.
	Name string
	// BeadsDir is the store's resolved .beads directory.
	BeadsDir string
}

func (s hookStore) String() string {
	return fmt.Sprintf("%s (%s)", s.Name, s.BeadsDir)
}

// hookedBeadRef pairs a hooked bead with the store it was found in, so the
// reset can be aimed at the database that actually holds it.
type hookedBeadRef struct {
	bead  *beads.Issue
	store hookStore
}

// hookStoreScan is the outcome of querying every store for hooked beads.
type hookStoreScan struct {
	// Beads are the hooked beads found, deduped by ID and tagged with their store.
	Beads []hookedBeadRef
	// Searched lists every store queried, in output order, including those that failed.
	Searched []hookStore
	// Errors records each store whose query failed, keyed by store name.
	Errors map[string]string
}

// storeLabels renders the searched stores for scan output.
func (s *hookStoreScan) storeLabels() []string {
	labels := make([]string, 0, len(s.Searched))
	for _, store := range s.Searched {
		labels = append(labels, store.String())
	}
	return labels
}

// errSummary renders all store query failures for the total-failure error.
func (s *hookStoreScan) errSummary() string {
	names := make([]string, 0, len(s.Errors))
	for name := range s.Errors {
		names = append(names, name)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+": "+s.Errors[name])
	}
	return strings.Join(parts, "; ")
}

// listStoreHooks queries one store for hooked beads. Indirected so tests can
// exercise the scan without a Dolt server.
var listStoreHooks = listHookedBeadsInStore

// unhookStoreBead resets a hooked bead to open in the store holding it.
// Indirected so tests can assert the target store without writing to it.
var unhookStoreBead = unhookBeadInStore

// ScanStaleHooks finds hooked beads with dead agents and optionally unhooks them.
// Session liveness is checked for ALL hooked beads regardless of age (gt-pqf9x).
// Every bead store in the town is searched — the town store plus each routed rig
// store (gt-hdph) — since rig beads are invisible to a town-only query.
// A hooked bead is considered stale if:
//  1. The assignee's tmux session is dead (immediate unhook), OR
//  2. The bead is older than MaxAge AND we can't determine session liveness
//     (e.g., unknown assignee format)
func ScanStaleHooks(townRoot string, cfg *StaleHookConfig) (*StaleHookScanResult, error) {
	if cfg == nil {
		cfg = DefaultStaleHookConfig()
	}

	scan, err := scanHookedStores(townRoot)
	if err != nil {
		return nil, err
	}
	if len(scan.Searched) == 0 {
		return nil, fmt.Errorf("no bead stores found under %s", townRoot)
	}
	if len(scan.Errors) == len(scan.Searched) {
		return nil, fmt.Errorf("queried none of %d bead store(s) successfully: %s",
			len(scan.Searched), scan.errSummary())
	}

	result := &StaleHookScanResult{
		ScannedAt:      time.Now().UTC(),
		TotalHooked:    len(scan.Beads),
		StoresSearched: scan.storeLabels(),
		Results:        make([]*StaleHookResult, 0),
	}
	if len(scan.Errors) > 0 {
		result.StoreErrors = scan.Errors
	}

	threshold := time.Now().Add(-cfg.MaxAge)
	t := tmux.NewTmux()

	for _, ref := range scan.Beads {
		bead := ref.bead
		updatedAt, ageKnown := parseBeadTime(bead.UpdatedAt)

		hookResult := &StaleHookResult{
			BeadID:   bead.ID,
			Title:    bead.Title,
			Assignee: bead.Assignee,
			Store:    ref.store.Name,
			Age:      formatBeadAge(updatedAt, ageKnown),
		}

		// Check if assignee agent is still alive (regardless of age)
		sessionChecked := false
		if bead.Assignee != "" {
			sessionName := assigneeToSessionName(bead.Assignee)
			if sessionName != "" {
				alive, _ := t.HasSession(sessionName)
				hookResult.AgentAlive = alive
				sessionChecked = true
			}
		}

		// Determine if this hook is stale:
		// - Agent confirmed dead → stale (regardless of age)
		// - Can't check session + older than MaxAge → stale (fallback)
		// - Agent alive → not stale
		isStale := false
		if sessionChecked && !hookResult.AgentAlive {
			// Session confirmed dead — unhook immediately regardless of age
			isStale = true
		} else if !sessionChecked && ageKnown && updatedAt.Before(threshold) {
			// Can't determine session liveness (unknown assignee format).
			// Fall back to age — only when bd's timestamp parsed, so an
			// unreadable timestamp cannot pass for "infinitely old".
			isStale = true
		}

		if !isStale {
			continue
		}

		result.StaleCount++

		// If agent is dead/gone, check worktree state before unhooking
		if !hookResult.AgentAlive {
			checkWorktreeState(townRoot, bead.Assignee, hookResult)

			if !cfg.DryRun {
				if err := unhookStoreBead(ref.store, bead.ID); err != nil {
					hookResult.Error = err.Error()
				} else {
					hookResult.Unhooked = true
					result.Unhooked++
				}
			}
		}

		result.Results = append(result.Results, hookResult)
	}

	return result, nil
}

// discoverHookStores returns every store a stale-hook scan must search: the
// town (hq) store plus each rig named in the town's routes.jsonl. Missing
// directories are skipped — a store that isn't there holds no hooked bead —
// and routes resolving to the same directory (hq- and hq-cv- both point at
// ".") collapse into one store.
func discoverHookStores(townRoot string) ([]hookStore, error) {
	var stores []hookStore
	seen := make(map[string]bool)
	add := func(name, beadsDir string) {
		if name == "" || beadsDir == "" || seen[beadsDir] {
			return
		}
		if info, err := os.Stat(beadsDir); err != nil || !info.IsDir() {
			return
		}
		seen[beadsDir] = true
		stores = append(stores, hookStore{Name: name, BeadsDir: beadsDir})
	}

	add("town", beads.ResolveBeadsDir(townRoot))

	routes, err := beads.LoadRoutes(townBeadsDir(townRoot))
	if err != nil {
		// Return what we have, but say so: a scan that quietly searched only
		// the town store is the blindness this function exists to remove.
		return stores, fmt.Errorf("loading routes for rig stores: %w", err)
	}
	for _, route := range routes {
		if route.Path == "." {
			continue
		}
		rigDir := route.Path
		if !filepath.IsAbs(rigDir) {
			rigDir = filepath.Join(townRoot, route.Path)
		}
		add(rigNameFromRoutePath(townRoot, route.Path), beads.ResolveBeadsDir(rigDir))
	}

	return stores, nil
}

// rigNameFromRoutePath names the store for a routes.jsonl path: the first path
// component under the town root ("gastown" for "gastown/mayor/rig").
func rigNameFromRoutePath(townRoot, routePath string) string {
	path := routePath
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(townRoot, path)
		if err != nil {
			return filepath.Base(path)
		}
		path = rel
	}

	parts := strings.Split(path, string(filepath.Separator))
	if len(parts) == 0 || parts[0] == "" || parts[0] == "." || parts[0] == ".." {
		return ""
	}
	return parts[0]
}

// scanHookedStores queries every bead store in the town for hooked beads.
// A store that fails to answer is recorded and skipped rather than aborting
// the scan: one unreachable rig database must not hide every other store's
// wedged beads.
func scanHookedStores(townRoot string) (*hookStoreScan, error) {
	stores, err := discoverHookStores(townRoot)
	if err != nil {
		return nil, err
	}

	scan := &hookStoreScan{Searched: stores, Errors: make(map[string]string)}
	seen := make(map[string]bool)

	for _, store := range stores {
		hooked, err := listStoreHooks(store)
		if err != nil {
			scan.Errors[store.Name] = err.Error()
			continue
		}
		for _, bead := range hooked {
			if bead == nil || bead.ID == "" || seen[bead.ID] {
				continue
			}
			seen[bead.ID] = true
			scan.Beads = append(scan.Beads, hookedBeadRef{bead: bead, store: store})
		}
	}

	return scan, nil
}

// listHookedBeadsInStore lists status=hooked beads from one store, its
// database pinned so rig beads are visible from any caller's working directory.
func listHookedBeadsInStore(store hookStore) ([]*beads.Issue, error) {
	hooked, err := beads.NewRigLocal(store.BeadsDir).List(beads.ListOptions{
		Status:   beads.StatusHooked,
		Priority: -1,
		Limit:    0,
	})
	if err != nil {
		return nil, fmt.Errorf("listing hooked beads: %w", err)
	}
	return hooked, nil
}

// unhookBeadInStore sets a bead's status back to 'open' in the store that holds
// it. bd update does not route by prefix (see beads.ResolveHookDir), so the
// target database must be pinned — otherwise a rig bead's reset lands in the
// town database and the wedge survives the unhook. The mutation env is pinned
// too, so the write commits rather than stranding in a daemon-context session
// (see beads.BuildMutationPinnedBDEnv).
func unhookBeadInStore(store hookStore, beadID string) error {
	cmd := beads.Command(filepath.Dir(store.BeadsDir), store.BeadsDir, beads.MutationPinned,
		"update", beadID, "--status=open")
	return cmd.Run()
}

// parseBeadTime parses a bd timestamp via the shared beads.ParseIssueTime, so
// this scan agrees with the rest of the town about what a bead's timestamp
// means. The second return is false when the value is empty or unparseable
// (the zero time); callers treat that as "age unknown" rather than
// "infinitely old", so a timestamp format change cannot silently unhook live
// work.
func parseBeadTime(value string) (time.Time, bool) {
	parsed := beads.ParseIssueTime(value)
	return parsed, !parsed.IsZero()
}

// formatBeadAge renders a bead's age for scan output.
func formatBeadAge(updatedAt time.Time, known bool) string {
	if !known {
		return "unknown"
	}
	return time.Since(updatedAt).Round(time.Minute).String()
}

// assigneeToSessionName converts an assignee address to a tmux session name.
// Delegates to session.ParseAddress for consistent parsing across the codebase.
func assigneeToSessionName(assignee string) string {
	identity, err := session.ParseAddress(assignee)
	if err != nil {
		return ""
	}
	return identity.SessionName()
}

// checkWorktreeState checks an agent's worktree for uncommitted changes or
// unpushed commits and populates the result fields. This is best-effort;
// errors are recorded but do not prevent unhooking.
func checkWorktreeState(townRoot, assignee string, result *StaleHookResult) {
	worktreePath := assigneeToWorktreePath(townRoot, assignee)
	if worktreePath == "" {
		return
	}

	g := git.NewGit(worktreePath)
	workStatus, err := g.CheckUncommittedWork()
	if err != nil {
		result.WorktreeError = fmt.Sprintf("checking worktree: %v", err)
		return
	}

	if !workStatus.CleanExcludingBeads() {
		result.PartialWork = true
		result.WorktreeDirty = workStatus.HasUncommittedChanges
		result.UnpushedCount = workStatus.UnpushedCommits
	}
}

// AssigneeWorktreePath resolves an assignee address (e.g. "rig/polecats/name")
// to its git worktree path, for callers outside this package that need the
// same resolution the stale-hook scan uses. Returns "" if the assignee format
// is unrecognized or no worktree exists there.
func AssigneeWorktreePath(townRoot, assignee string) string {
	return assigneeToWorktreePath(townRoot, assignee)
}

// isUnsafePathComponent reports whether s cannot safely be joined into a
// filesystem path as a single component: empty, or a "." / ".." traversal
// segment. Assignees are system-written, but this is exported and now drives
// a git push (AssigneeWorktreePath's dead-holder caller), so a rig or agent
// name like ".." must not resolve outside the town root.
func isUnsafePathComponent(s string) bool {
	return s == "" || s == "." || s == ".."
}

// assigneeToWorktreePath resolves an assignee address to its git worktree path.
// Returns "" if the assignee format is unrecognized or the worktree doesn't exist.
// Supports polecat format "rig/polecats/name" and crew format "rig/crew/name".
func assigneeToWorktreePath(townRoot, assignee string) string {
	parts := strings.Split(assignee, "/")
	if len(parts) != 3 {
		return ""
	}

	rigName, agentType, name := parts[0], parts[1], parts[2]
	if agentType != "polecats" && agentType != "crew" {
		return ""
	}
	if isUnsafePathComponent(rigName) || isUnsafePathComponent(name) {
		return ""
	}

	rigPath := filepath.Join(townRoot, rigName)

	// New structure: rig/polecats/<name>/<rigname>/
	newPath := filepath.Join(rigPath, agentType, name, rigName)
	if info, err := os.Stat(newPath); err == nil && info.IsDir() {
		if _, err := os.Stat(filepath.Join(newPath, ".git")); err == nil {
			return newPath
		}
	}

	// Old structure: rig/polecats/<name>/
	oldPath := filepath.Join(rigPath, agentType, name)
	if info, err := os.Stat(oldPath); err == nil && info.IsDir() {
		if _, err := os.Stat(filepath.Join(oldPath, ".git")); err == nil {
			return oldPath
		}
	}

	return ""
}
