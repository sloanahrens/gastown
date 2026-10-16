package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// The plan meter's flag defaults to the price the status line's percentages are
// drawn against, and the reader the command wires reads the town's own series.
func TestDashboardClaudePlanWiring(t *testing.T) {
	t.Parallel()

	f := dashboardCmd.Flags().Lookup("claude-plan-usd")
	if f == nil {
		t.Fatal("gt dashboard has no --claude-plan-usd flag")
	}
	if want := fmt.Sprint(dashboard.ClaudePlanDefaultUSD); f.DefValue != want {
		t.Errorf("--claude-plan-usd defaults to %q, want %q", f.DefValue, want)
	}

	root := t.TempDir()
	path := dashboardClaudePlanPath(root)
	if want := filepath.Join(root, ".runtime", "claude-plan", "samples.jsonl"); path != want {
		t.Errorf("the plan series is read from %q, want %q", path, want)
	}

	// One line, as the status line appends it.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	line := fmt.Sprintf(`{"t":%d,"five_hour":8,"five_hour_resets_at":%d,"seven_day":38,"seven_day_resets_at":%d}`,
		now.Unix(), now.Add(time.Hour).Unix(), now.Add(48*time.Hour).Unix())
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := dashboard.NewClaudePlanReader(path, dashboardClaudePlanUSD).Read(now)

	if p.WeeklyPct != 38 || p.Stale || p.Unavailable {
		t.Errorf("the reader wired to the town's series read %+v, want a fresh 38%%", p)
	}
	if p.UsedValueUSD <= 0 {
		t.Errorf("the flag's price did not reach the reading: %+v", p)
	}
}
