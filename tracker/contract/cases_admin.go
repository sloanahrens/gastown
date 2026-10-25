package contract

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "stats count durable issues by status per rig, closed included and wisps not", Run: adminStats})
	register(Case{Name: "config returns what was set, unset keys report unset, and a set replaces", Run: adminConfig})
}

func adminStats(t *testing.T, s store.Store) {
	mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "open"})
	prog := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "in progress"})
	mustUpdate(t, s, prog.ID, api.UpdatePatch{Status: str("in_progress")}, api.Guard{})
	done := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "done"})
	if err := s.CloseIssues(ctx(), []string{done.ID}, "r", false); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a wisp", Ephemeral: true})
	mustCreate(t, s, api.CreateParams{Rig: "hq", Title: "other rig"})

	gt, err := s.Stats(ctx(), "gt")
	if err != nil {
		t.Fatal(err)
	}
	if gt.Total != 3 || gt.ByStatus["open"] != 1 || gt.ByStatus["in_progress"] != 1 || gt.ByStatus["closed"] != 1 {
		t.Errorf("gt stats = %+v, want 3 in all: one open, one in progress, one closed, and the wisp not counted", gt)
	}
	all, err := s.Stats(ctx(), "")
	if err != nil || all.Total != 4 {
		t.Errorf("stats of every rig = %+v, %v; want 4 (gt's three and hq's one)", all, err)
	}
	empty, err := s.Stats(ctx(), "nosuchrig")
	if err != nil || empty.Total != 0 || empty.ByStatus == nil {
		t.Errorf("stats of a rig with nothing = %+v, %v; want zero and a non-nil map", empty, err)
	}
}

func adminConfig(t *testing.T, s store.Store) {
	if v, ok, err := s.ConfigGet(ctx(), "custom.probe"); err != nil || ok || v != "" {
		t.Errorf("ConfigGet(unset) = %q, %v, %v; want empty, not set", v, ok, err)
	}
	if err := s.ConfigSet(ctx(), "types.custom", "agent,rig"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.ConfigGet(ctx(), "types.custom"); err != nil || !ok || v != "agent,rig" {
		t.Errorf("ConfigGet after set = %q, %v, %v", v, ok, err)
	}
	if err := s.ConfigSet(ctx(), "types.custom", "agent"); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := s.ConfigGet(ctx(), "types.custom"); v != "agent" {
		t.Errorf("ConfigGet after overwrite = %q, want agent", v)
	}
	// An empty value is a set key with an empty value, not an unset one.
	if err := s.ConfigSet(ctx(), "blank", ""); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.ConfigGet(ctx(), "blank"); err != nil || !ok || v != "" {
		t.Errorf("ConfigGet(blank) = %q, %v, %v; want empty and set", v, ok, err)
	}
	if err := s.ConfigSet(ctx(), "", "x"); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("ConfigSet with no key = %v, want api.ErrInvalid", err)
	}
	if _, _, err := s.ConfigGet(ctx(), ""); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("ConfigGet with no key = %v, want api.ErrInvalid", err)
	}
}
