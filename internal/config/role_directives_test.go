package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// directiveFixture stages a town with a town-shared and a rig-role directive
// and returns the town and rig names.
func directiveFixture(t *testing.T) (town, rig string) {
	t.Helper()
	town, rig = t.TempDir(), "myrig"
	writeFixtureFile(t, filepath.Join(town, "directives", "_common.md"), "Town shared rule.\n")
	writeFixtureFile(t, filepath.Join(town, rig, "directives", "polecat.md"), "Rig polecat rule.\n")
	return town, rig
}

func writeFixtureFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReadRoleDirectivesOrderScopeAndText checks the sources come back broadest
// first, with their scope, name and full text as written.
func TestReadRoleDirectivesOrderScopeAndText(t *testing.T) {
	t.Parallel()
	town, rig := directiveFixture(t)
	got, err := ReadRoleDirectives("polecat", town, rig)
	if err != nil {
		t.Fatalf("ReadRoleDirectives: %v", err)
	}
	want := []RoleDirective{
		{Scope: "town", Name: "_common", Path: filepath.Join(town, "directives", "_common.md"), Text: "Town shared rule.\n"},
		{Scope: rig, Name: "polecat", Path: filepath.Join(town, rig, "directives", "polecat.md"), Text: "Rig polecat rule.\n"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadRoleDirectives = %+v, want %+v", got, want)
	}
}

// TestReadRoleDirectivesAbsentAndBlank checks that a missing file and a
// whitespace-only file are both absent, with no error.
func TestReadRoleDirectivesAbsentAndBlank(t *testing.T) {
	t.Parallel()
	town, rig := directiveFixture(t)
	writeFixtureFile(t, filepath.Join(town, rig, "directives", "polecat.md"), " \n\t\n")
	got, err := ReadRoleDirectives("polecat", town, rig)
	if err != nil {
		t.Fatalf("ReadRoleDirectives: %v", err)
	}
	if len(got) != 1 || got[0].Name != "_common" {
		t.Errorf("ReadRoleDirectives = %+v, want only the town shared file", got)
	}
}

// TestReadRoleDirectivesUnreadableIsAnError checks the fail-closed rule: a
// directive path that exists but cannot be read is an error, not an absence. A
// directory where a file should be is such a path.
func TestReadRoleDirectivesUnreadableIsAnError(t *testing.T) {
	t.Parallel()
	town, rig := directiveFixture(t)
	rigFile := filepath.Join(town, rig, "directives", "polecat.md")
	if err := os.Remove(rigFile); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigFile, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRoleDirectives("polecat", town, rig); err == nil {
		t.Fatalf("ReadRoleDirectives succeeded with an unreadable source")
	}
}

// TestOneReaderForTheFileAndTheHook checks that the lenient readers the hook and
// directive show use agree with the strict reader the prompt file uses: the same
// files, in the same order, with the same text once trimmed.
func TestOneReaderForTheFileAndTheHook(t *testing.T) {
	t.Parallel()
	town, rig := directiveFixture(t)
	strict, err := ReadRoleDirectives("polecat", town, rig)
	if err != nil {
		t.Fatalf("ReadRoleDirectives: %v", err)
	}
	var paths, texts []string
	for _, d := range strict {
		paths = append(paths, d.Path)
		texts = append(texts, strings.TrimSpace(d.Text))
	}
	if got := RoleDirectiveSources("polecat", town, rig); !reflect.DeepEqual(got, paths) {
		t.Errorf("RoleDirectiveSources = %v, want %v", got, paths)
	}
	if got := LoadRoleDirective("polecat", town, rig); got != strings.Join(texts, "\n") {
		t.Errorf("LoadRoleDirective = %q, want %q", got, strings.Join(texts, "\n"))
	}
}

// TestSpawnRefusesAnUnreadableDirective checks the spawn-side fail-closed rule: a
// polecat whose directive source cannot be read gets an error from the resolver
// the spawn uses, so the session does not start without its policy.
func TestSpawnRefusesAnUnreadableDirective(t *testing.T) {
	t.Parallel()
	town, rig := directiveFixture(t)
	if err := os.Remove(filepath.Join(town, "directives", "_common.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(town, "directives", "_common.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigPath := filepath.Join(town, rig)
	rc, err := ResolveRoleAgentConfigWithOverride("polecat", town, rigPath, "", "nux")
	if err == nil || rc != nil {
		t.Fatalf("spawn with an unreadable directive: rc=%v err=%v, want a refusal", rc, err)
	}
	if !strings.Contains(err.Error(), "directives unreadable") {
		t.Errorf("refusal does not say why: %v", err)
	}
}
