package config

import (
	"bytes"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeDirective writes one directive file, creating its directory.
func writeDirective(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// directiveTexts is the text of each directive ReadRoleDirectives returns, in
// order, so a test reads what the prompt would render.
func directiveTexts(t *testing.T, role, town, rig string) []string {
	t.Helper()
	dirs, err := ReadRoleDirectives(role, town, rig)
	if err != nil {
		t.Fatalf("ReadRoleDirectives: %v", err)
	}
	var out []string
	for _, d := range dirs {
		out = append(out, d.Text)
	}
	return out
}

// The central and rig layers resolve the same way, each from its own directories:
// the new file, or the legacy file where the new one is absent. Each row is one
// of the four states, and each asserts the text read and whether the legacy
// path is named in the warning (gt-oz73l, gt-3lvcjz.11). The warning goes to the
// standard log, which this test captures; the log output is process-wide, so the
// test is not parallel.
//
//testpolicy:allow parallel — captures the process-wide standard log output that the warning is written to
func TestDirectiveLayerResolution(t *testing.T) {
	const rig = "alpha"
	layers := []struct {
		name    string
		scope   string
		newFile func(town string) string
		legacy  func(town string) string
	}{
		{
			name:    "central",
			scope:   "town",
			newFile: func(town string) string { return filepath.Join(town, "directives", "polecat.md") },
			legacy: func(town string) string {
				return filepath.Join(town, "gastown", "mayor", "rig", "directives", "polecat.md")
			},
		},
		{
			name:    "rig",
			scope:   rig,
			newFile: func(town string) string { return filepath.Join(town, rig, ".gastown", "directives", "polecat.md") },
			legacy:  func(town string) string { return filepath.Join(town, rig, "directives", "polecat.md") },
		},
	}
	rows := []struct {
		name        string
		writeNew    bool
		writeLegacy bool
		wantText    []string
		wantWarn    bool
	}{
		{name: "new only", writeNew: true, wantText: []string{"new"}},
		{name: "legacy only", writeLegacy: true, wantText: []string{"legacy"}, wantWarn: true},
		{name: "both", writeNew: true, writeLegacy: true, wantText: []string{"new"}},
		{name: "neither", wantText: nil},
	}
	for _, layer := range layers {
		for _, row := range rows {
			t.Run(layer.name+"/"+row.name, func(t *testing.T) {
				town := t.TempDir()
				newPath, legacy := layer.newFile(town), layer.legacy(town)
				if row.writeNew {
					writeDirective(t, newPath, "new")
				}
				if row.writeLegacy {
					writeDirective(t, legacy, "legacy")
				}
				var buf bytes.Buffer
				log.SetOutput(&buf)
				defer log.SetOutput(os.Stderr)
				got := directiveTexts(t, "polecat", town, rig)
				if !slices.Equal(got, row.wantText) {
					t.Errorf("directives = %q, want %q", got, row.wantText)
				}
				dirs, err := ReadRoleDirectives("polecat", town, rig)
				if err != nil {
					t.Fatalf("ReadRoleDirectives: %v", err)
				}
				for _, d := range dirs {
					if d.Scope != layer.scope {
						t.Errorf("a %s-layer file has scope %q, want %q (%s)", layer.name, d.Scope, layer.scope, d.Path)
					}
				}
				wantLines := 0
				if row.wantWarn {
					wantLines = 1
				}
				if lines := strings.Count(buf.String(), legacy); lines != wantLines {
					t.Errorf("warnings name %s %d time(s), want %d:\n%s", legacy, lines, wantLines, buf.String())
				}
				if row.wantWarn && !strings.Contains(buf.String(), newPath) {
					t.Errorf("warning does not name the new path %s:\n%s", newPath, buf.String())
				}
			})
		}
	}
}

// The two layer directories are the locations the reader and the editor agree
// on: <town>/directives for the central layer, <rig>/.gastown/directives for the
// rig's own (gt-3lvcjz.11).
func TestDirectiveLayerLocations(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if got, want := CentralDirectiveDir(town), filepath.Join(town, "directives"); got != want {
		t.Errorf("CentralDirectiveDir = %s, want %s", got, want)
	}
	if got, want := RigDirectiveDir(town, "alpha"), filepath.Join(town, "alpha", ".gastown", "directives"); got != want {
		t.Errorf("RigDirectiveDir = %s, want %s", got, want)
	}
}

// The town-level layer is the deployed copy of the gastown repo's directives/, so
// its edit path refuses: the error names the repo file and the install step, and
// no path is returned to write to. The rig's own layer is edited in place
// (gt-3lvcjz.11).
func TestDirectiveEditPathRefusesTheTownLayer(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	tests := []struct {
		name      string
		rig       string
		townLevel bool
		wantPath  string
		wantErr   []string
	}{
		{
			name:      "town layer requested with a rig",
			rig:       "alpha",
			townLevel: true,
			wantErr:   []string{"directives/polecat.md", "gt install"},
		},
		{
			name:    "no rig names the town layer",
			wantErr: []string{"directives/polecat.md", "gt install"},
		},
		{
			name:     "rig layer is written under the rig",
			rig:      "alpha",
			wantPath: filepath.Join(town, "alpha", ".gastown", "directives", "polecat.md"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := DirectiveEditPath("polecat", town, tc.rig, tc.townLevel)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("DirectiveEditPath = %q, want a refusal", got)
				}
				if got != "" {
					t.Errorf("refusal returned path %q, want none", got)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal %q does not name %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("DirectiveEditPath: %v", err)
			}
			if got != tc.wantPath {
				t.Errorf("DirectiveEditPath = %s, want %s", got, tc.wantPath)
			}
		})
	}
}

// A legacy file is read and warned about once per file per process, however many
// times it is read (gt-oz73l). The warning goes to the standard log, which this
// test captures; the log output is process-wide, so the test is not parallel.
//
//testpolicy:allow parallel — captures the process-wide standard log output that the warning is written to
func TestDirectiveLegacyReadWarnsOncePerFile(t *testing.T) {
	town := t.TempDir()
	legacy := filepath.Join(town, "gastown", "mayor", "rig", "directives", "polecat.md")
	writeDirective(t, legacy, "legacy only")
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	for i := 0; i < 3; i++ {
		got := directiveTexts(t, "polecat", town, "alpha")
		if len(got) != 1 || got[0] != "legacy only" {
			t.Fatalf("pass %d: directives = %q, want the legacy file read", i, got)
		}
	}
	if lines := strings.Count(buf.String(), legacy); lines != 1 {
		t.Errorf("warnings name %s %d time(s), want once:\n%s", legacy, lines, buf.String())
	}
}

// ReadRoleDirectiveFrom resolves through the caller's reader the same text
// LoadRoleDirective renders: both layers, the Changelog strip and the join.
func TestReadRoleDirectiveFromMatchesTheReader(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDirective(t, filepath.Join(CentralDirectiveDir(town), "_common.md"),
		"## Changelog\n\n- a note\n\n## Shared\n\nshared\n")
	writeDirective(t, filepath.Join(RigDirectiveDir(town, "alpha"), "polecat.md"), "rig rule")
	text, found, err := ReadRoleDirectiveFrom("polecat", town, "alpha", os.ReadFile)
	if err != nil || !found {
		t.Fatalf("ReadRoleDirectiveFrom = (%q, %v, %v), want the directive found", text, found, err)
	}
	if want := LoadRoleDirective("polecat", town, "alpha"); text != want {
		t.Errorf("ReadRoleDirectiveFrom = %q, want the reader's %q", text, want)
	}
}

// A reader that reports no file gives found=false, and one that reports a
// failure other than absence is an error rather than a dropped policy.
func TestReadRoleDirectiveFromReportsAbsenceAndUnreadable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		read      func(string) ([]byte, error)
		wantFound bool
		wantErr   bool
	}{
		{"no file", func(string) ([]byte, error) { return nil, fs.ErrNotExist }, false, false},
		{"an empty file", func(string) ([]byte, error) { return []byte("  \n"), nil }, true, false},
		{"an unreadable file", func(string) ([]byte, error) { return nil, fs.ErrPermission }, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, found, err := ReadRoleDirectiveFrom("polecat", t.TempDir(), "", tc.read)
			if (err != nil) != tc.wantErr || found != tc.wantFound {
				t.Errorf("found, err = %v, %v, want %v, %v", found, err, tc.wantFound, tc.wantErr)
			}
		})
	}
}

