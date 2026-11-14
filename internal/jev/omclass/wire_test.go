package omclass

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/jev"
)

// A battery with no price could never price a call, so Open refuses it before
// it reads any key.
func TestOMClassOpenNeedsAPrice(t *testing.T) {
	t.Parallel()
	_, err := Open(t.TempDir(), Settings{}, nil)
	if err == nil || !strings.Contains(err.Error(), "price per billion tokens") {
		t.Fatalf("Open with no price = %v, want the price named", err)
	}
}

// A client whose key cannot be read is not opened, and the error says so.
func TestOMClassOpenRefusesAMissingKey(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	_, err := openPass(town, Settings{USDPerBillionTokens: 4}, nil, jev.WithKeyPaths(filepath.Join(town, "jev.env")))
	if err == nil || !strings.Contains(err.Error(), "opening the Jev client") {
		t.Fatalf("openPass with no key = %v, want the client named", err)
	}
}

// Open wires the town's own log, cursor, review log and client.
func TestOMClassOpenWiresTheTownsLogAndCursor(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	keyFile := filepath.Join(town, "jev.env")
	if err := os.WriteFile(keyFile, []byte("TYPESAFE_API_KEY=test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := openPass(town, Settings{USDPerBillionTokens: 4}, nil, jev.WithKeyPaths(keyFile))
	if err != nil {
		t.Fatalf("openPass: %v", err)
	}

	if p.LogPath != LogPath(town) || p.CursorPath != CursorPath(town) {
		t.Errorf("paths = %q, %q; want the town's log and cursor", p.LogPath, p.CursorPath)
	}
	if p.Asker == nil || p.Reviews == nil || p.Allows == nil {
		t.Errorf("pass = %+v, want an asker, a review log and a cap", p)
	}
}
