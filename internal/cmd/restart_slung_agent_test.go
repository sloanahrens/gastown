package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/polecat"
)

type recordingStarter struct {
	name string
	opts polecat.SessionStartOptions
}

func (r *recordingStarter) Start(name string, opts polecat.SessionStartOptions) error {
	r.name, r.opts = name, opts
	return nil
}

// TestRestartPolecatOnSlungAgent: a restarted seat runs the agent its sling
// recorded, and a seat whose record names none keeps the role default (gt-rsrnr).
func TestRestartPolecatOnSlungAgent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		slungOn   string // "" = MarkSlung on the role default; "-" = no record at all
		optsAgent string
		want      string
	}{
		{"record names an agent", "deepseek-flash", "", "deepseek-flash"},
		{"record names no agent", "", "", ""},
		{"no record", "-", "", ""},
		{"explicit agent wins over the record", "deepseek-flash", "claude-haiku", "claude-haiku"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			if tt.slungOn != "-" {
				if err := intent.MarkSlung(townRoot, polecatIntentSeat("gastown", "nux"), tt.slungOn, "test", time.Now()); err != nil {
					t.Fatalf("MarkSlung: %v", err)
				}
			}
			s := &recordingStarter{}
			err := restartPolecatOnSlungAgent(s, townRoot, "gastown", "nux", polecat.SessionStartOptions{Agent: tt.optsAgent})
			if err != nil {
				t.Fatalf("restartPolecatOnSlungAgent: %v", err)
			}
			if s.name != "nux" || s.opts.Agent != tt.want {
				t.Errorf("Start(%q, Agent=%q), want nux on %q", s.name, s.opts.Agent, tt.want)
			}
		})
	}
}

// TestRestartPolecatOnSlungAgentUnreadableRecord: a record that cannot be
// parsed must not stop the restart; the seat starts on the role default.
func TestRestartPolecatOnSlungAgentUnreadableRecord(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := polecatIntentSeat("gastown", "nux").Path(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, "{not json")
	s := &recordingStarter{}
	if err := restartPolecatOnSlungAgent(s, townRoot, "gastown", "nux", polecat.SessionStartOptions{}); err != nil {
		t.Fatalf("restartPolecatOnSlungAgent: %v", err)
	}
	if s.opts.Agent != "" {
		t.Errorf("Agent = %q, want the role default", s.opts.Agent)
	}
}
