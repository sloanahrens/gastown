package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/hostprobe"
)

// gocacheSource is the sources a tick hands the collector, its host publishing
// field as a Mac's host agent does.
func gocacheSource(t *testing.T, now time.Time, field *hostprobe.Field[hostprobe.GoCacheTrim]) *attentionSources {
	t.Helper()
	p := newFileHostProbes(t.TempDir(), nil, nil)
	p.now = func() time.Time { return now }
	writeFileProbes(t, p, hostprobe.Probes{At: now, GoCacheTrim: field})
	return &attentionSources{host: p, now: now}
}

// gocacheCollect runs the table entry for the kind, so a dropped entry fails here.
func gocacheCollect(t *testing.T, s *attentionSources) []attention.Item {
	t.Helper()
	for _, c := range s.collectors() {
		if c.kind == attention.KindHostGoCacheTrim && c.collect != nil {
			items, err := c.collect(t.Context())
			if err != nil {
				t.Fatalf("collector: %v", err)
			}
			return items
		}
	}
	t.Fatal("no host-gocache-trim collector in collectors()")
	return nil
}

// A published reading decides the rule: a stale, failed or absent trim raises
// the item; a fresh one, or a file without the field, raises none.
func TestGoCacheTrimCollectReadsTheProbeFile(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name  string
		field *hostprobe.Field[hostprobe.GoCacheTrim]
		text  string // "" wants no item
	}{
		{"stale", &hostprobe.Field[hostprobe.GoCacheTrim]{Value: hostprobe.GoCacheTrim{LastGood: now.Add(-13 * time.Hour)}}, "13h0m0s ago"},
		{"fresh", &hostprobe.Field[hostprobe.GoCacheTrim]{Value: hostprobe.GoCacheTrim{LastGood: now.Add(-time.Hour)}}, ""},
		{"no trim", &hostprobe.Field[hostprobe.GoCacheTrim]{}, "no Go cache trim has completed"},
		{"failed", &hostprobe.Field[hostprobe.GoCacheTrim]{Err: "exit 1"}, "Go cache trim failed: exit 1"},
		{"not published", nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			items := gocacheCollect(t, gocacheSource(t, now, tt.field))
			if tt.text == "" {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none", items)
				}
				return
			}
			if len(items) != 1 || items[0].Key != attention.RuleHostGoCacheTrim || items[0].Kind != attention.KindHostGoCacheTrim || items[0].Severity != attention.SeverityLow {
				t.Fatalf("items = %+v, want one low %q item", items, attention.RuleHostGoCacheTrim)
			}
			if !strings.Contains(items[0].Summary, tt.text) {
				t.Errorf("Summary = %q, want %q", items[0].Summary, tt.text)
			}
		})
	}
}

// A refused file raises the probe item and keeps the rule item an earlier tick raised.
func TestGoCacheTrimCollectRefusedFileKeepsTheRule(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	s := gocacheSource(t, now, nil)
	if err := os.Remove(s.host.(fileHostProbes).path); err != nil {
		t.Fatal(err)
	}
	s.prev = attention.State{Items: []attention.Item{{Key: attention.RuleHostGoCacheTrim, Kind: attention.KindHostGoCacheTrim, Summary: "held"}}}
	st := attention.State{Items: gocacheCollect(t, s)}
	probe, ok := attention.Find(st, hostProbeFailedPrefix+hostProbeGoCache)
	if !ok || probe.Kind != attention.KindHostProbe || probe.Severity != attention.SeverityLow {
		t.Fatalf("probe item = %+v (found %v), want a low host-probe item", probe, ok)
	}
	if !strings.Contains(probe.Summary, errProbeFileRefused.Error()) {
		t.Errorf("probe summary = %q, want the refusal named", probe.Summary)
	}
	if kept, ok := attention.Find(st, attention.RuleHostGoCacheTrim); len(st.Items) != 2 || !ok || kept.Summary != "held" {
		t.Errorf("items = %+v, want the held rule beside the probe item", st.Items)
	}
}

// A host without the read raises nothing: not publishing is nothing to say, not a failure.
func TestGoCacheTrimCollectNoSourceRaisesNothing(t *testing.T) {
	t.Parallel()
	if items := gocacheCollect(t, &attentionSources{host: healthyHost()}); len(items) != 0 {
		t.Fatalf("items = %+v, want none from a host with no reading", items)
	}
}
