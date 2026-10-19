package daemon

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// checkSeatMap is the daemon's start-time read of polecat_pool.seats: a map the
// loader rejects stops the start, and a town with no seats key starts as before.
func TestCheckSeatMapAtStart(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		settings string // empty: no settings file
		wantErr  string // empty: the start goes ahead
	}{
		{name: "no settings file"},
		{name: "no seats key", settings: `{"type":"town-settings","version":1,"polecat_pool":{"min_spawn_gap":"4m"}}`},
		{
			name:     "a retired two-seat key",
			settings: `{"type":"town-settings","version":1,"polecat_pool":{"agent":"deepseek-flash","max_seats":3}}`,
			wantErr:  "polecat_pool.agent was replaced by",
		},
		{name: "valid seat map", settings: `{"type":"town-settings","version":1,"polecat_pool":{"seats":{"claude-sonnet":{"cap":3},"claude-haiku":{"cap":2}}}}`},
		{
			name:     "unknown agent",
			settings: `{"type":"town-settings","version":1,"polecat_pool":{"seats":{"nope":{"cap":1}}}}`,
			wantErr:  "polecat_pool.seats.nope",
		},
		{
			name:     "closed window without a timezone",
			settings: `{"type":"town-settings","version":1,"polecat_pool":{"seats":{"claude-sonnet":{"cap":1,"closed":"20:00-08:00"}}}}`,
			wantErr:  "polecat_pool.timezone",
		},
		{
			name:     "key the strict decoder refuses",
			settings: `{"type":"town-settings","version":1,"polecat_pool":{"seats":{"claude-sonnet":{"cap":1,"cpa":2}}}}`,
			wantErr:  "cpa",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.settings != "" {
				path := filepath.Join(town, "settings", "config.json")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tc.settings), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := checkSeatMap(town)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkSeatMap = %v, want the start to go ahead", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("checkSeatMap = %v, want an error naming %q", err, tc.wantErr)
			}
		})
	}
}

// A helper that refuses proves nothing about the start unless Run calls it: a
// daemon that never reaches the check starts on a map the operator cannot have
// meant, and no test above notices (gt-uo9u3).
func TestRunRefusesStartOnInvalidSeatMap(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(town, "settings", "config.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"type":"town-settings","version":1,"polecat_pool":{"seats":{"nope":{"cap":1}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig(town)
	d := &Daemon{config: cfg, logger: log.New(io.Discard, "", 0)}
	// A start that got past the check would run the whole loop; a canceled
	// context ends it at the first select instead of parking the test.
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.cancel()

	bounded, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run() }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "polecat_pool.seats.nope") {
			t.Fatalf("Run = %v, want a refusal naming the unknown seat", err)
		}
	case <-bounded.Done():
		t.Fatal("Run did not return: the start-time seat check is not wired into Run")
	}
	if _, err := os.Stat(cfg.PidFile); !os.IsNotExist(err) {
		t.Errorf("Run wrote %s before refusing; the seat check must come before startup side effects", cfg.PidFile)
	}
}
