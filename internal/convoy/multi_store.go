// Package convoy — multi-store resolution for cross-database convoy tracking.
package convoy

import (
	"context"
	"fmt"
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
	// in opened. A caller that holds only the town store — the stranded check,
	// which opens the town store and then resolves tracked issues across rigs —
	// reaches a rig's beads this way without paying for every rig's connection
	// on each invocation (gt-tq6l).
	open func(name string) (beadsdk.Storage, error)

	// opened names the stores open produced, so Close can release them.
	opened map[string]beadsdk.Storage

	// failed remembers a store open could not produce, with its error, for
	// the life of this resolver: a rig that is down is tried once per gt
	// close, not once per lookup.
	failed map[string]error

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
		failed:   make(map[string]error),
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
	r.failed = make(map[string]error)
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
	store, _ := r.owningStoreOrGap(id)
	return store
}

// owningStoreOrGap returns the store holding id, opening it on demand when
// this resolver was built to, and reports whether the route named a rig store
// it could not produce. A caller holding only the town store reads a nil as
// "the town store owns it", which for a rig bead answers "no record" about a
// bead whose record is in the rig; the gap is the fact that caller acts on
// instead (gt-2ppfg). hq is never a gap — the caller's own store holds its
// record — and neither is an id whose prefix routes nowhere.
func (r *StoreResolver) owningStoreOrGap(id string) (beadsdk.Storage, bool) {
	if r == nil {
		return nil, false
	}
	name := r.storeForID(id)
	if name == "" {
		return nil, false
	}
	store, _ := r.storeByName(name)
	return store, store == nil && name != "hq"
}

// storeByName returns the store named name ("hq" or a rig name), opening it
// on demand when this resolver was built to, or the reason it has none. hq is
// never opened here: a caller holding a resolver without hq holds the town
// store itself.
func (r *StoreResolver) storeByName(name string) (beadsdk.Storage, error) {
	if r == nil {
		return nil, fmt.Errorf("no store resolver")
	}
	if name == "" {
		return nil, fmt.Errorf("no route for store")
	}
	if store := r.storeNamed(name); store != nil {
		return store, nil
	}
	if r.open == nil || name == "hq" {
		return nil, fmt.Errorf("no open store for %s", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-checked under the lock: a concurrent resolution may have opened it.
	if store := r.stores[name]; store != nil {
		return store, nil
	}
	if err := r.failed[name]; err != nil {
		return nil, err
	}
	store, err := r.open(name)
	if err == nil && store == nil {
		err = fmt.Errorf("no store for %s", name)
	}
	if err != nil {
		err = fmt.Errorf("opening store for %s: %w", name, err)
		r.failed[name] = err
		return nil, err
	}
	r.stores[name] = store
	r.opened[name] = store
	return store, nil
}

// ResolveIssues fetches fresh issue data for the given IDs, looking up each
// issue in the appropriate store based on its prefix, opening that store when
// the resolver was built to. Issues found in any store are returned in the
// result map. Issues not found in any store are omitted.
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

	for _, storeIDs := range byStore {
		store := r.owningStore(storeIDs[0])
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
