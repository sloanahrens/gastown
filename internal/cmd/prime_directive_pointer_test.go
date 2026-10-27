package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// pointerTown stages a town whose polecat carries one town-shared directive and
// returns the hook context and the directive's exact text as rendered.
func pointerTown(t *testing.T) (RoleContext, string) {
	t.Helper()
	town, _ := newSpawnRenderTown(t, "myrig", "nux")
	writeDirectiveFile(t, filepath.Join(town, "directives", "_common.md"), "Keep it short.\n")
	ctx := RoleContext{Role: RolePolecat, Rig: "myrig", Polecat: "nux", TownRoot: town,
		WorkDir: filepath.Join(town, "myrig", "polecats", "nux", "myrig")}
	return ctx, "Keep it short.\n"
}

// writeDirectiveFile stages one directive file (the pointer tests' helper; the
// name avoids the other test helpers in this package).
func writeDirectiveFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHookPointerWhenThePromptFileHoldsTheDirectives checks that with the
// system-prompt file in use and carrying the directives, the hook prints exactly
// one pointer line, with the size and a 12-character sha256 prefix of the file's
// own bytes, and no directive text (gt-uhzl7).
func TestHookPointerWhenThePromptFileHoldsTheDirectives(t *testing.T) {
	t.Parallel()
	ctx, _ := pointerTown(t)
	path := config.SystemPromptFilePath("polecat", ctx.TownRoot, filepath.Join(ctx.TownRoot, "myrig"), "nux")
	if err := renderSystemPromptFileForSpawn("polecat", ctx.TownRoot, filepath.Join(ctx.TownRoot, "myrig"), "nux", path, config.SystemPromptTarget{}); err != nil {
		t.Fatalf("render: %v", err)
	}

	var buf bytes.Buffer
	outputRoleDirectivesWith(ctx, &buf, false, true, path)
	out := buf.String()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	want := "Directives: in your system prompt (" + strconv.Itoa(len(data)) + " bytes, sha256 " + hex.EncodeToString(sum[:])[:12] + ")"
	if !strings.Contains(out, want+"\n") {
		t.Fatalf("hook output lacks the pointer line %q:\n%s", want, out)
	}
	if strings.Count(out, "Directives: in your system prompt") != 1 {
		t.Errorf("hook output does not carry exactly one pointer line:\n%s", out)
	}
	if strings.Contains(out, "Keep it short.") || strings.Contains(out, "## Town Directives") {
		t.Errorf("hook output carries directive text while the file is in use:\n%s", out)
	}
}

// TestHookPrintsTheCappedTextWithNoPromptFile checks that with no system-prompt
// file the hook prints the directive text as it did before, with no pointer.
func TestHookPrintsTheCappedTextWithNoPromptFile(t *testing.T) {
	t.Parallel()
	ctx, text := pointerTown(t)
	var buf bytes.Buffer
	outputRoleDirectivesWith(ctx, &buf, false, true, "")
	out := buf.String()
	if !strings.Contains(out, text) || !strings.Contains(out, "## Town Directives") {
		t.Errorf("no prompt file: the hook must print the directive text:\n%s", out)
	}
	if strings.Contains(out, "in your system prompt") {
		t.Errorf("no prompt file: the hook printed a pointer:\n%s", out)
	}
}

// TestHookPrintsTheCappedTextWhenThePromptFileLacksTheSection checks the fallback:
// a prompt file that does not hold the directive section must not earn a
// pointer, so the hook prints the directive text.
func TestHookPrintsTheCappedTextWhenThePromptFileLacksTheSection(t *testing.T) {
	t.Parallel()
	ctx, text := pointerTown(t)
	stale := filepath.Join(ctx.TownRoot, "stale-prompt.md")
	writeDirectiveFile(t, stale, "an older prompt with no directive section\n")
	var buf bytes.Buffer
	outputRoleDirectivesWith(ctx, &buf, false, true, stale)
	out := buf.String()
	if strings.Contains(out, "in your system prompt") {
		t.Errorf("a prompt file lacking the section earned a pointer:\n%s", out)
	}
	if !strings.Contains(out, text) {
		t.Errorf("a prompt file lacking the section: the hook must print the directive text:\n%s", out)
	}
}

// TestPlainPrimeKeepsTheFullTextWithAPromptFile checks that a plain `gt prime`
// (not hook mode, so not capped) still shows the directives in full even when the
// prompt file holds them: only the hook trades the text for a pointer.
func TestPlainPrimeKeepsTheFullTextWithAPromptFile(t *testing.T) {
	t.Parallel()
	ctx, text := pointerTown(t)
	path := config.SystemPromptFilePath("polecat", ctx.TownRoot, filepath.Join(ctx.TownRoot, "myrig"), "nux")
	if err := renderSystemPromptFileForSpawn("polecat", ctx.TownRoot, filepath.Join(ctx.TownRoot, "myrig"), "nux", path, config.SystemPromptTarget{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	var buf bytes.Buffer
	outputRoleDirectivesWith(ctx, &buf, false, false, path)
	out := buf.String()
	if strings.Contains(out, "in your system prompt") || !strings.Contains(out, text) {
		t.Errorf("plain prime must show the directive text, not a pointer:\n%s", out)
	}
}

// TestDirectivePointerTracksTheFileBytes checks that the pointer is the hash of
// the file as it stands: rewriting the file changes the pointer to the new bytes
// (gt-uhzl7).
func TestDirectivePointerTracksTheFileBytes(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "system-prompt.md")
	pointerFor := func(body string) string {
		t.Helper()
		writeDirectiveFile(t, file, body)
		return directivePointer(file)
	}
	first := pointerFor("\n## Directive: town _common\n\nKeep it short.\n")
	second := pointerFor("\n## Directive: town _common\n\nKeep it longer than before.\n")
	if first == "" || second == "" {
		t.Fatalf("pointers %q and %q, want both set", first, second)
	}
	if first == second {
		t.Errorf("the pointer did not change with the file: %q", first)
	}
	data := []byte("\n## Directive: town _common\n\nKeep it longer than before.\n")
	sum := sha256.Sum256(data)
	want := "Directives: in your system prompt (" + strconv.Itoa(len(data)) + " bytes, sha256 " + hex.EncodeToString(sum[:])[:12] + ")"
	if second != want {
		t.Errorf("pointer = %q, want %q", second, want)
	}
}

// TestDirectivePointerRefusesEveryBrokenInput drives each early return of
// directivePointer: an empty path, a missing file, a directory, and a file with
// no directive section. Each must answer "" so the hook prints the capped text,
// never a pointer to a file that does not carry the directives (gt-uhzl7).
func TestDirectivePointerRefusesEveryBrokenInput(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	dirPrompt := filepath.Join(town, "prompt-dir")
	if err := os.MkdirAll(dirPrompt, 0o755); err != nil {
		t.Fatal(err)
	}
	noSection := filepath.Join(town, "prompt-plain.md")
	writeDirectiveFile(t, noSection, "no directives here\n")
	cases := []struct {
		name, path string
	}{
		{"empty path", ""},
		{"missing file", filepath.Join(town, "no-such-prompt.md")},
		{"a directory", dirPrompt},
		{"a file without the directive section", noSection},
	}
	for _, tc := range cases {
		if got := directivePointer(tc.path); got != "" {
			t.Errorf("%s: directivePointer = %q, want no pointer", tc.name, got)
		}
	}
}
