package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/update"
)

// kubectlJobsFake answers the two kubectl calls a roll makes: a `get jobs` returns
// listing, and a `create` succeeds and is recorded by the Job it names.
type kubectlJobsFake struct {
	listing string
	creates []string
}

func (k *kubectlJobsFake) run(_ context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
	if len(args) > 0 && args[0] == "get" {
		return k.listing, 0, nil
	}
	k.creates = append(k.creates, strings.Join(args, " "))
	return "", 0, nil
}

// TestUpdateRollHeldByTheJobFixtures drives the production Job client through the
// heartbeat with each Job the cluster can report. A Job is finished only when its
// conditions say so: an unfinished one holds the roll with a logged reason, and a
// finished one leaves the roll free. The control row has no Job, so it proves the
// roll starts.
func TestUpdateRollHeldByTheJobFixtures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		job        string
		wantCreate bool
		wantReason string
	}{
		{
			name:       "a failed pod with an active pod, no terminal condition, holds the roll",
			job:        `{"metadata":{"name":"gt-update-retry1"},"status":{"active":1,"failed":1}}`,
			wantReason: "update Job gt-update-retry1 is still active; not starting a roll",
		},
		{
			name:       "a failed pod between retries, no terminal condition, holds the roll",
			job:        `{"metadata":{"name":"gt-update-retry2"},"status":{"active":0,"failed":1}}`,
			wantReason: "update Job gt-update-retry2 is still active; not starting a roll",
		},
		{
			name:       "a Job with no status yet holds the roll",
			job:        `{"metadata":{"name":"gt-update-new1"},"status":{}}`,
			wantReason: "update Job gt-update-new1 is still active; not starting a roll",
		},
		{
			name:       "a Job whose Complete condition is True is finished",
			job:        `{"metadata":{"name":"gt-update-done1"},"status":{"succeeded":1,"conditions":[{"type":"Complete","status":"True"}]}}`,
			wantCreate: true,
			wantReason: "created Job gt-update-",
		},
		{
			name:       "a Job whose Failed condition is True is finished",
			job:        `{"metadata":{"name":"gt-update-dead1"},"status":{"failed":3,"conditions":[{"type":"Failed","status":"True"}]}}`,
			wantCreate: true,
			wantReason: "created Job gt-update-",
		},
		{
			name:       "a Job with another name prefix is not an update Job",
			job:        `{"metadata":{"name":"other-job"},"status":{"active":1}}`,
			wantCreate: true,
			wantReason: "created Job gt-update-",
		},
		{
			name:       "an empty cluster lets the roll start (control)",
			job:        "",
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
			listing := `{"items":[]}`
			if tt.job != "" {
				listing = `{"items":[` + tt.job + `]}`
			}
			kubectl := &kubectlJobsFake{listing: listing}
			r.d.updateJob = kubectlUpdateJob{dir: update.Dir(r.town), run: kubectl.run}

			r.d.triggerUpdate()

			if tt.wantCreate {
				require.Len(t, kubectl.creates, 1, "the roll did not create its Job")
				assert.Contains(t, kubectl.creates[0], "gt-update-"+green[:12]+".yaml")
			} else {
				assert.Empty(t, kubectl.creates, "a held roll created a Job")
			}
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
