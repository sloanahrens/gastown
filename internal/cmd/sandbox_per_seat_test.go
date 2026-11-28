package cmd

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// sandboxPerSeatSettings holds two profiles on the per-seat switch, one with no
// image (its change is refused), and one already off.
const sandboxPerSeatSettings = `{
  "type": "town-settings",
  "version": 1,
  "sandboxes": {
    "polecat-k8s": {"driver": "kubernetes", "gateway": "k8s", "image": "ghcr.io/example/polecat:1", "policy": "policies/polecat.yaml", "providers": ["forgejo-polecat"], "forgejo_per_seat": true},
    "polecat-other": {"driver": "kubernetes", "gateway": "k8s", "image": "ghcr.io/example/other:1", "policy": "policies/other.yaml", "providers": ["forgejo-polecat"], "forgejo_per_seat": true},
    "polecat-bad": {"driver": "kubernetes", "gateway": "k8s", "policy": "policies/bad.yaml", "providers": ["forgejo-polecat"], "forgejo_per_seat": true},
    "polecat-plain": {"driver": "kubernetes", "gateway": "k8s", "image": "ghcr.io/example/plain:1", "policy": "policies/plain.yaml"}
  }
}`

// sandboxPerSeatTown names root as the town, as the resolver the command takes.
func sandboxPerSeatTown(root string) setResourcesTownResolver {
	return func(string) (string, error) { return root, nil }
}

// Off sets forgejo_per_seat false on the named profile and no other, keeps a .bak-
// copy of the file as it was, and the saved file loads with the setting off.
func TestSandboxPerSeatOffSetsOnlyThatProfile(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, sandboxPerSeatSettings, "")
	before := poolTestRawSettings(t, root)
	var out bytes.Buffer

	if err := runSandboxPerSeatCmd(&out, sandboxPerSeatTown(root), "polecat-k8s", "off"); err != nil {
		t.Fatalf("per-seat off: %v", err)
	}
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if ts.Sandboxes["polecat-k8s"].ForgejoPerSeat || !ts.Sandboxes["polecat-other"].ForgejoPerSeat {
		t.Errorf("forgejo_per_seat: polecat-k8s %v, polecat-other %v; want false and true",
			ts.Sandboxes["polecat-k8s"].ForgejoPerSeat, ts.Sandboxes["polecat-other"].ForgejoPerSeat)
	}
	if !strings.Contains(out.String(), "sandboxes.polecat-k8s.forgejo_per_seat: true -> false") {
		t.Errorf("output = %q, want the change shown", out.String())
	}
	backups := sandboxResourcesBackups(t, root)
	if len(backups) != 1 {
		t.Fatalf("backups beside the settings file = %v, want exactly one .bak- copy", backups)
	}
	if got, err := os.ReadFile(backups[0]); err != nil || string(got) != before {
		t.Errorf("backup %s holds %q (%v), want the file as it was before the write", backups[0], got, err)
	}
}

// Each refusal exits with an error naming its cause and leaves the file byte for
// byte as it was, with no backup made. on is refused before the town is read.
func TestSandboxPerSeatRefusals(t *testing.T) {
	t.Parallel()
	const corrupt = `{"type": "town-settings", "sandboxes": `
	for _, tc := range []struct {
		name     string
		settings string
		profile  string
		state    string
		want     string
	}{
		{"an unknown profile", "", "polecat-missing", "off", `"polecat-missing" is not a sandbox profile`},
		{"on", "", "polecat-k8s", "on", "per-seat on is not built yet"},
		{"an unknown state", "", "polecat-k8s", "maybe", `unknown state "maybe"`},
		{"a profile already off", "", "polecat-plain", "off", "nothing to change"},
		{"a profile that fails the check", "", "polecat-bad", "off", "image: required"},
		{"a corrupt settings file", corrupt, "polecat-k8s", "off", "sandbox per-seat:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			settings := tc.settings
			if settings == "" {
				settings = sandboxPerSeatSettings
			}
			root := poolTestTown(t, settings, "")
			before := poolTestRawSettings(t, root)

			err := runSandboxPerSeatCmd(&bytes.Buffer{}, sandboxPerSeatTown(root), tc.profile, tc.state)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("per-seat = %v, want a refusal containing %q", err, tc.want)
			}
			if after := poolTestRawSettings(t, root); after != before {
				t.Error("a refused change rewrote the settings file")
			}
			if backups := sandboxResourcesBackups(t, root); len(backups) != 0 {
				t.Errorf("a refused change made backups: %v", backups)
			}
		})
	}
}

// A copy that cannot be made stops the write before the profile changes, so the
// file keeps its bytes and the error names the copy that failed.
func TestSandboxPerSeatBackupFailureChangesNothing(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, sandboxPerSeatSettings, "")
	before := poolTestRawSettings(t, root)
	refuseCopy := func(string, string) (string, error) { return "", errors.New("copy refused") }

	_, _, err := writeSandboxPerSeat(config.TownSettingsPath(root), "20260101T000000Z", "polecat-k8s", false, refuseCopy)
	if err == nil || !strings.Contains(err.Error(), "copy refused") {
		t.Fatalf("per-seat off with a failing backup = %v, want the copy's refusal", err)
	}
	if after := poolTestRawSettings(t, root); after != before {
		t.Error("a write whose copy failed rewrote the settings file")
	}
}

// A town that does not resolve stops the change before the file is read, so the
// command returns the resolver's error and the file keeps its bytes, with no copy.
func TestSandboxPerSeatUnresolvedTownChangesNothing(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, sandboxPerSeatSettings, "")
	before := poolTestRawSettings(t, root)
	noTown := func(string) (string, error) { return "", errors.New("no town here") }

	err := runSandboxPerSeatCmd(&bytes.Buffer{}, noTown, "polecat-k8s", "off")
	if err == nil || !strings.Contains(err.Error(), "no town here") {
		t.Fatalf("per-seat off with no town = %v, want the resolver's error", err)
	}
	if after := poolTestRawSettings(t, root); after != before {
		t.Error("an unresolved town rewrote the settings file")
	}
	if backups := sandboxResourcesBackups(t, root); len(backups) != 0 {
		t.Errorf("an unresolved town made backups: %v", backups)
	}
}
