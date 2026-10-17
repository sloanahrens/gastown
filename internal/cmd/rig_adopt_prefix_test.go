package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// A rig adopted without --prefix is registered, in rigs.json and routes.jsonl,
// under the prefix its .beads database holds (xx), not one derived from its
// name (my_project would give mp).
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

// bd's issue_prefix answer wins over the database name in metadata.json.
func TestAdoptPrefixPrefersBdOverMetadataName(t *testing.T) {
	t.Parallel()
	town := adoptPrefixTown(t, "my_project", "xx")

	got, err := detectAdoptBeadsPrefix(filepath.Join(town, "my_project"), "", func(string) (string, error) { return "yy", nil })
	if err != nil || got != "yy" {
		t.Errorf("detectAdoptBeadsPrefix = %q, %v; want yy", got, err)
	}
}

// A --prefix that disagrees with the rig's .beads fails with the town
// untouched, so a retry with the right prefix is not met by ErrRigExists.
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

// The detection tries the rig-level .beads, then mayor/rig's, and errors on a
// metadata.json it cannot trust or a prefix it cannot find instead of registering a name-derived prefix.
func TestDetectAdoptBeadsPrefixDirs(t *testing.T) {
	t.Parallel()
	const doltMeta = `{"backend":"dolt","dolt_database":"beads_zz"}`
	tests := []struct {
		name       string
		rigMeta    *string // rig-level .beads/metadata.json; nil leaves the directory empty
		rigMetaDir bool    // make the rig-level metadata.json a directory, so reading it fails with a non-ENOENT error
		mayorMeta  *string // mayor/rig/.beads/metadata.json; nil leaves it absent
		flagPrefix string
		want       string
		wantErr    string
	}{
		{name: "rig-level metadata", rigMeta: ptr(doltMeta), want: "zz"},
		{name: "falls through an empty rig-level .beads to mayor/rig", mayorMeta: ptr(doltMeta), want: "zz"},
		{name: "no metadata anywhere", want: ""},
		{name: "empty backend is dolt", rigMeta: ptr(`{"dolt_database":"beads_zz"}`), want: "zz"},
		{name: "dolt backend with no prefix", rigMeta: ptr(`{"backend":"dolt"}`), mayorMeta: ptr(`{"backend":"dolt"}`), wantErr: "no prefix detectable"},
		{name: "dolt backend with no prefix falls through to mayor/rig", rigMeta: ptr(`{"backend":"dolt"}`), mayorMeta: ptr(doltMeta), want: "zz"},
		{name: "dolt backend with no prefix with --prefix", rigMeta: ptr(`{"backend":"dolt"}`), flagPrefix: "aa", want: ""},
		{name: "unreadable metadata", rigMetaDir: true, mayorMeta: ptr(doltMeta), wantErr: "reading"},
		{name: "unparseable metadata", rigMeta: ptr(`{not json`), mayorMeta: ptr(doltMeta), wantErr: "parsing"},
		{name: "non-dolt backend", rigMeta: ptr(`{"backend":"sqlite"}`), mayorMeta: ptr(doltMeta), wantErr: "pass --prefix"},
		{name: "non-dolt backend with --prefix", rigMeta: ptr(`{"backend":"sqlite"}`), flagPrefix: "aa", want: ""},
		{name: "mismatch in mayor/rig", mayorMeta: ptr(doltMeta), flagPrefix: "aa", wantErr: "prefix mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rigPath := filepath.Join(t.TempDir(), "rig")
			rigBeads := filepath.Join(rigPath, ".beads")
			mayorBeads := filepath.Join(rigPath, "mayor", "rig", ".beads")
			if err := os.MkdirAll(rigBeads, 0o755); err != nil {
				t.Fatal(err)
			}
			if tt.rigMetaDir {
				if err := os.Mkdir(filepath.Join(rigBeads, "metadata.json"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tt.rigMeta != nil {
				if err := os.WriteFile(filepath.Join(rigBeads, "metadata.json"), []byte(*tt.rigMeta), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.mayorMeta != nil {
				if err := os.MkdirAll(mayorBeads, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(mayorBeads, "metadata.json"), []byte(*tt.mayorMeta), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			got, err := detectAdoptBeadsPrefix(rigPath, tt.flagPrefix, noBdPrefix)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("detectAdoptBeadsPrefix error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("detectAdoptBeadsPrefix = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}
