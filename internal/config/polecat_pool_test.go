package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The seat-refill policy keys on polecat_pool (gt-y3pgh.12): absent means the
// value the plugin dispatched on before the keys existed, a value in the file
// wins, and Validate refuses what the plugin cannot act on.
func TestPolecatPool_SeatRefillPolicy(t *testing.T) {
	t.Parallel()

	t.Run("absent keys take the defaults", func(t *testing.T) {
		t.Parallel()
		var p *PolecatPool
		if p.GetMaxPriority() != DefaultSeatRefillMaxPriority ||
			p.GetTopCandidates() != DefaultSeatRefillTopCandidates ||
			p.GetEmptySeconds() != DefaultSeatRefillEmptySeconds ||
			p.GetNudgeSeconds() != DefaultSeatRefillNudgeSeconds ||
			p.GetDispatchEmptySeconds() != DefaultSeatRefillDispatchEmptySeconds ||
			p.GetMode() != DefaultSeatRefillMode ||
			p.GetShapeGate() != DefaultSeatRefillShapeGate {
			t.Fatal("a nil pool must read as every default")
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("defaults must validate: %v", err)
		}
	})

	t.Run("the file wins and survives a rewrite", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "config.json")
		settingsJSON := `{
			"type": "town-settings",
			"version": 1,
			"polecat_pool": {
				"seats": {"deepseek-flash": {"cap": 2}},
				"max_priority": 3,
				"top_candidates": 5,
				"empty_seconds": 60,
				"nudge_seconds": 0,
				"dispatch_empty_seconds": 30,
				"shape_gate": "refuse"
			}
		}`
		if err := os.WriteFile(path, []byte(settingsJSON), 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		ts, err := LoadOrCreateTownSettings(path)
		if err != nil {
			t.Fatalf("LoadOrCreateTownSettings: %v", err)
		}
		pool := ts.PolecatPool
		if pool == nil {
			t.Fatal("polecat_pool did not load")
		}
		if pool.GetMaxPriority() != 3 || pool.GetTopCandidates() != 5 || pool.GetEmptySeconds() != 60 ||
			pool.GetNudgeSeconds() != 0 || pool.GetDispatchEmptySeconds() != 30 {
			t.Errorf("int knobs not loaded: %+v", pool)
		}
		if pool.GetShapeGate() != "refuse" {
			t.Errorf("shape_gate not loaded: %q", pool.GetShapeGate())
		}
		if err := pool.Validate(); err != nil {
			t.Fatalf("the loaded pool must validate: %v", err)
		}

		if err := SaveTownSettings(path, ts); err != nil {
			t.Fatalf("SaveTownSettings: %v", err)
		}
		saved, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`"max_priority": 3`, `"top_candidates": 5`, `"empty_seconds": 60`,
			`"nudge_seconds": 0`, `"dispatch_empty_seconds": 30`, `"shape_gate": "refuse"`} {
			if !strings.Contains(string(saved), want) {
				t.Errorf("rewrite dropped %s:\n%s", want, saved)
			}
		}
	})
}

// max_total is the cap beside the per-agent ones (gt-bcpj5): it bounds live
// polecats across every seat, an absent key and a written 0 both read as a town
// with no total cap, and a file that names it loads it and keeps it.
func TestPolecatPool_MaxTotal(t *testing.T) {
	t.Parallel()
	var absent *PolecatPool
	if got := absent.GetMaxTotal(); got != 0 {
		t.Errorf("a nil pool reads as no total cap, got %d", got)
	}
	zero, three := 0, 3
	if got := (&PolecatPool{MaxTotal: &zero}).GetMaxTotal(); got != 0 {
		t.Errorf("max_total 0 = %d, want no total cap", got)
	}
	if got := (&PolecatPool{MaxTotal: &three}).GetMaxTotal(); got != 3 {
		t.Errorf("max_total 3 = %d, want 3", got)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	settingsJSON := `{"type":"town-settings","version":1,"polecat_pool":{"seats":{"deepseek-flash":{"cap":3}},"max_total":2}}`
	if err := os.WriteFile(path, []byte(settingsJSON), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ts, err := LoadOrCreateTownSettings(path)
	if err != nil {
		t.Fatalf("LoadOrCreateTownSettings: %v", err)
	}
	if got := ts.PolecatPool.GetMaxTotal(); got != 2 {
		t.Fatalf("loaded max_total = %d, want 2", got)
	}
	if err := ts.PolecatPool.Validate(); err != nil {
		t.Fatalf("a set max_total must validate: %v", err)
	}
	if err := SaveTownSettings(path, ts); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), `"max_total": 2`) {
		t.Errorf("a rewrite dropped max_total:\n%s", saved)
	}
}

