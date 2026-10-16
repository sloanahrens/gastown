package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// The raw dependency query authenticates with the town's configured Dolt
// password; a DSN that drops it made a password-protected server answer access
// denied, and depListRawIDs fell back to the slower bd path (gt-napz3).
func TestDepListRawDSNCarriesPassword(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := `{"type":"town-settings","version":1,"operational":{"dolt":{"password":"secret"}}}`
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "settings", "config.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	dsn := depListRawDSN(depListRawUserInfo(beadsDir), "127.0.0.1", 3307, "hq")
	if want := "root:secret@tcp(127.0.0.1:3307)/hq"; !strings.HasPrefix(dsn, want) {
		t.Errorf("depListRawDSN = %q, want prefix %q", dsn, want)
	}
}

// A dir outside any town has no Dolt config to read, so the raw query connects
// as the default user with no password (gt-sktin).
func TestDepListRawUserInfoOutsideTownIsDefaultUser(t *testing.T) {
	t.Parallel()
	if got := depListRawUserInfo(t.TempDir()); got != doltserver.DefaultUser {
		t.Errorf("depListRawUserInfo outside a town = %q, want %q", got, doltserver.DefaultUser)
	}
}

// A town whose Dolt config sets no password connects as the bare user (gt-sktin).
func TestDepListRawUserInfoWithoutPassword(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := depListRawUserInfo(townRoot); got != doltserver.DefaultUser {
		t.Errorf("depListRawUserInfo without a password = %q, want %q", got, doltserver.DefaultUser)
	}
}