// The central layer applies to every rig, and it is read once even for the
// gastown rig. A rig's own layer comes from <rig>/.gastown/directives (gt-oz73l,
// gt-3lvcjz.11).
func TestDirectiveCentralAppliesToEveryRig(t *testing.T) {
	t.Parallel()
	for _, rig := range []string{"alpha", "gastown"} {
		t.Run(rig, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			central := filepath.Join(CentralDirectiveDir(town), "_common.md")
			writeDirective(t, central, "central rules")
			rigFile := filepath.Join(RigDirectiveDir(town, rig), "polecat.md")
			writeDirective(t, rigFile, "rig rules")
			dirs, err := ReadRoleDirectives("polecat", town, rig)
			if err != nil {
				t.Fatalf("ReadRoleDirectives: %v", err)
			}
			centrals := 0
			var rigs []string
			for _, d := range dirs {
				if d.Path == central {
					centrals++
					if d.Scope != "town" {
						t.Errorf("central scope = %q, want town", d.Scope)
					}
				}
				if d.Path == rigFile {
					rigs = append(rigs, d.Text)
				}
			}
			if centrals != 1 {
				t.Errorf("the central file is read %d times, want once", centrals)
			}
			if len(rigs) != 1 || rigs[0] != "rig rules" {
				t.Errorf("the rig layer = %q, want the file from %s", rigs, rigFile)
			}
		})
	}
}

