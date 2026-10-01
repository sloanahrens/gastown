package cmd

import (
	"testing"
	"time"
)

func TestPauseActor(t *testing.T) {
	t.Parallel()
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "BD_ACTOR" {
				return v
			}
			return ""
		}
	}
	git := func() string { return " Sloan Ahrens\n" }
	if got := pauseActor(env("deacon"), git); got != "deacon" {
		t.Errorf("BD_ACTOR set: actor = %q", got)
	}
	if got := pauseActor(env(""), git); got != "Sloan Ahrens" {
		t.Errorf("BD_ACTOR unset: actor = %q, want the git identity", got)
	}
}

func TestNewPauseMarker(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 3, 0, 0, 0, time.UTC)
	m, err := newPauseMarker("deacon", " full gc ", 30*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if m.Actor != "deacon" || m.Reason != "full gc" || !m.Until.Equal(now.Add(30*time.Minute)) || !m.Since.Equal(now) {
		t.Errorf("marker = %+v", m)
	}
	for name, c := range map[string]struct {
		actor, reason string
		d             time.Duration
	}{
		"no actor":  {"", "gc", time.Minute},
		"no reason": {"deacon", " ", time.Minute},
		"zero":      {"deacon", "gc", 0},
		"too long":  {"deacon", "gc", 25 * time.Hour},
	} {
		if _, err := newPauseMarker(c.actor, c.reason, c.d, now); err == nil {
			t.Errorf("%s: err = nil, want refusal", name)
		}
	}
}
