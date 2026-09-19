package daemon

import (
	"testing"
	"time"
)

func TestScheduledSlingEntry_Validate(t *testing.T) {
	good := ScheduledSlingEntry{Name: "doc-audit", Rig: "gastown", Formula: "mol-doc-audit", IntervalStr: "168h"}
	if err := good.validate(); err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}
	cases := map[string]ScheduledSlingEntry{
		"empty name":    {Rig: "gastown", Formula: "f", IntervalStr: "1h"},
		"bad name":      {Name: "Doc Audit", Rig: "gastown", Formula: "f", IntervalStr: "1h"},
		"empty rig":     {Name: "a", Formula: "f", IntervalStr: "1h"},
		"empty formula": {Name: "a", Rig: "gastown", IntervalStr: "1h"},
		"no interval":   {Name: "a", Rig: "gastown", Formula: "f"},
		"bad interval":  {Name: "a", Rig: "gastown", Formula: "f", IntervalStr: "weekly"},
		"zero interval": {Name: "a", Rig: "gastown", Formula: "f", IntervalStr: "0s"},
	}
	for name, e := range cases {
		if err := e.validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if got := good.label(); got != "scheduled:doc-audit" {
		t.Errorf("label = %q", got)
	}
	if got := good.priority(); got != 3 {
		t.Errorf("default priority = %d, want 3", got)
	}
	if got := good.interval(); got != 168*time.Hour {
		t.Errorf("interval = %v", got)
	}
}

func TestDecideScheduledSling(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	week := 168 * time.Hour
	cases := []struct {
		name  string
		beads []scheduledBead
		want  scheduledAction
	}{
		{"no beads", nil, scheduledDispatch},
		{"open bead", []scheduledBead{{ID: "gt-1", Status: "open", CreatedAt: now.Add(-30 * 24 * time.Hour)}}, scheduledSkipOpen},
		{"in_progress bead", []scheduledBead{{ID: "gt-1", Status: "in_progress", CreatedAt: now.Add(-2 * time.Hour)}}, scheduledSkipOpen},
		{"closed recent", []scheduledBead{{ID: "gt-1", Status: "closed", CreatedAt: now.Add(-2 * 24 * time.Hour)}}, scheduledSkipRecent},
		{"closed old", []scheduledBead{{ID: "gt-1", Status: "closed", CreatedAt: now.Add(-8 * 24 * time.Hour)}}, scheduledDispatch},
		{"mixed: newest closed old, older open", []scheduledBead{
			{ID: "gt-1", Status: "closed", CreatedAt: now.Add(-8 * 24 * time.Hour)},
			{ID: "gt-0", Status: "open", CreatedAt: now.Add(-20 * 24 * time.Hour)},
		}, scheduledSkipOpen},
		{"exactly one interval ago", []scheduledBead{{ID: "gt-1", Status: "closed", CreatedAt: now.Add(-week)}}, scheduledDispatch},
	}
	for _, c := range cases {
		if got := decideScheduledSling(c.beads, week, now); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestParseScheduledBeads(t *testing.T) {
	data := []byte(`[{"id":"gt-abc","status":"closed","created_at":"2026-09-12T10:00:00Z","labels":["scheduled:doc-audit"]},
	                 {"id":"gt-def","status":"open","created_at":"2026-09-19T10:00:00-05:00"}]`)
	got, err := parseScheduledBeads(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "gt-abc" || got[1].Status != "open" {
		t.Fatalf("parsed %+v", got)
	}
	if !got[1].CreatedAt.Equal(time.Date(2026, 9, 19, 15, 0, 0, 0, time.UTC)) {
		t.Errorf("created_at not parsed with offset: %v", got[1].CreatedAt)
	}
	if _, err := parseScheduledBeads([]byte(`not json`)); err == nil {
		t.Error("expected error on bad json")
	}
	empty, err := parseScheduledBeads([]byte(`[]`))
	if err != nil || len(empty) != 0 {
		t.Errorf("empty list: %v %v", empty, err)
	}
}

func TestParseCreatedBeadID(t *testing.T) {
	for _, in := range []string{`{"id":"gt-new1","title":"x"}`, `[{"id":"gt-new1","title":"x"}]`} {
		id, err := parseCreatedBeadID([]byte(in))
		if err != nil || id != "gt-new1" {
			t.Errorf("%s: id=%q err=%v", in, id, err)
		}
	}
	if _, err := parseCreatedBeadID([]byte(`{}`)); err == nil {
		t.Error("expected error when id is missing")
	}
}

func TestIsPatrolEnabled_ScheduledSlingsIsOptIn(t *testing.T) {
	if IsPatrolEnabled(nil, "scheduled_slings") {
		t.Error("nil config must not enable scheduled_slings")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "scheduled_slings") {
		t.Error("absent block must not enable scheduled_slings")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{ScheduledSlings: &ScheduledSlingsConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "scheduled_slings") {
		t.Error("enabled block must enable scheduled_slings")
	}
}
