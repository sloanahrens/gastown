package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/land"
)

// Every collector the daemon registers raises a kind that has a rule row in the
// flags table, and the host rules the collectors name are the table's rows.
func TestAttentionEveryCollectorRuleHasFlags(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	for _, c := range f.src.collectors() {
		it := attention.Item{Kind: c.kind, Key: "k"}
		if c.kind == attention.KindHostDisk {
			for _, key := range []string{hostRuleDiskWarn, hostRuleDiskCrit} {
				if _, ok := attention.Flags(attention.RuleFor(attention.Item{Kind: c.kind, Key: key})); !ok {
					t.Errorf("host disk rule %q has no flags row", key)
				}
			}
			continue
		}
		rule := attention.RuleFor(it)
		if _, ok := attention.Flags(rule); !ok {
			t.Errorf("collector %s: rule %q has no flags row", c.kind, rule)
		}
	}
	for _, key := range []string{hostRuleMemLow, hostRuleDockerDown} {
		if _, ok := attention.Flags(key); !ok {
			t.Errorf("host rule %q has no flags row", key)
		}
	}
	if got := attention.RuleFor(attention.Item{Kind: attention.KindHostProbe, Key: hostProbeFailedPrefix + hostProbeDisk}); got != attention.RuleHostProbe {
		t.Errorf("probe item rule = %q, want %q", got, attention.RuleHostProbe)
	}
}

// The events a tick writes carry the flags of the rule that raised them.
func TestAttentionTickEventsCarryRuleFlags(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	h := healthyHost()
	h.memFree = 9
	h.containersErr = errors.New("docker.sock: connection refused")
	f.src.host = h
	f.tick(t, now)

	byKey := map[string]attention.Event{}
	for _, e := range f.events(t) {
		byKey[e.Key] = e
	}
	mem := byKey[hostRuleMemLow]
	if mem.Rule != attention.RuleHostMemLow || mem.Handled != attention.HandledOperatorAction || !mem.Wake {
		t.Errorf("mem_low event = %+v, want operator_action and wake", mem)
	}
	probe := byKey[hostProbeFailedPrefix+hostProbeDocker]
	if probe.Rule != attention.RuleHostProbe || probe.Handled != attention.HandledInfo || probe.Wake {
		t.Errorf("probe event = %+v, want info and no wake", probe)
	}
}

// A risk-path item is info until its landing is three hours old, then raises
// one operator_action wake line, once, aged from the landing record: a daemon
// restart (tick reads state.json back) neither delays nor repeats it, and the
// review note clears the item.
func TestAttentionRiskPathPromotesThreeHoursAfterLanding(t *testing.T) {
	t.Parallel()
	landed := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	// The item is first raised long after the landing was recorded, so the
	// age cannot be counted from first_seen.
	first := landed.Add(2 * time.Hour)
	f := newAttentionFixture(t, first)
	head := strings.Repeat("c", 40)
	f.src.riskLandings = func(string, time.Time) ([]land.LandingRecord, error) {
		return []land.LandingRecord{{BeadID: "gt-abc", Head: head, RiskPaths: []string{"internal/land/land.go"}, LandedAt: landed}}, nil
	}
	notes := ""
	f.src.riskNotes = func(context.Context, string, string) (string, error) { return notes, nil }

	wakes := func() []attention.Event {
		var out []attention.Event
		for _, e := range f.events(t) {
			if e.Wake {
				out = append(out, e)
			}
		}
		return out
	}

	f.tick(t, first)
	f.tick(t, landed.Add(attention.RiskPathPromoteAfter-time.Second))
	evs := f.events(t)
	if len(evs) != 1 || evs[0].Rule != attention.RuleRiskPath || evs[0].Handled != attention.HandledInfo || evs[0].Wake {
		t.Fatalf("under three hours events = %+v, want the one info line", evs)
	}

	over := landed.Add(attention.RiskPathPromoteAfter + time.Second)
	f.tick(t, over)
	got := wakes()
	if len(got) != 1 {
		t.Fatalf("over three hours wake events = %+v, want one", got)
	}
	if got[0].Rule != attention.RuleRiskPath || got[0].Handled != attention.HandledOperatorAction || got[0].State != attention.EventNew || got[0].Key != "risk:gt-abc:"+head[:12] {
		t.Errorf("promotion = %+v", got[0])
	}

	f.tick(t, over.Add(time.Minute))
	f.tick(t, over.Add(2*time.Minute))
	if n := len(wakes()); n != 1 {
		t.Errorf("wake events after more ticks = %d, want the promotion once", n)
	}

	notes = land.OverseerReviewMarker + " " + head + " PASS"
	st := f.tick(t, over.Add(3*time.Minute))
	if _, ok := attention.Find(st, "risk:gt-abc:"+head[:12]); ok {
		t.Errorf("state = %+v, want the reviewed item cleared", st.Items)
	}
	last := f.events(t)[len(f.events(t))-1]
	if last.State != attention.EventCleared || last.Wake {
		t.Errorf("last event = %+v, want a cleared line that does not wake", last)
	}
}

