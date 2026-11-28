package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/formula"
)

// formulaDriftRig is the shared fixture with the plan read replaced.
func formulaDriftRig(t *testing.T, read func(string) (*formula.SyncPlan, error)) *attentionFixture {
	t.Helper()
	f := newAttentionFixture(t, time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC))
	f.src.formulasStalePlan = read
	return f
}

// TestFormulaDriftRaisesOnlyChangedFormulas: a changed formula raises one item; unchanged, orphaned and unowned files alone raise none (gt-u661sv).
func TestFormulaDriftRaisesOnlyChangedFormulas(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		entries []formula.SyncEntry
		raise   bool
	}{
		{"outdated", []formula.SyncEntry{
			{Name: "mol-polecat-work.formula.toml", Action: formula.SyncUpdate},
			{Name: "tdd-cycle.formula.toml", Action: formula.SyncUpToDate},
		}, true},
		{"up-to-date, orphaned and unowned", []formula.SyncEntry{
			{Name: "mol-polecat-work.formula.toml", Action: formula.SyncUpToDate},
			{Name: "gone.formula.toml", Action: formula.SyncOrphaned},
			{Name: "hand.formula.toml", Action: formula.SyncUnowned},
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := formulaDriftRig(t, func(string) (*formula.SyncPlan, error) {
				return &formula.SyncPlan{Entries: tc.entries}, nil
			})
			items, err := collectorFor(t, f.src, attention.KindFormulasStale).collect(context.Background())
			if err != nil {
				t.Fatalf("collect: %v", err)
			}
			if !tc.raise {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none", items)
				}
				return
			}
			if len(items) != 1 || items[0].Key != formulasStaleKey || items[0].Kind != attention.KindFormulasStale {
				t.Fatalf("items = %+v, want one key %q kind %q", items, formulasStaleKey, attention.KindFormulasStale)
			}
			for _, want := range []string{"mol-polecat-work.formula.toml", "gt formula sync"} {
				if !strings.Contains(items[0].Summary, want) {
					t.Errorf("Summary = %q, want %q", items[0].Summary, want)
				}
			}
		})
	}
}

// TestFormulaDriftReturnsTheReadError: a failed plan read builds no item and is returned (gt-u661sv).
func TestFormulaDriftReturnsTheReadError(t *testing.T) {
	t.Parallel()
	f := formulaDriftRig(t, func(string) (*formula.SyncPlan, error) { return nil, errors.New("unreadable") })
	if items, err := f.src.collectFormulasStale(context.Background()); err == nil || len(items) != 0 {
		t.Fatalf("items = %+v err = %v, want the read error and no item", items, err)
	}
}

// TestFormulaDriftNilSeamReadsTheTownRoot: with no seam the collector plans the
// daemon's own town, naming only its hand-edited formula (another root would
// name every formula as an install) (gt-u661sv).
func TestFormulaDriftNilSeamReadsTheTownRoot(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if _, err := formula.ProvisionFormulas(town); err != nil {
		t.Fatalf("provisioning: %v", err)
	}
	drifted := filepath.Join(town, ".beads", "formulas", "mol-polecat-work.formula.toml")
	if err := os.WriteFile(drifted, []byte("hand edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := &attentionSources{d: &Daemon{config: &Config{TownRoot: town}}}
	items, err := src.collectFormulasStale(context.Background())
	if err != nil || len(items) != 1 || !strings.Contains(items[0].Summary, "mol-polecat-work.formula.toml") ||
		strings.Contains(items[0].Summary, "tdd-cycle.formula.toml") {
		t.Fatalf("items = %+v err = %v, want one naming only the town root's drift", items, err)
	}
	// A daemon's town with no formulas directory is drift: every embedded
	// formula is an install the sync would write (gt-u661sv).
	empty := &attentionSources{d: &Daemon{config: &Config{TownRoot: t.TempDir()}}}
	items, err = empty.collectFormulasStale(context.Background())
	if err != nil || len(items) != 1 || !strings.Contains(items[0].Summary, "mol-polecat-work.formula.toml") {
		t.Fatalf("items = %+v err = %v, want one naming an embedded formula for a town with no formulas", items, err)
	}
	// A town whose .beads is a file cannot be planned: the read's error is returned.
	notDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(notDir, ".beads"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := &attentionSources{d: &Daemon{config: &Config{TownRoot: notDir}}}
	if items, err := bad.collectFormulasStale(context.Background()); err == nil || len(items) != 0 {
		t.Fatalf("items = %+v err = %v, want the read error and no item", items, err)
	}
}
