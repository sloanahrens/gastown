package daemon

import (
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/townconfig"
)

// Regression test for gt-arz:
// getPatrolRigs should filter parked/docked rigs at list-building time.
func TestGetPatrolRigs_FiltersNonOperationalRigs(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	registerTestRigs(t, townRoot, map[string]string{"alpha": "al", "beta": "be", "gamma": "ga"})

	// beta is parked in the registry; gamma's identity bead is docked.
	if _, err := townconfig.Park(townRoot, "beta", config.RigParked{By: "test"}); err != nil {
		t.Fatalf("park beta: %v", err)
	}

	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: discardLogger,
		rigBeadShowFn: func(_, id string) (*beads.Issue, error) {
			if strings.HasPrefix(id, "ga-") {
				return &beads.Issue{ID: id, Labels: []string{"status:docked"}}, nil
			}
			return &beads.Issue{ID: id}, nil
		},
	}

	got := d.getPatrolRigs("witness")
	slices.Sort(got)
	if want := []string{"alpha"}; !slices.Equal(got, want) {
		t.Fatalf("getPatrolRigs() = %v, want %v", got, want)
	}
}
