package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		{name: "no seats key", settings: `{"type":"town-settings","version":1,"polecat_pool":{"agent":"deepseek-flash","max_seats":3}}`},
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
