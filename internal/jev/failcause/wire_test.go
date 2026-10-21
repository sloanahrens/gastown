package failcause

import (
	"path/filepath"
	"strings"
	"testing"
)

// The cause log lives under the town's runtime directory, beside the other
// batteries' logs.
func TestFailcauseLogIsUnderTheTown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	want := filepath.Join(town, ".runtime", "jev", "failcause.jsonl")
	if got := LogPath(town); got != want {
		t.Errorf("LogPath = %s, want %s", got, want)
	}
}

// A price is required: the ledger stores tokens, and the battery cannot price
// them without one.
func TestFailcauseOpenNeedsAPrice(t *testing.T) {
	t.Parallel()
	_, err := Open(t.TempDir(), Settings{}, nil)
	if err == nil || !strings.Contains(err.Error(), "usd_per_billion_tokens") {
		t.Errorf("Open with no price = %v, want the missing price named", err)
	}
}

// A cap that could never allow a call is refused when the battery opens.
func TestFailcauseOpenRefusesABadCap(t *testing.T) {
	t.Parallel()
	_, err := Open(t.TempDir(), Settings{MaxUSDPerDay: -1, USDPerBillionTokens: 3}, nil)
	if err == nil {
		t.Error("Open with a negative cap succeeded, want an error")
	}
}
