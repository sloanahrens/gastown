package dispatch

import (
	"path/filepath"
	"testing"
)

func TestPluginCensusPath(t *testing.T) {
	t.Parallel()
	town := filepath.Join("town", "root")
	got := PluginCensusPath(town, "town-metrics")
	if want := filepath.Join(town, "daemon", "plugin_census_town-metrics.txt"); got != want {
		t.Errorf("PluginCensusPath = %q, want %q", got, want)
	}
	if other := PluginCensusPath(town, "other"); other == got {
		t.Errorf("two plugins share one census file %q", got)
	}
}
