package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// stageDirectiveFile stages one directive file at path with the given text.
func stageDirectiveFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSpawnRenderCarriesEveryDirectiveWhole checks that all four directive
// sources appear in the rendered system-prompt file: each under its heading, in
// resolution order (town shared, rig shared, town role, rig role), with its text
// intact. The texts carry markdown that a truncation or a re-encode would damage.
func TestSpawnRenderCarriesEveryDirectiveWhole(t *testing.T) {
	t.Parallel()
	town, rigPath := newSpawnRenderTown(t, "myrig", "nux")
	texts := map[string]string{
		filepath.Join(town, "directives", "_common.md"):          "## Host Hygiene\n\nNo `/` scans.\n~~~\nfence\n~~~\n",
		filepath.Join(town, "myrig", "directives", "_common.md"): "## Rig testing\n\nNo new scripts.\n",
		filepath.Join(town, "directives", "polecat.md"):          "## Town polecat\n\nHermetic tests only.\n",
		filepath.Join(town, "myrig", "directives", "polecat.md"): "## What om rejects\n\n- an undeclared read\n- no end-to-end test",
	}
	for path, text := range texts {
		stageDirectiveFile(t, path, text)
	}
	path := config.SystemPromptFilePath("polecat", town, rigPath, "nux")
	if err := renderSystemPromptFileForSpawn("polecat", town, rigPath, "nux", path, config.SystemPromptTarget{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	rendered := string(got)

	headings := []string{
		"## Directive: town _common\n",
		"## Directive: myrig _common\n",
		"## Directive: town polecat\n",
		"## Directive: myrig polecat\n",
	}
	sources := []string{
		filepath.Join(town, "directives", "_common.md"),
		filepath.Join(town, "myrig", "directives", "_common.md"),
		filepath.Join(town, "directives", "polecat.md"),
		filepath.Join(town, "myrig", "directives", "polecat.md"),
	}
	last := -1
	for i, h := range headings {
		at := strings.Index(rendered, h)
		if at < 0 {
			t.Fatalf("no heading %q in the rendered file", h)
		}
		if at <= last {
			t.Errorf("heading %q is out of order", h)
		}
		last = at
		text := texts[sources[i]]
		body := rendered[at+len(h):]
		if !strings.HasPrefix(strings.TrimLeft(body, "\n"), text) {
			t.Errorf("the source under %q is not carried whole and byte for byte", h)
		}
	}
}

// TestSpawnRenderWithoutDirectivesIsTheTemplateOnly checks a role with no
// directive files renders no directive heading, so it carries the template and
// nothing else from this change.
func TestSpawnRenderWithoutDirectivesIsTheTemplateOnly(t *testing.T) {
	t.Parallel()
	town, rigPath := newSpawnRenderTown(t, "myrig", "nux")
	path := config.SystemPromptFilePath("polecat", town, rigPath, "nux")
	if err := renderSystemPromptFileForSpawn("polecat", town, rigPath, "nux", path, config.SystemPromptTarget{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if strings.Contains(string(got), "## Directive:") {
		t.Errorf("a role with no directive files carries a directive heading")
	}
}

// TestUnreadableDirectiveFailsTheRender checks fail-closed: a directive source
// that exists and cannot be read fails the render, and no prompt file is written
// without it. A directory where a file should be is an unreadable source.
func TestUnreadableDirectiveFailsTheRender(t *testing.T) {
	t.Parallel()
	town, rigPath := newSpawnRenderTown(t, "myrig", "nux")
	if err := os.MkdirAll(filepath.Join(town, "directives", "_common.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := config.SystemPromptFilePath("polecat", town, rigPath, "nux")
	if err := renderSystemPromptFileForSpawn("polecat", town, rigPath, "nux", path, config.SystemPromptTarget{}); err == nil {
		t.Fatalf("render succeeded with an unreadable directive source")
	}
	if _, err := os.Stat(path); err == nil {
		t.Errorf("a prompt file was written without the unreadable directive")
	}
	if _, _, err := staticRoleText(RoleContext{Role: RolePolecat, Rig: "myrig", Polecat: "nux", TownRoot: town,
		WorkDir: filepath.Join(rigPath, "polecats", "nux", "myrig")}); err == nil {
		t.Errorf("staticRoleText succeeded with an unreadable directive source")
	}
}

// TestWhitespaceDirectiveIsAbsent checks a directive file that holds only
// whitespace renders no heading, as RoleDirectiveSources treats it.
func TestWhitespaceDirectiveIsAbsent(t *testing.T) {
	t.Parallel()
	town, rigPath := newSpawnRenderTown(t, "myrig", "nux")
	stageDirectiveFile(t, filepath.Join(town, "directives", "_common.md"), " \n\t\n")
	path := config.SystemPromptFilePath("polecat", town, rigPath, "nux")
	if err := renderSystemPromptFileForSpawn("polecat", town, rigPath, "nux", path, config.SystemPromptTarget{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	got, _ := os.ReadFile(path)
	if strings.Contains(string(got), "## Directive: town _common") {
		t.Errorf("a whitespace-only directive rendered a heading")
	}
}

// TestSpawnAndPrimeWriteIdenticalBytesWithDirectives checks the two writers
// agree once directives exist: the spawn render and gt prime's refresh are both
// staticRoleText, so prime's refresh finds the file current and rewrites nothing.
func TestSpawnAndPrimeWriteIdenticalBytesWithDirectives(t *testing.T) {
	t.Parallel()
	town, rigPath := newSpawnRenderTown(t, "myrig", "nux")
	stageDirectiveFile(t, filepath.Join(town, "myrig", "directives", "polecat.md"), "## Rig polecat\n\nKeep it short.\n")
	path := config.SystemPromptFilePath("polecat", town, rigPath, "nux")
	if err := renderSystemPromptFileForSpawn("polecat", town, rigPath, "nux", path, config.SystemPromptTarget{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	got, _ := os.ReadFile(path)
	want, fromTemplate, err := staticRoleText(RoleContext{Role: RolePolecat, Rig: "myrig", Polecat: "nux", TownRoot: town,
		WorkDir: filepath.Join(rigPath, "polecats", "nux", "myrig")})
	if err != nil || !fromTemplate {
		t.Fatalf("staticRoleText: err=%v fromTemplate=%v", err, fromTemplate)
	}
	if string(got) != want {
		t.Fatalf("the spawn render differs from gt prime's text")
	}
	if wrote, err := writeSystemPromptFile(path, want); err != nil || wrote {
		t.Fatalf("prime's refresh must find the file current: wrote=%v err=%v", wrote, err)
	}
}
