package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/dashboard"
)

func TestParseDispatchTickCountsOnly(t *testing.T) {
	t.Parallel()
	d := parseDispatchTick("2026/10/03 17:14:32 spec_dispatch: tick: 0 candidate(s), roster deepseek-flash 1/3, 2 dispatched, 1 refused, 3 planning, 0 skipped, 4 failed, 5 held by the failed label")
	if d == nil {
		t.Fatal("a tick line did not parse")
	}
	if d.Candidates != 0 || d.Roster != "deepseek-flash 1/3" || d.Dispatched != 2 || d.Refused != 1 || d.Planning != 3 || d.Failed != 4 || d.Held != 5 {
		t.Errorf("tick = %+v", d)
	}
	if len(d.Named) != 0 || d.At.Format("15:04:05") != "17:14:32" {
		t.Errorf("named/at = %+v", d)
	}
}

func TestParseDispatchTickNamesSkippedBeadsAndReasons(t *testing.T) {
	t.Parallel()
	d := parseDispatchTick("2026/10/03 17:24:40 spec_dispatch: tick: 3 candidate(s), roster deepseek-flash 0/3, 0 dispatched, 0 refused, 0 planning, 3 skipped, 0 failed, 0 held by the failed label; skipped: gt-aoltu (unshaped: ## Goal, ## Constraints, acceptance); gt-b.1 (no seat: claude-sonnet 2/2); hq-c (held) +4 more")
	if d == nil {
		t.Fatal("did not parse")
	}
	want := []dashboard.SkippedBead{
		{Bead: "gt-aoltu", Reason: "unshaped: ## Goal, ## Constraints, acceptance"},
		{Bead: "gt-b.1", Reason: "no seat: claude-sonnet 2/2"},
		{Bead: "hq-c", Reason: "held"},
	}
	if len(d.Named) != len(want) {
		t.Fatalf("named = %+v", d.Named)
	}
	for i := range want {
		if d.Named[i] != want[i] {
			t.Errorf("named[%d] = %+v, want %+v", i, d.Named[i], want[i])
		}
	}
	if d.More != 4 || d.Skipped != 3 {
		t.Errorf("more=%d skipped=%d", d.More, d.Skipped)
	}
}

func TestParseDispatchTickIgnoresOtherLines(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"2026/10/03 17:25:39 spec_dispatch: warning: gt-aoltu unshaped: ## Goal",
		"2026/10/03 17:25:39 spec_dispatch: dispatched: gt-x: slung to gastown/agate",
		"not a log line",
	} {
		if d := parseDispatchTick(line); d != nil {
			t.Errorf("%q parsed as a tick: %+v", line, d)
		}
	}
}

func TestOMReaderKeepsTheLatestDispatchTick(t *testing.T) {
	t.Parallel()
	r := &omReader{}
	r.parseLogLine("2026/10/03 17:14:32 spec_dispatch: tick: 0 candidate(s), roster deepseek-flash 1/3, 0 dispatched, 0 refused, 0 planning, 0 skipped, 0 failed, 0 held by the failed label")
	r.parseLogLine("2026/10/03 17:15:32 spec_dispatch: tick: 2 candidate(s), roster deepseek-flash 2/3, 1 dispatched, 0 refused, 0 planning, 1 skipped, 0 failed, 0 held by the failed label; skipped: gt-q (unshaped: acceptance)")
	if r.lastTick == nil || r.lastTick.Candidates != 2 || len(r.lastTick.Named) != 1 {
		t.Fatalf("lastTick = %+v", r.lastTick)
	}
}

func TestDashPolecatHintsAreReadOnlyCommands(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		dashboard.StateStalled:    {"gt polecat status gastown/amber", "gt polecat check-recovery gastown/amber"},
		dashboard.StateRecovery:   {"gt polecat check-recovery gastown/amber", "gt polecat nuke gastown/amber --dry-run"},
		dashboard.StateNeedsHuman: {"bd show gt-1"},
		dashboard.StateWorking:    nil,
		dashboard.StateIdle:       nil,
		dashboard.StateParked:     nil,
	}
	for state, want := range cases {
		got := dashPolecatHints("gastown", "amber", "gt-1", state)
		if len(got) != len(want) {
			t.Errorf("%s: %v, want %v", state, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s[%d] = %q, want %q", state, i, got[i], want[i])
			}
		}
		for _, h := range got {
			// nuke is only ever suggested as its dry run
			if len(h) > 0 && strings.Contains(h, "nuke") && !strings.Contains(h, "--dry-run") {
				t.Errorf("%s: %q suggests a destructive command", state, h)
			}
		}
	}
}
