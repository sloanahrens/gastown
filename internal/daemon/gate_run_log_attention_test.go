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

// TestGateRunLogAttention: a failing AppendGateRun raises one gate-run-log item
// naming the error, the next success clears it, and a second failure keeps one
// item with the latest error (gt-nwea8n).
func TestGateRunLogAttention(t *testing.T) {
	t.Parallel()
	blocker := filepath.Join(gateRunRecorderRoot(t), "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	failing := filepath.Join(blocker, "town") // a path under a file: the append fails
	exec := func(context.Context, ...string) (string, error) { return "", nil }
	for _, tc := range []struct {
		name  string
		roots []string // each recorded run appends to the next root
		want  int      // items after the last run
	}{
		{"nothing recorded raises nothing", nil, 0},
		{"a failing append raises one item", []string{failing}, 1},
		{"the next success clears it", []string{failing, ""}, 0},
		{"a second failure keeps one item", []string{failing, failing}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC))
			for _, root := range tc.roots {
				if root == "" {
					root = gateRunRecorderRoot(t)
				}
				NewGateRunRecorder(exec, root, func(string, ...any) {}, f.src.d.gateRunLog.record)(land.GateRun{Bead: "gt-a"})
			}
			c := collectorFor(t, f.src, attention.KindGateRunLog)
			items, err := c.collect(context.Background())
			if err != nil || len(items) != tc.want {
				t.Fatalf("collector = %+v, %v; want %d item(s)", items, err, tc.want)
			}
			if tc.want == 1 && (items[0].Key != "gate-run-log" || items[0].Severity != attention.SeverityMedium || items[0].Summary == "") {
				t.Errorf("item = %+v, want key gate-run-log, medium severity and a summary naming the error", items[0])
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
	f := newAttentionFixture(t, time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC))
	d := f.src.d
	blocker := filepath.Join(gateRunRecorderRoot(t), "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d.config.TownRoot = filepath.Join(blocker, "town") // a path under a file: the append fails
	d.logger = log.New(io.Discard, "", 0)
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
	items, err := collectorFor(t, f.src, attention.KindGateRunLog).collect(context.Background())
	if err != nil || len(items) != 1 {
		t.Fatalf("collector = %+v, %v; want one item raised through the production landing", items, err)
	}
}
