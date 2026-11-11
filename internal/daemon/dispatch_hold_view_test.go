package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// TestLogDispatchHoldView: the daemon logs its view of the dispatch hold once
// per change, and not on the heartbeats in between.
func TestLogDispatchHoldView(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	logger, lines := lineLogger()
	d.logger = logger
	holdPath := filepath.Join(d.config.TownRoot, dispatch.HoldFileName)

	d.logDispatchHoldView() // clear: the first view is logged
	d.logDispatchHoldView() // still clear: nothing
	if err := os.WriteFile(holdPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d.logDispatchHoldView() // hold appears: logged
	d.logDispatchHoldView() // still held: nothing
	if err := os.Remove(holdPath); err != nil {
		t.Fatal(err)
	}
	d.logDispatchHoldView() // hold lifts: logged

	want := []string{
		"Dispatch hold view: clear",
		"Dispatch hold view: operator dispatch hold present (" + holdPath + "); remove it to resume",
		"Dispatch hold view: clear",
	}
	if got := lines(); !reflect.DeepEqual(got, want) {
		t.Errorf("logged lines = %q\nwant %q", got, want)
	}
}