// host.disk_warn is info until it has held for three consecutive heartbeats,
// then raises one operator_action wake line, once.
func TestAttentionDiskWarnPromotesAfterThreeBeats(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	h := healthyHost()
	h.disk = hostDisk{UsedPct: 85, FreeGB: 60}
	f.src.host = h

	wakes := func() []attention.Event {
		var out []attention.Event
		for _, e := range f.events(t) {
			if e.Wake {
				out = append(out, e)
			}
		}
		return out
	}

	for beat := 1; beat <= 2; beat++ {
		f.tick(t, now.Add(time.Duration(beat)*time.Minute))
	}
	evs := f.events(t)
	if len(evs) != 1 || evs[0].Rule != attention.RuleHostDiskWarn || evs[0].Handled != attention.HandledInfo || evs[0].Wake {
		t.Fatalf("after two beats events = %+v, want the one info line", evs)
	}

	f.tick(t, now.Add(3*time.Minute))
	got := wakes()
	if len(got) != 1 {
		t.Fatalf("after three beats wake events = %+v, want one", got)
	}
	if got[0].Rule != attention.RuleHostDiskWarn || got[0].Handled != attention.HandledOperatorAction || got[0].Key != hostRuleDiskWarn || got[0].State != attention.EventNew {
		t.Errorf("promotion = %+v", got[0])
	}

	f.tick(t, now.Add(4*time.Minute))
	f.tick(t, now.Add(5*time.Minute))
	if n := len(wakes()); n != 1 {
		t.Errorf("wake events after five beats = %d, want the promotion once", n)
	}

	// The warning clears, then recurs: the count starts again.
	h.disk = hostDisk{UsedPct: 40, FreeGB: 300}
	f.tick(t, now.Add(6*time.Minute))
	h.disk = hostDisk{UsedPct: 85, FreeGB: 60}
	for beat := 7; beat <= 8; beat++ {
		f.tick(t, now.Add(time.Duration(beat)*time.Minute))
	}
	if n := len(wakes()); n != 1 {
		t.Fatalf("wake events two beats into a recurrence = %d, want 1", n)
	}
	f.tick(t, now.Add(9*time.Minute))
	if n := len(wakes()); n != 2 {
		t.Errorf("wake events three beats into a recurrence = %d, want 2", n)
	}
}

// The disk-warn promotion honors an ack, and it survives a daemon restart: the
// beat count is memory only, so the promotion is marked on the item in the
// state, and a fresh count over the persisted item does not wake again.
func TestDiskWarnPromotionSkipsAckedAndRestart(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	warning := func(f *attentionFixture) {
		h := healthyHost()
		h.disk = hostDisk{UsedPct: 85, FreeGB: 60}
		f.src.host = h
	}
	wakes := func(f *attentionFixture) int {
		n := 0
		for _, e := range f.events(t) {
			if e.Wake {
				n++
			}
		}
		return n
	}

	t.Run("an acked item raises no promotion wake", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, now)
		warning(f)
		f.tick(t, now.Add(time.Minute))
		if err := attention.Acknowledge(f.d.config.TownRoot, hostRuleDiskWarn, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		for beat := 2; beat <= 6; beat++ {
			f.tick(t, now.Add(time.Duration(beat)*time.Minute))
		}
		if n := wakes(f); n != 0 {
			t.Errorf("wake events for an acked warning = %d, want 0", n)
		}
	})

	t.Run("a fresh cache over a persisted promotion does not wake again", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, now)
		warning(f)
		for beat := 1; beat <= 3; beat++ {
			f.tick(t, now.Add(time.Duration(beat)*time.Minute))
		}
		if n := wakes(f); n != 1 {
			t.Fatalf("wake events after three beats = %d, want 1", n)
		}

		// A restart: the beat count starts over, the state on disk stays.
		f.src.diskWarnBeats = 0
		for beat := 4; beat <= 8; beat++ {
			f.tick(t, now.Add(time.Duration(beat)*time.Minute))
		}
		if n := wakes(f); n != 1 {
			t.Errorf("wake events after a restart with the warning held = %d, want the one already written", n)
		}
	})
}
