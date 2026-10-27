package config

import (
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
// The warning is captured through legacyDirectiveWarn, a package variable, so
// this test runs alone.
//
//testpolicy:allow parallel — swaps the package variable legacyDirectiveWarn, which parallel tests in this package read through warnLegacyDirective
func TestDirectiveLegacyReadWarnsOncePerFile(t *testing.T) {
	town := t.TempDir()
	legacy := filepath.Join(town, "directives", "polecat.md")
	writeDirective(t, legacy, "legacy only")
	var lines []string
	saved := legacyDirectiveWarn
	legacyDirectiveWarn = func(line string) { lines = append(lines, line) }
	defer func() { legacyDirectiveWarn = saved }()
	for i := 0; i < 3; i++ {
		got := directiveTexts(t, "polecat", town, "alpha")
		if len(got) != 1 || got[0] != "legacy only" {
			t.Fatalf("pass %d: directives = %q, want the legacy file read", i, got)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], legacy) {
		t.Errorf("warnings = %q, want one line naming %s", lines, legacy)
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
