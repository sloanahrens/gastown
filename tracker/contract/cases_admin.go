package contract

import (
	"errors"
	"maps"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "stats count durable issues by status per rig, closed included, and wisps as one total", Run: adminStats})
	register(Case{Name: "config returns what was set, unset keys report unset, and a set replaces", Run: adminConfig})
	register(Case{Name: "kv get returns what was set, and an unset key reports unset", Run: adminKVGet, Fake: true})
	register(Case{Name: "kv set replaces an earlier value", Run: adminKVOverwrite, Fake: true})
	register(Case{Name: "kv keeps a blank value as set and empty", Run: adminKVBlank, Fake: true})
	register(Case{Name: "kv refuses an empty key on get and set", Run: adminKVEmptyKey, Fake: true})
	register(Case{Name: "kv list returns every key set", Run: adminKVList, Fake: true})
}

func adminStats(t *testing.T, s store.Store) {
	mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "open"})
	prog := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "in progress"})
	mustUpdate(t, s, prog.ID, api.UpdatePatch{Status: str("in_progress")}, api.Guard{})
	done := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "done"})
	if err := s.CloseIssues(ctx(), []string{done.ID}, "r", false); err != nil {
		t.Fatal(err)
	}
	wisp := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a wisp", Ephemeral: true})
	// Held at a status no durable issue has, so a status that only wisps carry
	// is visibly left out of by_status instead of reported as zero.
	mustUpdate(t, s, wisp.ID, api.UpdatePatch{Status: str("deferred")}, api.Guard{})
	mustCreate(t, s, api.CreateParams{Rig: "hq", Title: "other rig"})

	gt, err := s.Stats(ctx(), "gt")
	if err != nil {
		t.Fatal(err)
	}
	if gt.Total != 3 || gt.ByStatus["open"] != 1 || gt.ByStatus["in_progress"] != 1 || gt.ByStatus["closed"] != 1 {
		t.Errorf("gt stats = %+v, want 3 in all: one open, one in progress, one closed", gt)
	}
	if gt.Wisps != 1 {
		t.Errorf("gt wisps = %d, want 1: the wisp counted while the durable counts are not", gt.Wisps)
	}
	if n, ok := gt.ByStatus["deferred"]; ok {
		t.Errorf("gt by_status has deferred = %d, want the key absent: only a wisp holds that status", n)
	}
	all, err := s.Stats(ctx(), "")
	if err != nil || all.Total != 4 || all.Wisps != 1 {
		t.Errorf("stats of every rig = %+v, %v; want 4 durable (gt's three and hq's one) and 1 wisp", all, err)
	}
	empty, err := s.Stats(ctx(), "nosuchrig")
	if err != nil || empty.Total != 0 || empty.Wisps != 0 || empty.ByStatus == nil {
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

func adminKVGet(t *testing.T, s store.Store) {
	if v, ok, err := s.KVGet(ctx(), "probe"); err != nil || ok || v != "" {
		t.Errorf("KVGet(unset) = %q, %v, %v; want empty, not set, no error", v, ok, err)
	}
	if err := s.KVSet(ctx(), "probe", "x"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.KVGet(ctx(), "probe"); err != nil || !ok || v != "x" {
		t.Errorf("KVGet after set = %q, %v, %v; want x and set", v, ok, err)
	}
}

func adminKVOverwrite(t *testing.T, s store.Store) {
	if err := s.KVSet(ctx(), "probe", "first"); err != nil {
		t.Fatal(err)
	}
	if err := s.KVSet(ctx(), "probe", "second"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.KVGet(ctx(), "probe"); err != nil || !ok || v != "second" {
		t.Errorf("KVGet after overwrite = %q, %v, %v; want second and set", v, ok, err)
	}
}

func adminKVBlank(t *testing.T, s store.Store) {
	// An empty value is a set key with an empty value, not an unset one.
	if err := s.KVSet(ctx(), "blank", ""); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.KVGet(ctx(), "blank"); err != nil || !ok || v != "" {
		t.Errorf("KVGet(blank) = %q, %v, %v; want empty and set", v, ok, err)
	}
}

func adminKVEmptyKey(t *testing.T, s store.Store) {
	if err := s.KVSet(ctx(), "", "x"); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("KVSet with no key = %v, want api.ErrInvalid", err)
	}
	if _, _, err := s.KVGet(ctx(), ""); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("KVGet with no key = %v, want api.ErrInvalid", err)
	}
}

func adminKVList(t *testing.T, s store.Store) {
	if got, err := s.KVList(ctx()); err != nil || got == nil || len(got) != 0 {
		t.Errorf("KVList of an empty table = %v, %v; want an empty, non-nil map", got, err)
	}
	want := map[string]string{"a": "1", "b": "", "c": "3"}
	for k, v := range want {
		if err := s.KVSet(ctx(), k, v); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.KVList(ctx())
	if err != nil || !maps.Equal(got, want) {
		t.Errorf("KVList = %v, %v; want every key and value set, blank included", got, err)
	}
}
