package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

func commitRateCheck(counts []doltserver.DBCommits, err error) *DoltCommitRateCheck {
	c := NewDoltCommitRateCheck()
	c.measure = func(context.Context, string) ([]doltserver.DBCommits, error) { return counts, err }
	return c
}

// The default threshold (500) warns on the databases above it and names them.
func TestDoltCommitRateCheck_WarnsAboveDefault(t *testing.T) {
	t.Parallel()

	c := commitRateCheck([]doltserver.DBCommits{
		{Database: "be", Commits: 17},
		{Database: "gt", Commits: 3048},
		{Database: "hq", Commits: 2041},
	}, nil)
	r := c.Run(&CheckContext{TownRoot: t.TempDir()})
	if r.Status != StatusWarning {
		t.Fatalf("status = %v, want warning: %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "gt=3048 hq=2041") || strings.Contains(r.Message, "be=") {
		t.Errorf("message = %q, want gt and hq only, busiest first", r.Message)
	}
}

// A town that raised operational.dolt.commits_per_day_warn is within it.
func TestDoltCommitRateCheck_ConfiguredThreshold(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"type": "town-settings", "version": 1, "operational": {"dolt": {"commits_per_day_warn": 5000}}}`
	if err := os.WriteFile(filepath.Join(townRoot, "settings", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	c := commitRateCheck([]doltserver.DBCommits{{Database: "gt", Commits: 3048}}, nil)
	r := c.Run(&CheckContext{TownRoot: townRoot})
	if r.Status != StatusOK || !strings.Contains(r.Message, "within 5000") {
		t.Errorf("status = %v, message = %q; want OK within 5000", r.Status, r.Message)
	}
}

// A meter that cannot be read is Skipped, never OK.
func TestDoltCommitRateCheck_UnreadableIsSkipped(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]*DoltCommitRateCheck{
		"server":       commitRateCheck(nil, errors.New("connection refused")),
		"no databases": commitRateCheck(nil, nil),
		"every query":  commitRateCheck([]doltserver.DBCommits{{Database: "gt", Err: "timeout"}}, nil),
	} {
		if r := c.Run(&CheckContext{TownRoot: t.TempDir()}); r.Status != StatusSkipped {
			t.Errorf("%s: status = %v, want skipped: %s", name, r.Status, r.Message)
		}
	}
}
