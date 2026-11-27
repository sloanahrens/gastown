package attention

import (
	"testing"
	"time"
)

// TestHostGoCacheTrimTables pins the judge's kind, rule, flags and owner (gt-a0sewj).
func TestHostGoCacheTrimTables(t *testing.T) {
	t.Parallel()

	listed := false
	for _, k := range Kinds() {
		listed = listed || k == KindHostGoCacheTrim
	}
	if !listed {
		t.Errorf("Kinds() does not list %q", KindHostGoCacheTrim)
	}
	if got := RuleFor(Item{Kind: KindHostGoCacheTrim}); got != RuleHostGoCacheTrim {
		t.Errorf("RuleFor(%q) = %q, want %q", KindHostGoCacheTrim, got, RuleHostGoCacheTrim)
	}
	if f, ok := Flags(RuleHostGoCacheTrim); !ok || f != flagsAsk {
		t.Errorf("Flags(%q) = %+v (found %v), want flagsAsk", RuleHostGoCacheTrim, f, ok)
	}
	if got := RuleOwner(RuleHostGoCacheTrim); got != OwnerOverseer {
		t.Errorf("RuleOwner(%q) = %q, want %q", RuleHostGoCacheTrim, got, OwnerOverseer)
	}
}

// TestGoCacheTrimItems is the freshness arithmetic (gt-a0sewj).
func TestGoCacheTrimItems(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		failure  string
		lastGood time.Time
		summary  string
	}{
		{"failure", "exit 1", time.Time{}, "Go cache trim failed: exit 1; the next go command may pay a full trim"},
		{"failure overrides a fresh trim", "timed out", now.Add(-time.Hour), "Go cache trim failed: timed out; the next go command may pay a full trim"},
		{"no trim has completed", "", time.Time{}, "no Go cache trim has completed; the next go command may pay a full trim"},
		{"stale", "", now.Add(-13 * time.Hour), "last Go cache trim completed 13h0m0s ago; the next go command may pay a full trim"},
		{"just over 12 hours", "", now.Add(-12*time.Hour - time.Minute), "last Go cache trim completed 12h1m0s ago; the next go command may pay a full trim"},
		{"exactly 12 hours", "", now.Add(-12 * time.Hour), ""},
		{"newer", "", now.Add(-time.Hour), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			items := GoCacheTrimItems(tc.failure, tc.lastGood, now)
			if tc.summary == "" {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none", items)
				}
				return
			}
			if len(items) != 1 {
				t.Fatalf("items = %+v, want one", items)
			}
			it := items[0]
			if it.Kind != KindHostGoCacheTrim || it.Severity != SeverityLow || it.Key != RuleHostGoCacheTrim {
				t.Errorf("item = %+v, want a low %q item keyed %q", it, KindHostGoCacheTrim, RuleHostGoCacheTrim)
			}
			if it.Summary != tc.summary {
				t.Errorf("Summary = %q, want %q", it.Summary, tc.summary)
			}
		})
	}
}
