package ring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// holdKey is the key the holds below are signed with. holdGetenv resolves
// ${GT_RING_KEY} to it, so no test reads the process environment.
var holdKey = []byte("hold-test-key")

func holdGetenv(name string) string {
	if name == "GT_RING_KEY" {
		return string(holdKey)
	}
	return ""
}

// signedHold is a record signed with holdKey that expires at expires.
func signedHold(t *testing.T, expires time.Time) Record {
	t.Helper()
	rec, err := Sign(Record{ForgedAt: now.Add(-time.Hour), ExpiresAt: expires, ForgedBy: "test"}, holdKey)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// writeSettings writes settings/config.json with the given text, the way a
// town names its signing key (or does not, for "{}").
func writeSettings(t *testing.T, root, text string) {
	t.Helper()
	dir := filepath.Join(root, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// keyedSettings names the signing key as ${GT_RING_KEY}.
const keyedSettings = `{"ring":{"key":"${GT_RING_KEY}"}}`

// TestHolds pins the one rule every reader of the Ring applies: what holds the
// town, why, and what does not. A town with no mayor/town.json is a town with
// no ring section, so it holds nothing.
func TestHolds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// town builds the town root the case reads.
		town func(t *testing.T) string
		// settings is settings/config.json; empty means the file is absent.
		settings string
		getenv   func(string) string
		// want is a substring the reason must carry; "" means no hold.
		want string
	}{
		{
			name: "valid ring holds nothing",
			town: func(t *testing.T) string {
				return townWith(t, signedHold(t, now.Add(Lease)))
			},
			settings: keyedSettings,
			getenv:   holdGetenv,
		},
		{
			name: "waned ring holds and names the wane",
			town: func(t *testing.T) string {
				return townWith(t, signedHold(t, now.Add(-time.Minute)))
			},
			settings: keyedSettings,
			getenv:   holdGetenv,
			want:     "the Ring waned at " + now.Add(-time.Minute).UTC().Format(time.RFC3339),
		},
		{
			name: "waned at the exact expiry",
			town: func(t *testing.T) string {
				return townWith(t, signedHold(t, now))
			},
			settings: keyedSettings,
			getenv:   holdGetenv,
			want:     "the Ring waned at " + now.UTC().Format(time.RFC3339),
		},
		{
			name: "no ring section holds nothing",
			town: func(t *testing.T) string {
				return tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test"})
			},
			settings: keyedSettings,
			getenv:   holdGetenv,
		},
		{
			name:     "no mayor/town.json holds nothing",
			town:     func(t *testing.T) string { return t.TempDir() },
			settings: keyedSettings,
			getenv:   holdGetenv,
		},
		{
			name: "a waned ring holds nothing when settings name no ring.key",
			town: func(t *testing.T) string {
				return townWith(t, signedHold(t, now.Add(-time.Minute)))
			},
			settings: `{}`,
			getenv:   holdGetenv,
		},
		{
			name: "a ring whose key is unresolvable holds with the reason",
			town: func(t *testing.T) string {
				return townWith(t, signedHold(t, now.Add(Lease)))
			},
			settings: keyedSettings,
			getenv:   func(string) string { return "" },
			want:     "the Ring cannot be verified (ring.key names GT_RING_KEY",
		},
		{
			name: "a ring whose settings cannot be read holds with the reason",
			town: func(t *testing.T) string {
				return townWith(t, signedHold(t, now.Add(Lease)))
			},
			settings: `{not json`,
			getenv:   holdGetenv,
			want:     "reading settings/config.json",
		},
		{
			name: "a ring with a bad signature holds with the reason",
			town: func(t *testing.T) string {
				rec := signedHold(t, now.Add(Lease))
				rec.ExpiresAt = now.Add(10 * Lease)
				return townWith(t, rec)
			},
			settings: keyedSettings,
			getenv:   holdGetenv,
			want:     ReasonBadSignature,
		},
		{
			name: "an unsigned ring holds with the reason",
			town: func(t *testing.T) string {
				return tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test",
					"ring": Record{ExpiresAt: now.Add(Lease), ForgedBy: "test"}})
			},
			settings: keyedSettings,
			getenv:   holdGetenv,
			want:     ReasonUnsigned,
		},
		{
			name: "an unparseable ring section holds with the reason",
			town: func(t *testing.T) string {
				return tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test", "ring": "garbage"})
			},
			settings: keyedSettings,
			getenv:   holdGetenv,
			want:     ReasonUnparseable,
		},
		{
			name: "a town config that cannot be read holds with the reason",
			town: func(t *testing.T) string {
				root := t.TempDir()
				if err := os.MkdirAll(filepath.Dir(mustTownPath(t, root)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(mustTownPath(t, root), []byte("{not json"), 0o600); err != nil {
					t.Fatal(err)
				}
				return root
			},
			settings: keyedSettings,
			getenv:   holdGetenv,
			want:     "the Ring cannot be read",
		},
		{
			name:   "an empty town root holds nothing",
			town:   func(*testing.T) string { return "" },
			getenv: holdGetenv,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := tt.town(t)
			if root != "" && tt.settings != "" {
				writeSettings(t, root, tt.settings)
			}
			got := Hold(root, TownKey(tt.getenv), now)
			if tt.want == "" {
				if got != "" {
					t.Fatalf("Hold = %q, want no hold", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Fatalf("Hold = %q, want it to carry %q", got, tt.want)
			}
		})
	}
}
