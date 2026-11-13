package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// redMainBeads is the work-bead reader a red-main read goes through: it lists
// issues, or fails with err.
type redMainBeads struct {
	issues []*beads.Issue
	err    error
}

func (r redMainBeads) Show(string) (*beads.Issue, error) { return nil, nil }

func (r redMainBeads) List(beads.ListOptions) ([]*beads.Issue, error) {
	return r.issues, r.err
}

// TestUpdateRollReadsRedMainFromTheBeads drives the production red-main read
// through the heartbeat: an open red-main bead holds the roll, and a bead read
// that fails holds it too, so main's state is never read as green on an error.
func TestUpdateRollReadsRedMainFromTheBeads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		reader     redMainBeads
		wantReason string
	}{
		{
			name:       "an open red-main bead holds the roll",
			reader:     redMainBeads{issues: []*beads.Issue{{ID: "gt-red1"}}},
			wantReason: "main is red on gastown: 1 open red-main bead(s)",
		},
		{
			name:       "a failed bead read holds the roll",
			reader:     redMainBeads{err: errors.New("bd timed out")},
			wantReason: "main's red state for gastown cannot be read: bd timed out",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
			r.pairFor(t, r.commits[2], true, true)
			r.d.updateTrigger.redMain = r.d.listOpenRedMain
			r.d.openWorkBeads = func([]string, time.Duration) workBeadReader { return tt.reader }

			r.d.triggerUpdate()

			assert.Empty(t, r.cluster.calls, "a held roll created a Job")
			assert.Contains(t, r.logs.String(), tt.wantReason)
		})
	}
}

// TestUpdateRollRefusesAJobClientThatIsNotWired holds the roll to a daemon with no
// cluster client: the Job list cannot be read, so no Job is created.
func TestUpdateRollRefusesAJobClientThatIsNotWired(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	r.pairFor(t, r.commits[2], true, true)
	r.d.updateJob = nil

	r.d.triggerUpdate()

	assert.Contains(t, r.logs.String(), "the update Jobs cannot be read: no cluster client is wired into this daemon")
	assert.Empty(t, r.cluster.calls, "a roll started with no cluster client wired")
}