// A rig's directive files at the legacy path are not read for another rig, and
// the other rig's layer is not read for this one: each rig reads only its own
// checkout plus the central layer (gt-oz73l).
func TestDirectiveRigReadsOnlyItsOwnLayer(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDirective(t, filepath.Join(RigDirectiveDir(town, "beta"), "polecat.md"), "beta only")
	got := directiveTexts(t, "polecat", town, "alpha")
	for _, text := range got {
		if strings.Contains(text, "beta") {
			t.Errorf("alpha reads beta's layer: %q", got)
		}
	}
	if len(got) != 0 {
		t.Errorf("directives = %q, want none for alpha", got)
	}
}

// A content file's leading "## Changelog" block is the editors' record, not
// directive text: it stays out of the text ReadRoleDirectives returns, and the
// rules after it come through (gt-80rnu).
func TestDirectiveChangelogBlockIsLeftOut(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDirective(t, filepath.Join(CentralDirectiveDir(town), "polecat.md"),
		"## Changelog\n- 2026-10-26 edit\n- 2026-10-25 first cut\n\n## Rules\nreal text\n")
	got := directiveTexts(t, "polecat", town, "alpha")
	if len(got) != 1 || got[0] != "## Rules\nreal text\n" {
		t.Errorf("directive text = %q, want the rules without the changelog block", got)
	}
}

// A directive layer that cannot be listed is an error the scan returns, not an
// empty layer: a directory that is a file fails for each layer (gt-80rnu).
func TestScanDirectiveFilesReportsAnUnlistableLayer(t *testing.T) {
	t.Parallel()
	for _, layer := range []string{"central", "legacy"} {
		t.Run(layer, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			dir := CentralDirectiveDir(town)
			if layer == "legacy" {
				dir = centralLegacyDirectiveDir(town)
			}
			if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dir, []byte("not a directory"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ScanDirectiveFiles(town, "alpha"); err == nil {
				t.Errorf("ScanDirectiveFiles returned no error for an unlistable %s layer", layer)
			}
		})
	}
}
