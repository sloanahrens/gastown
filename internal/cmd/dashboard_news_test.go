package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeDashboardSettings writes settings/config.json for a town, which is where
// the dashboard's own switches live.
func writeDashboardSettings(t *testing.T, townRoot, body string) {
	t.Helper()

	dir := filepath.Join(townRoot, "settings")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600))
}

// The News pane runs unless settings/config.json turns it off, and a town with
// no settings file at all has it on: a missing file is the default town, not a
// switched-off pane (gt-9ktrr).
func TestDashboardNewsEnabledDefaultsToOn(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	on, err := dashboardNewsEnabled(town)
	require.NoError(t, err)
	assert.True(t, on, "a town with no settings file keeps the pane")

	writeDashboardSettings(t, town, `{"type":"town-settings","version":1,"dashboard":{"news":{"enabled":true}}}`)
	on, err = dashboardNewsEnabled(town)
	require.NoError(t, err)
	assert.True(t, on)

	writeDashboardSettings(t, town, `{"type":"town-settings","version":1,"dashboard":{"news":{"enabled":false}}}`)
	on, err = dashboardNewsEnabled(town)
	require.NoError(t, err)
	assert.False(t, on, "dashboard.news.enabled=false switches the pane off")

	// A section with no key under it is the pane's own default rather than off.
	writeDashboardSettings(t, town, `{"type":"town-settings","version":1,"dashboard":{}}`)
	on, err = dashboardNewsEnabled(town)
	require.NoError(t, err)
	assert.True(t, on)
}

// The switch is read through the settings loader, which refuses a key it does
// not know: a dashboard section written with a typo fails loudly instead of
// quietly leaving the pane on, which is what a second, looser reader here would
// do (gt-9ktrr).
func TestDashboardNewsEnabledRefusesAnUnknownKey(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	writeDashboardSettings(t, town, `{"type":"town-settings","version":1,"dashboard":{"news":{"enabledd":false}}}`)

	on, err := dashboardNewsEnabled(town)
	require.Error(t, err, "a mistyped key must not read as a working switch")
	assert.False(t, on)
	assert.Contains(t, err.Error(), "dashboard.news.enabled")
}

// The pane's reader is wired only when the switch is on, and the switch being
// off leaves the hub with no news reader at all, so nothing fetches Hacker News
// and the page draws no section (gt-9ktrr).
func TestDashboardNewsReaderFollowsTheSwitch(t *testing.T) {
	t.Parallel()

	off := t.TempDir()
	writeDashboardSettings(t, off, `{"type":"town-settings","version":1,"dashboard":{"news":{"enabled":false}}}`)
	read, err := dashboardNewsReader(off)
	require.NoError(t, err)
	assert.Nil(t, read, "a switched-off pane gets no reader")

	on := t.TempDir()
	read, err = dashboardNewsReader(on)
	require.NoError(t, err)
	require.NotNil(t, read)

	// A settings file the town cannot read is an error, not a silent default:
	// the pane coming back on by accident is the failure the switch prevents.
	broken := t.TempDir()
	writeDashboardSettings(t, broken, `{"type":"town-settings","version":1,"dashboard":{"news":`)
	_, err = dashboardNewsReader(broken)
	require.Error(t, err)
}
