package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/config"
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

	tests := []struct {
		name       string
		state      *update.State
		active     []string
		listErr    error
		red        int
		redErr     error
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
			name:       "an open red-main bead holds the roll",
			red:        1,
			wantReason: "main is red on gastown: 1 open red-main bead(s)",
		},
		{
			name:       "an unreadable red-main state holds the roll",
			redErr:     errors.New("bd list failed"),
			wantReason: "main's red state for gastown cannot be read",
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
			r.d.updateTrigger.redMain = func(string) (int, error) { return tt.red, tt.redErr }
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
			assert.Contains(t, r.logs.String(), tt.wantReason)
		})
	}
}

// TestUpdateRollRefusesAnUnwiredRedMainRead holds the red-main read to the
// refusal the daemon must make when nothing is wired: an unseen main is never
// read as green.
func TestUpdateRollRefusesAnUnwiredRedMainRead(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	green := r.commits[2]
	r.pairFor(t, green, true, true)
	r.d.updateTrigger.redMain = nil

	r.d.triggerUpdate()

	assert.Empty(t, r.cluster.calls, "a roll started with no red-main read wired")
	assert.Contains(t, r.logs.String(), "no red-main read is wired")
}
