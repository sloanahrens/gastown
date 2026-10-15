package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
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
