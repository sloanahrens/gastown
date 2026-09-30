package doltserver

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// purgeTown makes a town whose gastown rig has an initialized beads
// directory, and returns the town root and that directory.
func purgeTown(t *testing.T) (string, string) {
	t.Helper()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, "gastown", ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	metadata := []byte(`{"dolt_database":"gastown","dolt_server_host":"metadata-host","dolt_server_port":3307}`)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), metadata, 0644); err != nil {
		t.Fatal(err)
	}
	return townRoot, beadsDir
}

// bdCall returns the one call whose argv starts with "bd purge".
func bdPurgeCall(t *testing.T, f *fakeHost) []string {
	t.Helper()
	var found []string
	for _, c := range f.calls {
		if len(c) > 1 && c[0] == "bd" && c[1] == "purge" {
			if found != nil {
				t.Fatalf("more than one bd purge: %q", f.commands())
			}
			found = c
		}
	}
	if found == nil {
		t.Fatalf("no bd purge ran: %q", f.commands())
	}
	return found
}

// bd purge runs pinned to the rig's .beads and its database, on the town's
// configured endpoint, with the caller's stale bd selectors stripped.
func TestPurgeClosedEphemeralsUsesHardenedBDEnv(t *testing.T) {
	t.Parallel()
	townRoot, beadsDir := purgeTown(t)
	f := newFakeHost().on("bd purge *", fakeReply{stdout: `{"purged_count":3}` + "\n"})
	for k, v := range map[string]string{
		"GT_DOLT_HOST":               "127.0.0.2",
		"GT_DOLT_PORT":               "5507",
		"BEADS_DIR":                  "/wrong",
		"BEADS_DB":                   "/wrong.db",
		"BD_DB":                      "/wrong.bd",
		"BEADS_DOLT_SERVER_DATABASE": "wrong",
		"BEADS_DOLT_SERVER_HOST":     "stale-host",
		"BEADS_DOLT_SERVER_PORT":     "9999",
		"BEADS_DOLT_PORT":            "9999",
	} {
		f.setenv(k, v)
	}
	var envSeen []string
	h := f.host()
	run := h.run
	h.run = func(c hostCall) ([]byte, []byte, error) {
		if len(c.Args) > 1 && c.Args[1] == "purge" {
			envSeen = c.Env
		}
		return run(c)
	}

	purged, err := h.PurgeClosedEphemerals(townRoot, "gastown", false)
	if err != nil {
		t.Fatalf("PurgeClosedEphemerals: %v", err)
	}
	if purged != 3 {
		t.Fatalf("purged = %d, want 3", purged)
	}
	if args := bdPurgeCall(t, f); !slices.Equal(args, []string{"bd", "purge", "--json", "--force"}) {
		t.Errorf("argv = %q, want bd purge --json --force", args)
	}
	for _, want := range []string{
		"BEADS_DIR=" + beadsDir,
		"BEADS_DOLT_SERVER_DATABASE=gastown",
		"BEADS_DOLT_SERVER_HOST=127.0.0.2",
		"BEADS_DOLT_SERVER_PORT=5507",
		"BEADS_DOLT_PORT=5507",
		"BD_DOLT_AUTO_COMMIT=on",
	} {
		if !slices.Contains(envSeen, want) {
			t.Errorf("bd env missing %q:\n%s", want, strings.Join(envSeen, "\n"))
		}
	}
	for _, forbidden := range []string{"BEADS_DB=/wrong.db", "BD_DB=/wrong.bd", "BEADS_DOLT_SERVER_DATABASE=wrong", "BEADS_DOLT_SERVER_HOST=stale-host", "BEADS_DOLT_SERVER_PORT=9999", "BEADS_DOLT_PORT=9999"} {
		if slices.Contains(envSeen, forbidden) {
			t.Errorf("stale env leaked via %q", forbidden)
		}
	}
}

// TestPurgeClosedEphemeralsDryRunOmitsForce verifies that a dry-run purge
// passes --dry-run (preview only) and never --force, so gt maintain's
// preview paths can never delete data.
func TestPurgeClosedEphemeralsDryRunOmitsForce(t *testing.T) {
	t.Parallel()
	townRoot, _ := purgeTown(t)
	f := newFakeHost().on("bd purge *", fakeReply{stdout: "warning: preamble\n" + `{"purged_count":0}` + "\n"})

	purged, err := f.host().PurgeClosedEphemerals(townRoot, "gastown", true)
	if err != nil {
		t.Fatalf("PurgeClosedEphemerals: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0", purged)
	}
	if args := bdPurgeCall(t, f); !slices.Equal(args, []string{"bd", "purge", "--json", "--dry-run"}) {
		t.Errorf("argv = %q, want bd purge --json --dry-run and never --force", args)
	}
}

// A rig with no beads directory, or one never initialized, has nothing to
// purge and runs no bd; a failing bd is an error carrying its stderr.
func TestPurgeClosedEphemeralsSkipsAndErrors(t *testing.T) {
	t.Parallel()
	f := newFakeHost()
	h := f.host()
	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, "gastown", ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, townRoot := range []string{t.TempDir(), empty} {
		if n, err := h.PurgeClosedEphemerals(townRoot, "gastown", false); n != 0 || err != nil {
			t.Errorf("PurgeClosedEphemerals(%s) = %d, %v; want 0, nil", townRoot, n, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("bd ran for a rig with nothing to purge: %q", f.commands())
	}

	townRoot, _ := purgeTown(t)
	f.on("bd purge *", fakeReply{stderr: "database locked", code: 1})
	if _, err := h.PurgeClosedEphemerals(townRoot, "gastown", false); err == nil || !strings.Contains(err.Error(), "database locked") {
		t.Errorf("failed purge = %v, want an error with bd's stderr", err)
	}
	f.on("bd purge *", fakeReply{stdout: "not json"})
	if _, err := h.PurgeClosedEphemerals(townRoot, "gastown", false); err == nil || !strings.Contains(err.Error(), "unexpected output format") {
		t.Errorf("garbled purge = %v, want an output-format error", err)
	}
}
