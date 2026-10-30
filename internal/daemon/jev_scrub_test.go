package daemon

import (
	"errors"
	"io"
	"log"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/dedupe"
	"github.com/steveyegge/gastown/internal/jev/jevtest"
)

// The synthetic secrets a scrub test's state carries.
const (
	scrubTestToken = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
	scrubTestHome  = "/home/alice"
)

// TestJevPatrolScrubKey: the patrol's scrub key is off when the config leaves
// it out and on when it is set, and the client the daemon builds from the
// settings it read scrubs the state only in the second case.
func TestJevPatrolScrubKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config *JevDedupeConfig
		scrubs bool
	}{
		{
			name:   "left out, the state goes as the battery built it",
			config: &JevDedupeConfig{Enabled: true, USDPerBillionTokens: 3},
		},
		{
			name:   "set, the state is scrubbed before it is sent",
			config: &JevDedupeConfig{Enabled: true, USDPerBillionTokens: 3, JevScrub: JevScrub{Scrub: true}},
			scrubs: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			settings := jevDedupeSettingsFrom(t, tt.config)
			require.Equal(t, tt.scrubs, settings.Scrub)

			body := patrolBody(t, settings)
			if tt.scrubs {
				assert.NotContains(t, body, scrubTestToken)
				assert.NotContains(t, body, scrubTestHome)
			} else {
				assert.Contains(t, body, scrubTestToken, "without the key the state is untouched")
			}
		})
	}
}

// jevDedupeSettingsFrom writes config as the town's patrol config, runs one
// jev_dedupe pass, and returns the settings the daemon handed the pass
// builder.
func jevDedupeSettingsFrom(t *testing.T, config *JevDedupeConfig) dedupe.Settings {
	t.Helper()
	town := t.TempDir()
	require.NoError(t, SavePatrolConfig(town, &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDedupe: config}}))
	read, err := ReadPatrolConfig(town)
	require.NoError(t, err)

	d := &Daemon{
		config:       &Config{TownRoot: town},
		logger:       log.New(io.Discard, "", 0),
		patrolConfig: read,
	}
	var settings dedupe.Settings
	d.jevDedupeOpen = func(_ string, s dedupe.Settings, _ io.Writer) (*dedupe.Pass, error) {
		settings = s
		return nil, errors.New("the test reads the settings and stops the pass")
	}
	d.runJevDedupe()
	return settings
}

// patrolBody sends a state holding both test secrets through the client the
// daemon builds from settings, and returns the body the client posted. The
// transport answers 502, so the call must fail with that status and the
// transport must have received exactly one request: a client that sends
// nothing fails here, not in the body checks.
func patrolBody(t *testing.T, settings dedupe.Settings) string {
	t.Helper()
	sink := &jevtest.PostedBody{}
	opts := append(jev.BatteryOptions(t.TempDir(), dedupe.Caller, nil, settings.Scrub),
		jev.WithKey("test-key"),
		jev.WithBaseURL("http://jev.test/v1/systemone"),
		jev.WithHTTPClient(&http.Client{Transport: sink}))
	c, err := jev.NewClient(opts...)
	require.NoError(t, err)

	state := map[string]string{"log": "pushed with " + scrubTestToken + " from " + scrubTestHome + "/src"}
	_, err = c.Choice(t.Context(), state, "Which team should handle this?", map[string]string{"returns": "Exchanges"})
	var status *jev.StatusError
	require.ErrorAs(t, err, &status)
	require.Equal(t, http.StatusBadGateway, status.Status)
	require.Equal(t, 1, sink.Calls, "the state must reach the transport once")
	require.NotEmpty(t, sink.Body)
	return sink.Body
}
