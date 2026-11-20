package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// coldStartHealth builds the daemon's health sources as a rolled town has them:
// every record the daemon before the roll persisted is on disk, and every
// record this daemon keeps in memory is empty — no previous report, no
// dispatch tick, no seat-settings read, no landing-infra observation. The
// first tick of a freshly started daemon computes from this result, so the
// cold-start tables read today's behaviour without touching production code
// (gt-ogxa07). A table adds rows about fields; it leaves this helper alone
// (gt-53pe0n).
func coldStartHealth(t *testing.T) townhealth.Inputs {
	t.Helper()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, _ := healthTown(t, now)
	// A rolled town has its identity file, so the config field judges the town
	// rather than the fixture. healthTown leaves it absent because the test it
	// serves pins a town with no config at all.
	writeJSONFile(t, filepath.Join(d.config.TownRoot, "mayor", "town.json"), map[string]any{
		"type": "town", "version": 2, "name": "gt",
		"owner": "operator@example.com", "public_name": "gt",
		"dolt": map[string]any{"port": 3307},
	})
	// The rolled daemon overwrote the previous process's state as it started:
	// started now, the start written as the heartbeat, count 0, so the first
	// tick reads a young process as fresh rather than as one that never beat
	// (startState, gt-v9oug).
	if err := SaveState(d.config.TownRoot, startState(1, now, "cold-start")); err != nil {
		t.Fatal(err)
	}
	src := &healthSources{d: d, evidence: townhealth.DefaultThresholds().SeatEvidence}
	stubHealthProbes(src, d.config.TownRoot, now)
	// A roll drains the landing passes before it restarts the daemon
	// (gt-u98suh), so no slot lock is held at the first tick.
	src.slots = func() (slot.Report, error) { return slot.Report{}, nil }
	th := townhealth.DefaultThresholds()
	th.DeadSamples = PatrolScanDeadSamples(d.patrol())
	// The nil previous report is the empty in-memory record: no tick has
	// computed a report yet.
	return src.inputs(now, th, nil)
}

// TestTownHealthColdStartProcessFields pins what the town health reads on the
// first tick of a freshly rolled daemon for the process fields, all of which
// are answered from the daemon itself or a live probe rather than bd. It
// records today's behaviour; a field that reads UNKNOWN here is a cold-start
// defect and is named in the bead notes (gt-ogxa07).
func TestTownHealthColdStartProcessFields(t *testing.T) {
	t.Parallel()
	in := coldStartHealth(t)
	r := townhealth.Compute(context.Background(), in)

	for _, want := range []struct {
		key     string
		tag     townhealth.Tag
		verdict townhealth.Verdict
		value   string
		detail  string
	}{
		{key: "dolt", tag: townhealth.Live, verdict: townhealth.Green, value: "p50 4ms"},
		{key: "exec-tax", tag: townhealth.Live, verdict: townhealth.Green, value: "9ms/exec"},
		{key: "host", tag: townhealth.Live, verdict: townhealth.Green, value: "load 2 on 8 cores"},
		{key: "daemon", tag: townhealth.Recorded, verdict: townhealth.Green, value: "heartbeat 0ms ago"},
		{key: "tick:git_hygiene", tag: townhealth.Recorded, verdict: townhealth.Green, value: "50m/12h"},
		{key: "tick:events_prune", tag: townhealth.Recorded, verdict: townhealth.Green, value: "50m/1h"},
		{key: "slot", tag: townhealth.Recorded, verdict: townhealth.Green, value: "free"},
		{key: "dispatch", tag: townhealth.Recorded, verdict: townhealth.Green, value: "no ticks",
			detail: "no tick in the last 10m; the daemon is younger than that"},
	} {
		got := healthField(t, r, want.key)
		if got.Tag != want.tag || got.Verdict != want.verdict || got.Value != want.value || got.Detail != want.detail {
			t.Errorf("%s = %s %s %q %q, want %s %s %q %q",
				want.key, got.Tag, got.Verdict, got.Value, got.Detail,
				want.tag, want.verdict, want.value, want.detail)
		}
	}
}
