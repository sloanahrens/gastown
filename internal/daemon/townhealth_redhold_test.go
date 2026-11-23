package daemon

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// TestHealthSourceRedHold pins Dispatch's RedHold field (gt-5vku77): the
// published kinds in file order, "" with no file, and "unreadable" for a file
// that cannot be read.
func TestHealthSourceRedHold(t *testing.T) {
	t.Parallel()
	redHold := func(t *testing.T, town string) string {
		t.Helper()
		got, err := (&healthSources{d: &Daemon{config: &Config{TownRoot: town}}}).Dispatch()
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		return got.RedHold
	}
	if got := redHold(t, t.TempDir()); got != "" {
		t.Errorf("no file: RedHold = %q, want empty", got)
	}
	held := t.TempDir()
	rec := dispatch.RedHoldRecord{Reasons: []dispatch.RedHoldReason{
		{Kind: dispatch.RedHoldSweepRed}, {Kind: dispatch.RedHoldMainRed},
	}}
	if err := dispatch.WriteRedHold(held, rec); err != nil {
		t.Fatalf("WriteRedHold: %v", err)
	}
	if got := redHold(t, held); got != "sweep-red, main-red" {
		t.Errorf("two reasons: RedHold = %q, want %q", got, "sweep-red, main-red")
	}
	broken := t.TempDir()
	if err := os.MkdirAll(dispatch.RedHoldPath(broken), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if got := redHold(t, broken); got != "unreadable" {
		t.Errorf("unreadable file: RedHold = %q, want %q", got, "unreadable")
	}
}