// cpus is the sandbox CPU count the seat sizing divides (gt-3ueid): the pool
// block decodes it under strict decoding, a key beside it the pool does not
// name is refused, and a written value below 1 is refused with the key named
// rather than sizing a seat from nothing.
func TestSandboxPoolCPUsStrict(t *testing.T) {
	t.Parallel()

	t.Run("cpus decodes and validates", func(t *testing.T) {
		t.Parallel()
		var ts TownSettings
		err := DecodeJSONFile("settings/config.json", []byte(`{"type":"town-settings","version":1,
			"polecat_pool":{"seats":{"deepseek-flash":{"cap":3},"claude-haiku":{"cap":1}},"cpus":12}}`), &ts)
		if err != nil {
			t.Fatalf("DecodeJSONFile: %v", err)
		}
		if got := ts.PolecatPool.GetCPUs(); got != 12 {
			t.Errorf("GetCPUs() = %d, want 12", got)
		}
		if got := ts.PolecatPool.SeatTotal(); got != 4 {
			t.Errorf("SeatTotal() = %d, want 4 (the sum of the seat caps)", got)
		}
		if err := ts.PolecatPool.Validate(); err != nil {
			t.Fatalf("a set cpus must validate: %v", err)
		}
	})

	t.Run("an unknown key beside cpus is refused", func(t *testing.T) {
		t.Parallel()
		var ts TownSettings
		err := DecodeJSONFile("settings/config.json", []byte(`{"type":"town-settings","version":1,
			"polecat_pool":{"cpus":12,"nonsense":1}}`), &ts)
		if err == nil || !strings.Contains(err.Error(), "nonsense") {
			t.Fatalf("err = %v, want an unknown-key error naming nonsense", err)
		}
	})

	t.Run("cpus 0 is refused with the key named", func(t *testing.T) {
		t.Parallel()
		var ts TownSettings
		err := DecodeJSONFile("settings/config.json", []byte(`{"type":"town-settings","version":1,
			"polecat_pool":{"cpus":0}}`), &ts)
		if err != nil {
			t.Fatalf("DecodeJSONFile: %v", err)
		}
		err = ts.PolecatPool.Validate()
		if err == nil || !strings.Contains(err.Error(), "polecat_pool.cpus") {
			t.Fatalf("err = %v, want a refusal naming polecat_pool.cpus", err)
		}
	})
}

func TestPolecatPool_ValidateSeatRefillPolicy(t *testing.T) {
	t.Parallel()
	neg := -1
	zero := 0
	one := 1
	for _, tc := range []struct {
		name    string
		pool    *PolecatPool
		wantSub string // empty: the pool must validate
	}{
		{"max_priority below zero", &PolecatPool{MaxPriority: &neg}, "polecat_pool.max_priority"},
		{"empty_seconds below zero", &PolecatPool{EmptySeconds: &neg}, "polecat_pool.empty_seconds"},
		{"nudge_seconds below zero", &PolecatPool{NudgeSeconds: &neg}, "polecat_pool.nudge_seconds"},
		{"dispatch_empty_seconds below zero", &PolecatPool{DispatchEmptySeconds: &neg}, "polecat_pool.dispatch_empty_seconds"},
		{"max_total below zero", &PolecatPool{MaxTotal: &neg}, "polecat_pool.max_total"},
		{"top_candidates zero", &PolecatPool{TopCandidates: &zero}, "polecat_pool.top_candidates"},
		{"an unknown mode", &PolecatPool{Mode: "ask-the-mayor"}, "polecat_pool.mode"},
		{"an unknown shape gate", &PolecatPool{ShapeGate: "block"}, "polecat_pool.shape_gate"},
		{"nudge mode validates", &PolecatPool{Mode: "nudge"}, ""},
		{"each shape gate validates", &PolecatPool{ShapeGate: "off"}, ""},
		{"refuse validates", &PolecatPool{ShapeGate: "refuse"}, ""},
		{"zero priority is the P0 ceiling", &PolecatPool{MaxPriority: &zero}, ""},
		{"nudge_seconds 0 drops the repeat cap", &PolecatPool{NudgeSeconds: &zero}, ""},
		{"max_total 0 is a town with no total cap", &PolecatPool{MaxTotal: &zero}, ""},
		{"everything at its floor validates", &PolecatPool{MaxPriority: &zero, TopCandidates: &one, EmptySeconds: &zero, NudgeSeconds: &zero, DispatchEmptySeconds: &zero, MaxTotal: &zero}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.pool.Validate()
			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Validate() = %v, want an error naming %s", err, tc.wantSub)
			}
		})
	}
}
