package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/update"
)

// TestUpdateRollHoldsAreLoggedAndCreateNoJob drives a pod roll through the
// production heartbeat to each hold a roll must clear before its Job is created.
// Each row sets one condition, and the roll must log that condition's reason and
// create nothing. The control row clears every hold and must create its Job, so
// the control proves the rig can create one: a held row that creates a Job is a
// hold that does not hold.
func TestUpdateRollHoldsAreLoggedAndCreateNoJob(t *testing.T) {
	t.Parallel()
	now := updateTriggerTestTime
	// A run that started before today's window opened and ended long ago: the
	// nightly rule is due, and no hold but the one a row sets applies.
	idle := update.State{
		RunID:     "run-1",
		StartedAt: now.Add(-24 * time.Hour),
		EndedAt:   now.Add(-24*time.Hour + time.Minute),
		Outcome:   update.OutcomeOK,
	}
	recentEnd := idle
	recentEnd.EndedAt = now.Add(-30 * time.Minute)
	// A run that started just now: a run open a day would be an overrun, which
	// raises its own escalation on the daemon's clock and is not this hold.
	running := idle
	running.StartedAt = now.Add(-2 * time.Minute)
	running.EndedAt = time.Time{}
	running.Outcome = update.OutcomeRunning
	running.HeartbeatAt = now.Add(-time.Minute)

	// tipPending is main's tip with a pending verdict that started since: the
	// roll's gate reads its age on the daemon clock.
	tipPending := func(r *updateJobRig, since time.Duration) {
		r.main.st = landworker.MainState{PostLandPending: []landworker.PendingPostLand{
			{Land: landworker.PostLand{Commit: r.commits[2]}, Context: "ci", Since: now.Add(-since)},
		}}
	}

	tests := []struct {
		name       string
		state      *update.State
		active     []string
		listErr    error
		main       func(r *updateJobRig)
		noRunner   bool
		tipExit    int
		wantCreate bool
		wantReason string
	}{
		{
			name:       "a run in progress holds the roll",
			state:      &running,
			wantReason: "an update run is in progress",
		},
		{
			name:       "an active gt-update Job holds the roll",
			active:     []string{"gt-update-manual"},
			wantReason: "update Job gt-update-manual is still active",
		},
		{
			name:       "an unreadable Job list holds the roll",
			listErr:    errors.New("the server refused the list"),
			wantReason: "the update Jobs cannot be read",
		},
		{
			name:       "a run that ended inside min_interval holds the roll",
			state:      &recentEnd,
			wantReason: "inside min_interval",
		},
		{
			name: "a red tip holds the roll, though no red-main bead names it",
			main: func(r *updateJobRig) {
				r.main.st = landworker.MainState{LastRun: r.commits[2], LastGreen: r.commits[1]}
			},
			wantReason: "main is red at {tip}; not starting a roll onto it",
		},
		{
			name:       "a pending verdict inside the bound passes",
			main:       func(r *updateJobRig) { tipPending(r, 10*time.Minute) },
			wantCreate: true,
			wantReason: "created Job gt-update-",
		},
		{
			name:       "a pending verdict older than the bound holds the roll",
			main:       func(r *updateJobRig) { tipPending(r, 3*time.Hour) },
			wantReason: "main's verdict is still pending after 3h0m0s; not starting a roll",
		},
		{
			name: "a shortened pending bound holds a verdict the constant would pass",
			main: func(r *updateJobRig) {
				r.d.updateTrigger.pendingVerdictBound = 5 * time.Minute
				tipPending(r, 10*time.Minute)
			},
			wantReason: "main's verdict is still pending after 10m0s",
		},
		{
			name:       "no verdict recorded for the tip holds the roll",
			main:       func(r *updateJobRig) { r.main.st = landworker.MainState{} },
			wantReason: "no post-land verdict is recorded for main's tip",
		},
		{
			name:       "an unreadable verdict store holds the roll",
			main:       func(r *updateJobRig) { r.main.err = errors.New("disk gone") },
			wantReason: "main's verdict for gastown cannot be read: reading the main state: disk gone",
		},
		{
			name:       "a rig with no post-land runner holds the roll",
			noRunner:   true,
			wantReason: "main's verdict for gastown cannot be read: no post-land runner is wired for gastown",
		},
		{
			name:       "a tip origin cannot report holds the roll",
			tipExit:    128,
			wantReason: "main's tip for gastown cannot be resolved",
		},
		{
			name:       "every hold clear starts the roll",
			wantCreate: true,
			wantReason: "created Job gt-update-",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
			green := r.commits[2]
			r.pairFor(t, green, true, true)
			r.cluster.active = tt.active
			r.cluster.listErr = tt.listErr
			if tt.main != nil {
				tt.main(r)
			}
			if tt.noRunner {
				r.d.postLandRunners.set("gastown", nil)
			}
			r.remote.exit = tt.tipExit
			if tt.state != nil {
				store := update.Store{Dir: update.Dir(r.town)}
				require.NoError(t, store.Write(*tt.state))
			}

			r.d.triggerUpdate()

			if tt.wantCreate {
				assert.Equal(t, []string{"gt-update-" + green[:12]}, r.cluster.calls, "the control roll did not create its Job")
			} else {
				assert.Empty(t, r.cluster.calls, "a held roll created a Job")
			}
			// {tip} stands for main's tip, a sha the rig makes at run time.
			assert.Contains(t, r.logs.String(), strings.ReplaceAll(tt.wantReason, "{tip}", green[:12]))
		})
	}
}
