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
			p.GetProMax() != DefaultSeatRefillProMax ||
			p.GetProAgent() != DefaultSeatRefillProAgent ||
			p.GetProLabel() != DefaultSeatRefillProLabel ||
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
				"overflow_agent": "deepseek-flash",
				"max_priority": 3,
				"top_candidates": 5,
				"empty_seconds": 60,
				"nudge_seconds": 0,
				"dispatch_empty_seconds": 30,
				"pro_max": 0,
				"pro_agent": "deepseek-reasoner",
				"pro_label": "hard",
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
		if pool.GetProMax() != 0 || pool.GetProAgent() != "deepseek-reasoner" || pool.GetProLabel() != "hard" {
			t.Errorf("pro seat knobs not loaded: %+v", pool)
		}
		if pool.GetShapeGate() != "refuse" {
			t.Errorf("shape_gate not loaded: %q", pool.GetShapeGate())
		}
		// pro_max 0 drops the seat, so the agent and label go unread: an
		// unset pair is not a config error there.
		if err := pool.Validate(); err != nil {
			t.Fatalf("a dropped pro seat must validate: %v", err)
		}

		if err := SaveTownSettings(path, ts); err != nil {
			t.Fatalf("SaveTownSettings: %v", err)
		}
		saved, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`"max_priority": 3`, `"top_candidates": 5`, `"empty_seconds": 60`,
			`"nudge_seconds": 0`, `"dispatch_empty_seconds": 30`, `"pro_max": 0`,
			`"pro_agent": "deepseek-reasoner"`, `"pro_label": "hard"`, `"shape_gate": "refuse"`} {
			if !strings.Contains(string(saved), want) {
				t.Errorf("rewrite dropped %s:\n%s", want, saved)
			}
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
		{"pro_max below zero", &PolecatPool{ProMax: &neg}, "polecat_pool.pro_max"},
		{"top_candidates zero", &PolecatPool{TopCandidates: &zero}, "polecat_pool.top_candidates"},
		{"an unknown mode", &PolecatPool{Mode: "ask-the-mayor"}, "polecat_pool.mode"},
		{"an unknown shape gate", &PolecatPool{ShapeGate: "block"}, "polecat_pool.shape_gate"},
		{"nudge mode validates", &PolecatPool{Mode: "nudge"}, ""},
		{"each shape gate validates", &PolecatPool{ShapeGate: "off"}, ""},
		{"refuse validates", &PolecatPool{ShapeGate: "refuse"}, ""},
		{"zero priority is the P0 ceiling", &PolecatPool{MaxPriority: &zero}, ""},
		{"nudge_seconds 0 drops the repeat cap", &PolecatPool{NudgeSeconds: &zero}, ""},
		{"an empty pro agent reads as the default", &PolecatPool{ProMax: &one}, ""},
		{"a dropped pro seat is still valid", &PolecatPool{ProMax: &zero}, ""},
		{"everything at its floor validates", &PolecatPool{MaxPriority: &zero, TopCandidates: &one, EmptySeconds: &zero, NudgeSeconds: &zero, DispatchEmptySeconds: &zero, ProMax: &zero}, ""},
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
