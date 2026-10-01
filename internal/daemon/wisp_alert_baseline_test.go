package daemon

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/reaper"
)

func TestWispAlertBaselineRoundTrip(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	previous, err := LoadWispAlertBaseline(townRoot)
	if err != nil {
		t.Fatalf("load with no baseline recorded: %v", err)
	}
	if previous != nil {
		t.Fatalf("a town that has never run the reaper has no baseline, got %+v", previous)
	}

	want := reaper.OpenWispSample{OpenWisps: 138, Databases: 2, DryRun: true}
	if err := SaveWispAlertBaseline(townRoot, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadWispAlertBaseline(townRoot)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got == nil || *got != want {
		t.Fatalf("round trip returned %+v, want %+v", got, want)
	}
}

// A corrupt baseline is replaced by the cycle that finds it, not compared
// against: an unreadable reading is the thing the alert would otherwise judge
// the town by (gt-11kyy).
func TestWispAlertBaselineCorruptFileIsAnErrorAndIsReplaced(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := WispAlertBaselinePath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
		t.Fatalf("seed corrupt baseline: %v", err)
	}

	if _, err := LoadWispAlertBaseline(townRoot); err == nil {
		t.Fatal("a corrupt baseline must be reported, not read as an absent one")
	}

	sample := reaper.OpenWispSample{OpenWisps: 42, Databases: 1}
	if err := SaveWispAlertBaseline(townRoot, sample); err != nil {
		t.Fatalf("save over a corrupt baseline: %v", err)
	}
	got, err := LoadWispAlertBaseline(townRoot)
	if err != nil {
		t.Fatalf("load after repair: %v", err)
	}
	if got == nil || *got != sample {
		t.Fatalf("after repair the baseline is %+v, want %+v", got, sample)
	}
}

func TestReportOpenWispAlert(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		previous   *reaper.OpenWispSample
		current    reaper.OpenWispSample
		wantWarned bool
	}{
		{
			name:       "first cycle only records",
			current:    reaper.OpenWispSample{OpenWisps: 900, Databases: 2},
			wantWarned: false,
		},
		{
			name:       "steady state is silent",
			previous:   &reaper.OpenWispSample{OpenWisps: 138, Databases: 2},
			current:    reaper.OpenWispSample{OpenWisps: 138, Databases: 2},
			wantWarned: false,
		},
		{
			name:       "runaway accumulation warns",
			previous:   &reaper.OpenWispSample{OpenWisps: 138, Databases: 2},
			current:    reaper.OpenWispSample{OpenWisps: 900, Databases: 2},
			wantWarned: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			if tt.previous != nil {
				if err := SaveWispAlertBaseline(townRoot, *tt.previous); err != nil {
					t.Fatalf("seed baseline: %v", err)
				}
			}

			var buf strings.Builder
			d := &Daemon{
				logger: log.New(&buf, "", 0),
				config: &Config{TownRoot: townRoot},
			}
			d.reportOpenWispAlert(tt.current)

			warned := strings.Contains(buf.String(), "investigate wisp lifecycle")
			if warned != tt.wantWarned {
				t.Fatalf("warned = %v, want %v; log: %q", warned, tt.wantWarned, buf.String())
			}

			// The cycle is the next one's baseline whether or not it warned.
			// A reaper that only records on an alert has no baseline on the
			// steady cycles that matter.
			recorded, err := LoadWispAlertBaseline(townRoot)
			if err != nil {
				t.Fatalf("load recorded baseline: %v", err)
			}
			if recorded == nil || *recorded != tt.current {
				t.Fatalf("recorded baseline is %+v, want %+v", recorded, tt.current)
			}
		})
	}
}
