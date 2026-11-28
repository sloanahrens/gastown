package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/hostprobe"
)

// forgejoBackupSource is the sources a tick hands the collector, its host publishing field.
func forgejoBackupSource(t *testing.T, now time.Time, field *hostprobe.Field[hostprobe.ForgejoBackup]) *attentionSources {
	t.Helper()
	p := newFileHostProbes(t.TempDir(), nil, nil)
	p.now = func() time.Time { return now }
	writeFileProbes(t, p, hostprobe.Probes{At: now, ForgejoBackup: field})
	return &attentionSources{host: p, now: now}
}

// forgejoBackupCollect runs the table entry for the kind, so a dropped entry fails here.
func forgejoBackupCollect(t *testing.T, s *attentionSources) []attention.Item {
	t.Helper()
	for _, c := range s.collectors() {
		if c.kind == attention.KindHostForgejoBackup && c.collect != nil {
			items, err := c.collect(t.Context())
			if err != nil {
				t.Fatalf("collector: %v", err)
			}
			return items
		}
	}
	t.Fatal("no host-forgejo-backup collector in collectors()")
	return nil
}

// TestHostForgejoBackupCollectReadsTheProbeFile: a published reading decides
// the rule: a failed or stale dump raises the item; a fresh one, or no field, raises none.
func TestHostForgejoBackupCollectReadsTheProbeFile(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name  string
		field *hostprobe.Field[hostprobe.ForgejoBackup]
		text  string // "" wants no item
	}{
		{"failed", &hostprobe.Field[hostprobe.ForgejoBackup]{Err: "dump aborted"}, "Forgejo backup failed: dump aborted"},
		{"stale", &hostprobe.Field[hostprobe.ForgejoBackup]{Value: hostprobe.ForgejoBackup{LastGood: now.Add(-27 * time.Hour)}}, "27h0m0s old"},
		{"fresh", &hostprobe.Field[hostprobe.ForgejoBackup]{Value: hostprobe.ForgejoBackup{LastGood: now.Add(-time.Hour)}}, ""},
		{"not published", nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			items := forgejoBackupCollect(t, forgejoBackupSource(t, now, tt.field))
			if tt.text == "" {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none", items)
				}
				return
			}
			if len(items) != 1 || items[0].Key != attention.RuleHostForgejoBackup || items[0].Kind != attention.KindHostForgejoBackup || items[0].Severity != attention.SeverityMedium {
				t.Fatalf("items = %+v, want one medium %q item", items, attention.RuleHostForgejoBackup)
			}
			if !strings.Contains(items[0].Summary, tt.text) {
				t.Errorf("Summary = %q, want %q", items[0].Summary, tt.text)
			}
		})
	}
}

// TestHostForgejoBackupCollectRefusedFileKeepsTheItem: a refused file raises
// the probe item and keeps the backup item an earlier tick raised.
func TestHostForgejoBackupCollectRefusedFileKeepsTheItem(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	s := forgejoBackupSource(t, now, nil)
	if err := os.Remove(s.host.(fileHostProbes).path); err != nil {
		t.Fatal(err)
	}
	s.prev = attention.State{Items: []attention.Item{{Key: attention.RuleHostForgejoBackup, Kind: attention.KindHostForgejoBackup, Summary: "held"}}}
	st := attention.State{Items: forgejoBackupCollect(t, s)}
	probe, ok := attention.Find(st, hostProbeFailedPrefix+hostProbeForgejoBackup)
	if !ok || probe.Kind != attention.KindHostProbe || probe.Severity != attention.SeverityLow {
		t.Fatalf("probe item = %+v (found %v), want a low host-probe item", probe, ok)
	}
	if !strings.Contains(probe.Summary, errProbeFileRefused.Error()) {
		t.Errorf("probe summary = %q, want the refusal named", probe.Summary)
	}
	if kept, ok := attention.Find(st, attention.RuleHostForgejoBackup); len(st.Items) != 2 || !ok || kept.Summary != "held" {
		t.Errorf("items = %+v, want the held rule beside the probe item", st.Items)
	}
}

// TestHostForgejoBackupCollectOutsideContainerMode: the host outside container
// mode cannot read the dump state, so the registered collector raises the probe
// item naming why and keeps the backup item an earlier tick raised.
func TestHostForgejoBackupCollectOutsideContainerMode(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	host := attentionHostProbes(t.TempDir(), func(string) string { return "" }, nil, nil)
	s := &attentionSources{host: host, now: now}
	s.prev = attention.State{Items: []attention.Item{{Key: attention.RuleHostForgejoBackup, Kind: attention.KindHostForgejoBackup, Summary: "held"}}}
	st := attention.State{Items: forgejoBackupCollect(t, s)}
	probe, ok := attention.Find(st, hostProbeFailedPrefix+hostProbeForgejoBackup)
	if !ok || probe.Kind != attention.KindHostProbe || probe.Severity != attention.SeverityLow {
		t.Fatalf("probe item = %+v (found %v), want a low host-probe item", probe, ok)
	}
	if !strings.Contains(probe.Summary, errForgejoBackupNotPublished.Error()) {
		t.Errorf("probe summary = %q, want the sentinel named", probe.Summary)
	}
	if kept, ok := attention.Find(st, attention.RuleHostForgejoBackup); len(st.Items) != 2 || !ok || kept.Summary != "held" {
		t.Errorf("items = %+v, want the held rule beside the probe item", st.Items)
	}
}
