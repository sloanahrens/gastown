package daemon

import (
	"errors"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/reconcile"
)

func TestReconcilerDueDuties(t *testing.T) {
	t.Parallel()
	obs := func(duty string, verdict reconcile.Verdict) reconcile.Observation {
		return reconcile.Observation{Duty: duty, Verdict: verdict}
	}
	rootErr := errors.New("the town state root is unset")
	tests := []struct {
		name             string
		obs              []reconcile.Observation
		townStateRootErr error
		want             []string
	}{
		{name: "a due backup is due", obs: []reconcile.Observation{obs("backup", reconcile.VerdictDue)}, want: []string{"backup"}},
		{name: "an overdue backup is due", obs: []reconcile.Observation{obs("backup", reconcile.VerdictOverdue)}, want: []string{"backup"}},
		{name: "a due archive is due", obs: []reconcile.Observation{obs("archive", reconcile.VerdictDue)}, want: []string{"archive"}},
		{name: "an overdue archive is due", obs: []reconcile.Observation{obs("archive", reconcile.VerdictOverdue)}, want: []string{"archive"}},
		{name: "a due gc names its database", obs: []reconcile.Observation{obs("gc:hq", reconcile.VerdictDue)}, want: []string{"gc:hq"}},
		{name: "an overdue gc names its database", obs: []reconcile.Observation{obs("gc:gastown", reconcile.VerdictOverdue)}, want: []string{"gc:gastown"}},
		{name: "a due town_state with a root is due", obs: []reconcile.Observation{obs("town_state", reconcile.VerdictDue)}, want: []string{"town_state"}},
		{name: "an overdue town_state with a root is due", obs: []reconcile.Observation{obs("town_state", reconcile.VerdictOverdue)}, want: []string{"town_state"}},
		{name: "an ok backup is left out", obs: []reconcile.Observation{obs("backup", reconcile.VerdictOK)}},
		{name: "an unknown archive is left out", obs: []reconcile.Observation{obs("archive", reconcile.VerdictUnknown)}},
		{name: "an unrelated duty is left out", obs: []reconcile.Observation{obs("dolt_backup", reconcile.VerdictOverdue)}},
		{name: "a bare gc is left out", obs: []reconcile.Observation{obs("gc", reconcile.VerdictDue)}},
		{name: "a gc with no database is left out", obs: []reconcile.Observation{obs("gc:", reconcile.VerdictOverdue)}},
		{name: "a town_state with no root is left out", obs: []reconcile.Observation{obs("town_state", reconcile.VerdictOverdue)}, townStateRootErr: rootErr},
		{name: "input order is kept", obs: []reconcile.Observation{
			obs("gc:zeta", reconcile.VerdictOverdue),
			obs("backup", reconcile.VerdictOK),
			obs("backup", reconcile.VerdictDue),
			obs("town_state", reconcile.VerdictDue),
		}, want: []string{"gc:zeta", "backup", "town_state"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := reconcilerDueDuties(tt.obs, tt.townStateRootErr); !slices.Equal(got, tt.want) {
				t.Errorf("reconcilerDueDuties() = %q, want %q", got, tt.want)
			}
		})
	}
}
