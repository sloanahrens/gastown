package daemon

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jonboulle/clockwork"
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
	// The item is written to the attention queue and the escalation mail, so its
	// text is the path and the fixed sentence exactly: anything appended would
	// publish the credential the refusal exists to keep out.
	reason := got[0].Escalation.Reason
	wantText := townStateRefusedPath + " holds a credential and is not backed up"
	if reason != wantText {
		t.Errorf("item text = %q, want exactly %q", reason, wantText)
	}
	// The fixture's refused file holds this value; no sentence may carry it.
	const refusedValue = "PLANTED-123"
	if strings.Contains(reason, refusedValue) {
		t.Errorf("item text = %q, holds the credential the refused file carries", reason)
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

// TestTownStateBackupAlertFailingTickKeepsItems: a tick whose push fails after
// a refusal raised the path's item leaves the sync alert and the item open and
// clears no path key, so a clean tick is what decides the item.
func TestTownStateBackupAlertFailingTickKeepsItems(t *testing.T) {
	t.Parallel()
	d, rec, tree := townStateAlertWorld(t)
	key := townStateRefusedKey(townStateRefusedPath)

	d.syncTownStateBackup()

	// The failing tick needs a changed file to reach the push it is faulted at.
	delete(tree, townStateRefusedPath)
	tree["settings/themes/dark.json"] = &fstest.MapFile{Data: []byte(`{"bg":"#111"}`)}
	orig := d.openGitFn
	d.openGitFn = func(dir string) daemonGit { return townStateFault{orig(dir), "push"} }
	d.syncTownStateBackup()

	if open := rec.OpenEscalations(alertKeyTownStateBackup); len(open) != 1 {
		t.Fatalf("after the failing tick, the sync alert is open %d time(s), want 1", len(open))
	}
	if open := rec.OpenEscalations(key); len(open) != 1 {
		t.Errorf("after the failing tick, the path's item is open %d time(s), want 1", len(open))
	}
	if n := townStateClearsOf(rec, key); n != 0 {
		t.Errorf("a failing tick cleared the path's key %d time(s), want 0", n)
	}
}

// TestTownStateBackupAlertRaiseFailureIsRetried: an item whose raise the
// notifier dropped is raised at the next tick.
func TestTownStateBackupAlertRaiseFailureIsRetried(t *testing.T) {
	t.Parallel()
	d, rec, _ := townStateAlertWorld(t)
	key := townStateRefusedKey(townStateRefusedPath)
	d.clock = &diskMaintNoSleepClock{FakeClock: clockwork.NewFakeClockAt(diskMaintNow)}

	rec.Fail(notifyfake.KindEscalate, errors.New("injected: raise"))
	d.syncTownStateBackup()
	if open := rec.OpenEscalations(key); len(open) != 0 {
		t.Fatalf("after the dropped raise, the item is open: %+v", open)
	}

	rec.Fail(notifyfake.KindEscalate, nil)
	d.syncTownStateBackup()
	if open := rec.OpenEscalations(key); len(open) != 1 {
		t.Fatalf("after the next tick, the item is open %d time(s), want 1", len(open))
	}
}

// TestTownStateBackupAlertClearFailureIsRetried: an item whose clear the
// notifier dropped stays open, and the next tick clears it.
func TestTownStateBackupAlertClearFailureIsRetried(t *testing.T) {
	t.Parallel()
	d, rec, tree := townStateAlertWorld(t)
	key := townStateRefusedKey(townStateRefusedPath)

	d.syncTownStateBackup()
	delete(tree, townStateRefusedPath)

	rec.Fail(notifyfake.KindClear, errors.New("injected: clear"))
	d.syncTownStateBackup()
	if open := rec.OpenEscalations(key); len(open) != 1 {
		t.Fatalf("after the dropped clear, the item is open %d time(s), want 1", len(open))
	}

	rec.Fail(notifyfake.KindClear, nil)
	d.syncTownStateBackup()
	if open := rec.OpenEscalations(key); len(open) != 0 {
		t.Fatalf("after the next tick, the item is still open: %+v", open)
	}
}
