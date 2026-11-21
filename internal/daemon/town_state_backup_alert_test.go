package daemon

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// townStateRefusedPath is the file of townStateRedactTree that holds a secret
// and is refused.
const townStateRefusedPath = "settings/themes/notes.txt"

// townStateAlertWorld is a daemon whose town is the redact tree, over a seeded
// backup repository, its alerts recorded. The tree is the one the daemon reads:
// a test edits it between ticks.
func townStateAlertWorld(t *testing.T) (d *Daemon, rec *notifyfake.Recorder, tree fstest.MapFS) {
	t.Helper()
	d, f, town := townStateDaemon(t)
	rec = notifyfake.New()
	d.notifier = rec
	tree = townStateRedactTree()
	d.townStateFS = func(string) fs.FS { return tree }
	seedTownStateOrigin(t, f, town, filepath.Join(town, ".dolt-archive", "git"))
	return d, rec, tree
}

// townStateClearsOf counts the clears the recorder took for one alert key; each
// clean tick also clears the sync key, which is not what a test of a path asks.
func townStateClearsOf(rec *notifyfake.Recorder, key string) int {
	n := 0
	for _, c := range rec.Clears() {
		for _, fp := range c.Fingerprints {
			if fp == key {
				n++
			}
		}
	}
	return n
}

// TestTownStateBackupAlertRaisesOnceAndClears: a tick that refuses a path raises
// one item naming the path and no value, a second tick that refuses it again
// raises nothing, and a tick that finds it clean clears the item.
func TestTownStateBackupAlertRaisesOnceAndClears(t *testing.T) {
	t.Parallel()
	d, rec, tree := townStateAlertWorld(t)
	key := townStateRefusedKey(townStateRefusedPath)

	d.syncTownStateBackup()
	d.syncTownStateBackup()

	got := rec.Escalations()
	if len(got) != 1 || got[0].Escalation.Fingerprint != key {
		t.Fatalf("after two refusing ticks, escalations = %+v, want one under %s", got, key)
	}
	if reason := got[0].Escalation.Reason; !strings.Contains(reason, townStateRefusedPath) || strings.Contains(reason, "PLANTED-123") {
		t.Errorf("item text = %q, want the path and no value", reason)
	}

	delete(tree, townStateRefusedPath)
	d.syncTownStateBackup()

	if n := townStateClearsOf(rec, key); n != 1 {
		t.Fatalf("clears of %s = %d, want 1", key, n)
	}
	if open := rec.OpenEscalations(key); len(open) != 0 {
		t.Errorf("after the path was clean, the item is still open: %+v", open)
	}
}
