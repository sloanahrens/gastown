package convoy

import (
	"context"
	"github.com/steveyegge/gastown/internal/beads"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreResolver_ResolveIssues_SingleStore(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	issue := &beads.Issue{
		ID:        "hq-test1",
		Title:     "Test Issue",
		Status:    "open",
		Priority:  2,
		Type:      "task",
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	if err := store.CreateIssue(ctx, issue, "test"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	// Create a town root with routes pointing hq- to "."
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	os.MkdirAll(beadsDir, 0755)
	beads.WriteRoutes(beadsDir, []beads.Route{
		{Prefix: "hq-", Path: "."},
	})

	resolver := NewStoreResolver(townRoot, map[string]IssueSource{
		"hq": store,
	})

	result := resolver.ResolveIssues(ctx, []string{"hq-test1"})
	if len(result) != 1 {
		t.Fatalf("ResolveIssues returned %d issues, want 1", len(result))
	}
	if result["hq-test1"] == nil {
		t.Fatal("hq-test1 not found in result")
	}
	if string(result["hq-test1"].Status) != "open" {
		t.Errorf("status = %s, want open", result["hq-test1"].Status)
	}
}

func TestStoreResolver_ResolveIssues_CrossStore(t *testing.T) {
	t.Parallel()
	hqStore, hqCleanup := setupTestStore(t)
	defer hqCleanup()
	dsStore, dsCleanup := setupTestStore(t)
	defer dsCleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Create issue in "ds" store only
	dsIssue := &beads.Issue{
		ID:        "ds-abc",
		Title:     "Dashboard Issue",
		Status:    "closed",
		Priority:  1,
		Type:      "task",
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	if err := dsStore.CreateIssue(ctx, dsIssue, "test"); err != nil {
		t.Fatalf("CreateIssue ds: %v", err)
	}

	// Create issue in "hq" store
	hqIssue := &beads.Issue{
		ID:        "hq-xyz",
		Title:     "HQ Issue",
		Status:    "open",
		Priority:  2,
		Type:      "task",
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	if err := hqStore.CreateIssue(ctx, hqIssue, "test"); err != nil {
		t.Fatalf("CreateIssue hq: %v", err)
	}

	// Set up routes
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	os.MkdirAll(beadsDir, 0755)
	beads.WriteRoutes(beadsDir, []beads.Route{
		{Prefix: "hq-", Path: "."},
		{Prefix: "ds-", Path: "dashboard"},
	})

	resolver := NewStoreResolver(townRoot, map[string]IssueSource{
		"hq":        hqStore,
		"dashboard": dsStore,
	})

	// Resolve both cross-store IDs
	result := resolver.ResolveIssues(ctx, []string{"ds-abc", "hq-xyz"})
	if len(result) != 2 {
		t.Fatalf("ResolveIssues returned %d issues, want 2", len(result))
	}
	if result["ds-abc"] == nil {
		t.Fatal("ds-abc not found in result")
	}
	if string(result["ds-abc"].Status) != "closed" {
		t.Errorf("ds-abc status = %s, want closed", result["ds-abc"].Status)
	}
	if result["hq-xyz"] == nil {
		t.Fatal("hq-xyz not found in result")
	}
	if string(result["hq-xyz"].Status) != "open" {
		t.Errorf("hq-xyz status = %s, want open", result["hq-xyz"].Status)
	}
}

func TestStoreResolver_NilStores(t *testing.T) {
	t.Parallel()
	resolver := NewStoreResolver("/nonexistent", nil)
	result := resolver.ResolveIssues(context.Background(), []string{"ds-abc"})
	if len(result) != 0 {
		t.Errorf("expected empty result for nil stores, got %d", len(result))
	}
}

func TestStoreResolver_EmptyIDs(t *testing.T) {
	t.Parallel()
	resolver := NewStoreResolver("/nonexistent", map[string]IssueSource{})
	result := resolver.ResolveIssues(context.Background(), nil)
	if len(result) != 0 {
		t.Errorf("expected empty result for nil IDs, got %d", len(result))
	}
}

func TestStoreResolver_StoreForID_ExternalFormat(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	os.MkdirAll(beadsDir, 0755)
	beads.WriteRoutes(beadsDir, []beads.Route{
		{Prefix: "ds-", Path: "dashboard"},
	})

	resolver := NewStoreResolver(townRoot, nil)
	storeName := resolver.storeForID("external:ds:ds-abc")
	if storeName != "dashboard" {
		t.Errorf("storeForID(external:ds:ds-abc) = %q, want %q", storeName, "dashboard")
	}
}

// TestStoreResolver_OwningStoreOrGap pins gt-2ppfg at the resolver: a nil
// store means two different things, and only one of them is "the caller's own
// store owns this bead". A rig the resolver cannot produce is a gap; hq, an
// unroutable id, and a resolver held by nobody are not, so the caller reads
// its own store as it always has.
func TestStoreResolver_OwningStoreOrGap(t *testing.T) {
	t.Parallel()
	townRoot := setupTownRoot(t)
	townStore := newFakeStore()
	rigStore := newFakeStore()

	tests := []struct {
		name      string
		withHQ    bool
		withRig   bool
		nilResolv bool
		issue     string
		wantStore IssueSource
		wantGap   bool
	}{
		{name: "no resolver", nilResolv: true, issue: "test-rigbead"},
		{name: "rig store missing", withHQ: true, issue: "test-rigbead", wantGap: true},
		{name: "rig store open", withHQ: true, withRig: true, issue: "test-rigbead", wantStore: rigStore},
		{name: "prefix routes nowhere", withHQ: true, issue: "noroute"},
		{name: "hq store held", withHQ: true, issue: "hq-abc", wantStore: townStore},
		{name: "hq store absent", issue: "hq-abc"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stores := map[string]IssueSource{}
			if tc.withHQ {
				stores["hq"] = townStore
			}
			if tc.withRig {
				stores["testrig"] = rigStore
			}
			var resolver *StoreResolver
			if !tc.nilResolv {
				resolver = NewStoreResolver(townRoot, stores)
			}

			store, gap := resolver.owningStoreOrGap(tc.issue)
			if gap != tc.wantGap {
				t.Errorf("owningStoreOrGap(%q) gap = %v, want %v", tc.issue, gap, tc.wantGap)
			}
			if store != tc.wantStore {
				t.Errorf("owningStoreOrGap(%q) store = %v, want %v", tc.issue, store, tc.wantStore)
			}
		})
	}
}
