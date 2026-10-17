package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// adoptPrefixTown writes a town holding one unregistered rig directory whose
// own beads database uses beadsPrefix (metadata.json names beads_<prefix>).
func adoptPrefixTown(t *testing.T, rigName, beadsPrefix string) string {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(town, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	beadsDir := filepath.Join(town, rigName, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"backend":"dolt","dolt_database":"beads_` + beadsPrefix + `"}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	return town
}

// noBdPrefix is the bd lookup of a host where bd cannot answer, so detection
// falls back to the database name in metadata.json.
func noBdPrefix(string) (string, error) { return "", errors.New("bd unavailable") }

// readIfPresent returns a file's contents, or "" when it does not exist.
func readIfPresent(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// Adopting a rig whose .beads uses xx, with no --prefix, registered the prefix
// derived from the name (my_project -> mp): the detection ran after
// RegisterRig, behind a guard that RegisterRig's non-empty prefix always
// failed. InitBeads, VerifyRigIdentity and the identity bead then used mp
// against a database holding xx- issues (gt-prqdo).
func TestAdoptDetectedPrefixRegistered(t *testing.T) {
	t.Parallel()
	town := adoptPrefixTown(t, "my_project", "xx")

	_, result, err := adoptRegisterIn(town, rigAdoptRequest{
		name:   "my_project",
		gitURL: "file:///nonexistent",
	}, noBdPrefix)
	if err != nil {
		t.Fatalf("adoptRegisterIn: %v", err)
	}

	if result.BeadsPrefix != "xx" {
		t.Errorf("result prefix = %q, want xx", result.BeadsPrefix)
	}
	// The identity bead is created under result.BeadsPrefix.
	if got, want := beads.RigBeadIDWithPrefix(result.BeadsPrefix, "my_project"), beads.RigBeadIDWithPrefix("xx", "my_project"); got != want {
		t.Errorf("identity bead id = %q, want %q", got, want)
	}

	rigs, err := config.LoadRigsConfig(filepath.Join(town, "mayor", "rigs.json"))
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := rigs.Rigs["my_project"]
	if !ok || entry.BeadsConfig == nil {
		t.Fatalf("rigs.json has no beads config for my_project: %+v", rigs.Rigs)
	}
	if entry.BeadsConfig.Prefix != "xx" {
		t.Errorf("rigs.json prefix = %q, want xx", entry.BeadsConfig.Prefix)
	}

	routes := readIfPresent(t, filepath.Join(town, ".beads", "routes.jsonl"))
	if !strings.Contains(routes, `"prefix":"xx-"`) || strings.Contains(routes, `"prefix":"mp-"`) {
		t.Errorf("routes.jsonl = %q, want an xx- route and no mp- route", routes)
	}
}

// A detected prefix wins over bd's silence, and bd's answer wins over the
// database name.
func TestAdoptPrefixPrefersBdOverMetadataName(t *testing.T) {
	t.Parallel()
	town := adoptPrefixTown(t, "my_project", "xx")

	got, err := detectAdoptBeadsPrefix(filepath.Join(town, "my_project"), "", func(string) (string, error) { return "yy", nil })
	if err != nil || got != "yy" {
		t.Errorf("detectAdoptBeadsPrefix = %q, %v; want yy", got, err)
	}
}

// --prefix aa against a .beads of bb returned the mismatch error after the rig
// was written to rigs.json, routed and committed, so the retry met
// ErrRigExists. Nothing is written now (gt-prqdo).
func TestAdoptPrefixMismatchWritesNothing(t *testing.T) {
	t.Parallel()
	town := adoptPrefixTown(t, "my_project", "bb")
	rigsPath := filepath.Join(town, "mayor", "rigs.json")
	routesPath := filepath.Join(town, ".beads", "routes.jsonl")
	rigsBefore := readIfPresent(t, rigsPath)
	routesBefore := readIfPresent(t, routesPath)

	_, _, err := adoptRegisterIn(town, rigAdoptRequest{
		name:   "my_project",
		gitURL: "file:///nonexistent",
		prefix: "aa",
	}, noBdPrefix)
	if err == nil || !strings.Contains(err.Error(), "prefix mismatch") {
		t.Fatalf("adoptRegisterIn error = %v, want the prefix mismatch error", err)
	}

	if got := readIfPresent(t, rigsPath); got != rigsBefore {
		t.Errorf("rigs.json changed: %q -> %q", rigsBefore, got)
	}
	if got := readIfPresent(t, routesPath); got != routesBefore {
		t.Errorf("routes.jsonl changed: %q -> %q", routesBefore, got)
	}

	// The matching prefix is no mismatch, and the retry is not met by ErrRigExists.
	if _, _, err := adoptRegisterIn(town, rigAdoptRequest{
		name:   "my_project",
		gitURL: "file:///nonexistent",
		prefix: "bb",
	}, noBdPrefix); err != nil {
		t.Errorf("retry with the matching prefix: %v", err)
	}
}
