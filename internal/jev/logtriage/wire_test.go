package logtriage

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/jev/battery"
)

// The pass a town runs reads the daemon log under the town and writes its log
// and cursor under the town's runtime directory.
func TestJevLogtriageWirePassUnderTheTown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f := &fakeJev{body: jevReply}
	pass := wirePass(town, battery.Cap{MaxUSDPerDay: 1, USDPerBillionTokens: 3}, newJevClient(t, f))

	if want := filepath.Join(town, "daemon", "daemon.log"); pass.DaemonLog != want {
		t.Errorf("DaemonLog = %s, want %s", pass.DaemonLog, want)
	}
	if want := LogPath(town); pass.LogPath != want {
		t.Errorf("LogPath = %s, want %s", pass.LogPath, want)
	}
	if want := CursorPath(town); pass.CursorPath != want {
		t.Errorf("CursorPath = %s, want %s", pass.CursorPath, want)
	}

	// A town with no daemon log yet has nothing to triage, and that is not an
	// error.
	if sum, err := pass.Run(context.Background()); err != nil || sum.Matched != 0 || len(f.requests) != 0 {
		t.Errorf("Run on an empty town = %+v with %d calls, %v; want nothing read and no error", sum, len(f.requests), err)
	}
}

// A price is required: the ledger stores tokens, and a pass cannot price them
// without one.
func TestJevLogtriageOpenNeedsAPrice(t *testing.T) {
	t.Parallel()
	_, err := Open(t.TempDir(), Settings{}, nil)
	if err == nil || !strings.Contains(err.Error(), "usd_per_billion_tokens") {
		t.Errorf("Open with no price = %v, want the missing price named", err)
	}
}

// A cap that could never allow a call is refused when the pass opens.
func TestJevLogtriageOpenRefusesABadCap(t *testing.T) {
	t.Parallel()
	_, err := Open(t.TempDir(), Settings{MaxUSDPerDay: -1, USDPerBillionTokens: 3}, nil)
	if err == nil {
		t.Error("Open with a negative cap succeeded, want an error")
	}
}
