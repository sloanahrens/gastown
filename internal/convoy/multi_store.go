// Package convoy — multi-store resolution for cross-database convoy tracking.
package convoy

import (
	"context"
	"strings"
	"sync"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
)

// StoreResolver resolves beads issues across multiple stores using prefix-based
// routing. In multi-rig Gas Town setups, each rig has its own Dolt database.
// Convoys live in the HQ store but may track issues in rig stores (e.g., ds-*
// in dashboard). Without cross-store resolution, convoy tracking sees 0/0 for
// cross-database dependencies. See GH #2624.
type StoreResolver struct {
	// stores maps store names ("hq", "dashboard", etc.) to beads stores.
	stores map[string]beadsdk.Storage

	// townRoot is the path to the town root, used for prefix → rig name lookup.
	townRoot string

	// open, when set, opens a store the map does not hold yet and is remembered
	// in opened. A caller that holds only the town store — gt close, which runs
	// once per bead — reaches a rig's beads this way without paying for every
	// rig's connection on each invocation (gt-tq6l).
	open func(name string) (beadsdk.Storage, error)

	// opened names the stores open produced, so Close can release them.
	opened map[string]beadsdk.Storage

	mu sync.Mutex
}

// NewStoreResolver creates a resolver from the daemon's store map.
// If stores is nil or empty, all resolution methods fall through gracefully.
func NewStoreResolver(townRoot string, stores map[string]beadsdk.Storage) *StoreResolver {
	return &StoreResolver{
		stores:   stores,
		townRoot: townRoot,
	}
}

// NewOpeningStoreResolver creates a resolver that opens a name's store the
// first time an issue routing to it is resolved; open receives a store name
// ("hq" or a rig name) and its stores are released by Close (gt-tq6l).
func NewOpeningStoreResolver(townRoot string, open func(name string) (beadsdk.Storage, error)) *StoreResolver {
	return &StoreResolver{
		stores:   make(map[string]beadsdk.Storage),
		opened:   make(map[string]beadsdk.Storage),
		townRoot: townRoot,
		open:     open,
	}
}

// Close releases the stores this resolver opened on demand, leaving a store
// the caller handed in at construction open.
func (r *StoreResolver) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var first error
	for name, store := range r.opened {
		if err := store.Close(); err != nil && first == nil {
			first = err
		}
		delete(r.stores, name)
	}
	r.opened = make(map[string]beadsdk.Storage)
	return first
}

// storeNamed returns the store named name, or nil.
func (r *StoreResolver) storeNamed(name string) beadsdk.Storage {
	if r == nil || name == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stores[name]
}

// owningStore returns the store holding id, opening it on demand when this
// resolver was built to, or nil when no store for id is reachable.
func (r *StoreResolver) owningStore(id string) beadsdk.Storage {
	name := r.storeForID(id)
	if name == "" {
		return nil
	}
	if store := r.storeNamed(name); store != nil {
		return store
	}
	if r.open == nil || name == "hq" {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-checked under the lock: a concurrent resolution may have opened it.
	if store := r.stores[name]; store != nil {
		return store
	}
	store, err := r.open(name)
	if err != nil || store == nil {
		// Not remembered: the next resolution retries, so a rig store that
		// opens late is picked up without a restart.
		return nil
	}
	r.stores[name] = store
	r.opened[name] = store
	return store
}

// ResolveIssues fetches fresh issue data for the given IDs, looking up each
// issue in the appropriate store based on its prefix. Issues found in any store
// are returned in the result map. Issues not found in any store are omitted.
func (r *StoreResolver) ResolveIssues(ctx context.Context, ids []string) map[string]*beadsdk.Issue {
	result := make(map[string]*beadsdk.Issue, len(ids))
	if len(ids) == 0 {
		return result
	}

	// Group IDs by target store name via prefix → rig name routing.
	// IDs whose prefix maps to HQ (empty rig name) use the "hq" store.
	byStore := make(map[string][]string)
	for _, id := range ids {
		storeName := r.storeForID(id)
		if storeName != "" {
			byStore[storeName] = append(byStore[storeName], id)
		}
	}

	for storeName, storeIDs := range byStore {
		store := r.storeNamed(storeName)
		if store == nil {
			continue
		}

		issues, err := store.GetIssuesByIDs(ctx, storeIDs)
		if err != nil {
			continue
		}
		for _, iss := range issues {
			if iss != nil {
				result[iss.ID] = iss
			}
		}
	}

	return result
}

// ResolveDepsWithMetadata fetches dependency metadata for an issue, trying
// the appropriate store for that issue's prefix. Returns nil on any error.
func (r *StoreResolver) ResolveDepsWithMetadata(ctx context.Context, issueID string) []*beadsdk.IssueWithDependencyMetadata {
	store := r.storeNamed(r.storeForID(issueID))
	if store == nil {
		return nil
	}

	deps, err := store.GetDependenciesWithMetadata(ctx, issueID)
	if err != nil {
		return nil
	}
	return deps
}

// storeForID returns the store name for a given issue ID based on prefix routing.
// Returns "hq" for town-level prefixes, rig name for rig prefixes, or "" if unknown.
func (r *StoreResolver) storeForID(id string) string {
	// Strip external: wrapper if present
	if strings.HasPrefix(id, "external:") {
		parts := strings.SplitN(id, ":", 3)
		if len(parts) == 3 {
			id = parts[2]
		}
	}

	prefix := beads.ExtractPrefix(id)
	if prefix == "" {
		return ""
	}

	rigName := beads.GetRigNameForPrefix(r.townRoot, prefix)
	if rigName == "" {
		// Town-level prefix (e.g., "hq-") or unknown → use hq store
		return "hq"
	}
	return rigName
}
