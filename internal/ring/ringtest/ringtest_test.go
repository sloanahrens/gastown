package ringtest_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/ring"
	"github.com/steveyegge/gastown/internal/ring/ringtest"
)

var now = time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC)

// TestMintLeavesTheStateItNames: each helper mints a record into a directory
// that is not a town yet, and Read of that town reports the state the helper
// promised.
func TestMintLeavesTheStateItNames(t *testing.T) {
	t.Parallel()
	key := []byte("a town's signing key")
	cases := []struct {
		name   string
		mint   func(t testing.TB, townRoot string, key []byte, now time.Time) ring.Record
		kind   ring.Kind
		reason string
	}{
		{"valid", ringtest.Valid, ring.Valid, ""},
		{"waned", ringtest.Waned, ring.Waned, ring.ReasonExpired},
		{"tampered", ringtest.Tampered, ring.Absent, ring.ReasonBadSignature},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			rec := tc.mint(t, root, key, now)
			if rec.HMAC == "" {
				t.Fatal("the minted record carries no signature")
			}
			st, err := ring.Read(root, key, now)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if st.Kind != tc.kind || st.Reason != tc.reason {
				t.Fatalf("state = %v, want %s (%s)", st, tc.kind, tc.reason)
			}
		})
	}
}

// TestMintKeepsAnExistingTown: minting into a town that is already there adds
// the ring section and leaves the town's identity alone.
func TestMintKeepsAnExistingTown(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, filepath.FromSlash(config.MachineConfigFile))
	if err := config.WriteConfigJSON(path, &config.TownConfig{
		Type: "town", Version: config.CurrentTownVersion, Name: "gastown",
	}, 0o600); err != nil {
		t.Fatal(err)
	}
	ringtest.Waned(t, root, []byte("k"), now)
	town, err := config.LoadTownConfig(path)
	if err != nil {
		t.Fatalf("LoadTownConfig: %v", err)
	}
	if town.Name != "gastown" {
		t.Errorf("town name after minting = %q, want %q", town.Name, "gastown")
	}
	st, err := ring.Read(root, []byte("k"), now)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if st.Kind != ring.Waned {
		t.Fatalf("state = %v, want waned", st)
	}
}
