package attention

import (
	"strings"
	"testing"
	"time"
)

// TestHostForgejoBackupTables pins the judge's kind, rule, flags and owner (gt-q8nbzp).
func TestHostForgejoBackupTables(t *testing.T) {
	t.Parallel()

	listed := false
	for _, k := range Kinds() {
		listed = listed || k == KindHostForgejoBackup
	}
	if !listed {
		t.Errorf("Kinds() does not list %q", KindHostForgejoBackup)
	}
	if got := RuleFor(Item{Kind: KindHostForgejoBackup}); got != RuleHostForgejoBackup {
		t.Errorf("RuleFor(%q) = %q, want %q", KindHostForgejoBackup, got, RuleHostForgejoBackup)
	}
	if f, ok := Flags(RuleHostForgejoBackup); !ok || f != flagsAsk {
		t.Errorf("Flags(%q) = %+v (found %v), want flagsAsk", RuleHostForgejoBackup, f, ok)
	}
	if got := RuleOwner(RuleHostForgejoBackup); got != OwnerOverseer {
		t.Errorf("RuleOwner(%q) = %q, want %q", RuleHostForgejoBackup, got, OwnerOverseer)
	}
}

// TestForgejoBackupItems is the freshness arithmetic (gt-q8nbzp).
func TestForgejoBackupItems(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		failure  string
		lastGood time.Time
		carries  string
		raises   bool
	}{
		{"failure", "tar exited 1", time.Time{}, "tar exited 1", true},
		{"failure overrides a fresh dump", "dump aborted", now.Add(-time.Hour), "dump aborted", true},
		{"no dump has completed", "", time.Time{}, "no verified Forgejo backup", true},
		{"stale", "", now.Add(-27 * time.Hour), "27h0m0s", true},
		{"just over 26 hours", "", now.Add(-26*time.Hour - time.Minute), "26h1m0s", true},
		{"exactly 26 hours", "", now.Add(-26 * time.Hour), "", false},
		{"fresh", "", now.Add(-time.Hour), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			items := ForgejoBackupItems(tc.failure, tc.lastGood, now)
			if !tc.raises {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none", items)
				}
				return
			}
			if len(items) != 1 {
				t.Fatalf("items = %+v, want one", items)
			}
			it := items[0]
			if it.Key != RuleHostForgejoBackup || it.Kind != KindHostForgejoBackup || it.Severity != SeverityMedium {
				t.Errorf("item = %+v, want a medium %q item keyed %q", it, KindHostForgejoBackup, RuleHostForgejoBackup)
			}
			if !strings.Contains(it.Summary, tc.carries) {
				t.Errorf("Summary = %q, want it to carry %q", it.Summary, tc.carries)
			}
		})
	}
}
