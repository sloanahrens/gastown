package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// followupTown writes a town whose rigs registry names rigs, so the store set
// and its order come from the reader rather than the fixture.
func followupTown(t *testing.T, rigs ...string) string {
	t.Helper()
	town := t.TempDir()
	cfg := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	for _, rig := range rigs {
		cfg.Rigs[rig] = config.RigEntry{GitURL: "test://" + rig}
	}
	path := filepath.Join(town, "mayor", "rigs.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveRigsConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	return town
}

// The town holds the hq store and one store per rig; each is listed exactly
// once, in store order, and the answers are joined in that order.
func TestFollowupAcrossStores(t *testing.T) {
	t.Parallel()
	town := followupTown(t, "beta", "alpha")
	var calls []string
	fake := func(store string) ([]*beads.Issue, error) {
		calls = append(calls, store)
		return []*beads.Issue{{ID: "gt-" + store}}, nil
	}
	got, err := dashFollowupIssues(town, fake)
	if err != nil {
		t.Fatalf("dashFollowupIssues: %v", err)
	}
	if want := []string{"hq", "alpha", "beta"}; !slices.Equal(calls, want) {
		t.Fatalf("listed stores = %v, want %v (one list each, in store order)", calls, want)
	}
	ids := make([]string, 0, len(got))
	for _, issue := range got {
		ids = append(ids, issue.ID)
	}
	if want := []string{"gt-hq", "gt-alpha", "gt-beta"}; !slices.Equal(ids, want) {
		t.Errorf("joined issues = %v, want %v", ids, want)
	}
}

// One store's list failing fails the whole read, wherever it sits in the order:
// a partial answer would read as that store having filed no follow-ups.
func TestDashFollowupIssuesFailClosed(t *testing.T) {
	t.Parallel()
	town := followupTown(t, "alpha", "beta")
	fail := errors.New("dolt down")
	for _, failing := range []string{"hq", "alpha", "beta"} {
		t.Run(failing, func(t *testing.T) {
			t.Parallel()
			lister := func(store string) ([]*beads.Issue, error) {
				if store == failing {
					return nil, fail
				}
				return []*beads.Issue{{ID: "gt-" + store}}, nil
			}
			got, err := dashFollowupIssues(town, lister)
			if !errors.Is(err, fail) {
				t.Fatalf("error = %v, want %v", err, fail)
			}
			if got != nil {
				t.Errorf("issues = %v, want none: a partial answer would shrink the counts", got)
			}
		})
	}
}

// A registry that cannot be read is an error, not a town with no rigs: the read
// must not fall back to listing the hq store alone.
func TestDashFollowupIssuesRefusesAnUnreadableRegistry(t *testing.T) {
	t.Parallel()
	listed := false
	got, err := dashFollowupIssues(t.TempDir(), func(string) ([]*beads.Issue, error) {
		listed = true
		return nil, nil
	})
	if err == nil {
		t.Fatal("a town with no rig registry read as a successful list")
	}
	if got != nil {
		t.Errorf("issues = %v, want none", got)
	}
	if listed {
		t.Error("a store was listed before the registry was read")
	}
}
