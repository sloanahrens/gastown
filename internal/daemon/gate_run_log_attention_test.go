package daemon

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
)

// gateRunLogSources is a daemon whose gate-run path cannot be written (a path under
// a device file, so the append fails for real) and the sources that read it. It
// touches no directory of its own.
func gateRunLogSources() *attentionSources {
	d := &Daemon{config: &Config{TownRoot: "/dev/null/town"}, logger: log.New(io.Discard, "", 0)}
	return &attentionSources{d: d, now: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
}

// TestGateRunLogAttention: a failing AppendGateRun raises one gate-run-log item
// naming the error, the next success clears it, and a second failure with another
// error keeps one item under the same key (gt-nwea8n). The failures are real
// appends through the recorder, to paths under a device file; a success is the
// recorder's report(nil), which TestGateRunRecorderAppends shows a real append makes.
func TestGateRunLogAttention(t *testing.T) {
	t.Parallel()
	exec := func(context.Context, ...string) (string, error) { return "", nil }
	const ok = ""
	for _, tc := range []struct {
		name  string
		roots []string // each recorded run appends to the next root; ok is a success
		want  int      // items after the last run
	}{
		{"nothing recorded raises nothing", nil, 0},
		{"a failing append raises one item", []string{"/dev/null/a"}, 1},
		{"the next success clears it", []string{"/dev/null/a", ok}, 0},
		{"a second failure with another error keeps one item and its key", []string{"/dev/null/a", "/dev/null/b"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := gateRunLogSources()
			c := collectorFor(t, src, attention.KindGateRunLog)
			var keys, summaries []string
			for _, root := range tc.roots {
				if root == ok {
					src.d.gateRunLog.record(nil)
				} else {
					NewGateRunRecorder(exec, root, func(string, ...any) {}, src.d.gateRunLog.record)(land.GateRun{Bead: "gt-a"})
				}
				items, err := c.collect(context.Background())
				if err != nil {
					t.Fatalf("collector: %v", err)
				}
				for _, it := range items {
					keys, summaries = append(keys, it.Key), append(summaries, it.Summary)
				}
			}
			items, err := c.collect(context.Background())
			if err != nil || len(items) != tc.want {
				t.Fatalf("collector = %+v, %v; want %d item(s)", items, err, tc.want)
			}
			if tc.want == 1 && (items[0].Key != "gate-run-log" || items[0].Severity != attention.SeverityMedium || items[0].Summary == "") {
				t.Errorf("item = %+v, want key gate-run-log, medium severity and a summary naming the error", items[0])
			}
			for _, k := range keys {
				if k != "gate-run-log" {
					t.Errorf("an item was raised under the key %q, want gate-run-log whatever the error", k)
				}
			}
			if len(tc.roots) == 2 && tc.want == 1 && len(summaries) == 2 && summaries[0] == summaries[1] {
				t.Errorf("the two failures raised the same summary %q, want each to name its own error", summaries[0])
			}
		})
	}
}

// TestGateRunLogAttentionThroughTheProductionLanding: the recorder the production
// landing builds (newForgejoLanding) reports to the daemon's own failure state, so
// one append that fails raises the gate-run-log item through the registered
// collector; a landing that dropped the report argument would log the failure and
// raise nothing, which looks like a healthy log (gt-nwea8n).
func TestGateRunLogAttentionThroughTheProductionLanding(t *testing.T) {
	t.Parallel()
	src := gateRunLogSources()
	d := src.d
	// The one directory this test needs is the token directory: the Forgejo client
	// reads the landing bot's token from a mode 600 file, which the landing builds.
	tokenDir := gateRunRecorderRoot(t)
	if err := os.WriteFile(filepath.Join(tokenDir, "forgejo-"+config.ForgejoRoleLanding+".env"), []byte("FORGEJO_TOKEN=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fj := &config.ForgejoConfig{
		RemoteURL: "https://forgejo.example/gastown/gastown",
		Bots:      map[string]string{config.ForgejoRoleLanding: "gt-landing"},
	}
	gate, _, err := d.newForgejoLanding("gastown", "origin", fj, "/nonexistent/repo", &land.LandingsFile{Path: filepath.Join(tokenDir, "landings.jsonl")}, "/nonexistent/gate-logs",
		&config.LandingWorkerConfig{Forgejo: &config.ForgejoWorkerConfig{TokenDir: tokenDir}})
	if err != nil {
		t.Fatalf("newForgejoLanding: %v", err)
	}
	gate.Recorder(land.GateRun{Bead: "gt-a"})
	items, err := collectorFor(t, src, attention.KindGateRunLog).collect(context.Background())
	if err != nil || len(items) != 1 {
		t.Fatalf("collector = %+v, %v; want one item raised through the production landing", items, err)
	}
}
