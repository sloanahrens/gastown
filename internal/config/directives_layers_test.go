package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
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

// A name present at the new central path and at the legacy town path is read
// from the new path only: the legacy file is not also read (gt-oz73l).
func TestDirectiveNewPathWinsOverLegacy(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDirective(t, filepath.Join(CentralDirectiveDir(town), "polecat.md"), "new central")
	writeDirective(t, filepath.Join(town, "directives", "polecat.md"), "legacy town")
	got := directiveTexts(t, "polecat", town, "alpha")
	for _, text := range got {
		if strings.Contains(text, "legacy") {
			t.Fatalf("the legacy file is read alongside the new one: %q", got)
		}
	}
	if len(got) != 1 || got[0] != "new central" {
		t.Errorf("directives = %q, want just the new central file", got)
	}
}

// A name present only at a legacy path is read, and the warning naming that path
// is logged once per file per process, however many times it is read (gt-oz73l).
// The warning goes to the standard log, which this test captures; the log output
// is process-wide, so the test is not parallel.
//
//testpolicy:allow parallel — captures the process-wide standard log output that the warning is written to
func TestDirectiveLegacyReadWarnsOncePerFile(t *testing.T) {
	town := t.TempDir()
	legacy := filepath.Join(town, "directives", "polecat.md")
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

// The central layer applies to every rig, and it is read once even for the
// gastown rig. A rig's own layer comes from its canonical checkout (gt-oz73l).
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
				dir = filepath.Join(town, "directives")
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
