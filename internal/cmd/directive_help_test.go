package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// directiveLayer is one layer of the directive layout, as the help documents it.
type directiveLayer struct {
	name      string // how the help names the layer
	newDir    string // the layer's own directory, with the help's placeholders
	legacyDir string // the pre-gt-oz73l directory the reader falls back to
}

// documentedDirectiveLayers is the layout the help documents, written out here
// rather than read back through the config helpers that build the help, so a
// helper that moves a directory leaves these tests red (gt-v1ln5).
var documentedDirectiveLayers = []directiveLayer{
	{
		name:      "central",
		newDir:    "<townRoot>/gastown/mayor/rig/directives",
		legacyDir: "<townRoot>/directives",
	},
	{
		name:      "rig",
		newDir:    "<townRoot>/<rig>/mayor/rig/.gastown/directives",
		legacyDir: "<townRoot>/<rig>/directives",
	},
}

// TestDirectiveHelp_NamesTheResolvedLayerPaths: `gt directive --help` names the
// central and rig layer directories that the reader resolves and that
// 'gt directive list' prints (gt-v1ln5).
func TestDirectiveHelp_NamesTheResolvedLayerPaths(t *testing.T) {
	t.Parallel()

	for _, layer := range documentedDirectiveLayers {
		want := layer.newDir + "/<role>.md"
		if !strings.Contains(directiveCmd.Long, want) {
			t.Errorf("help does not name %q:\n%s", want, directiveCmd.Long)
		}
	}
}

// TestDirectiveHelp_DocumentsTheLayoutTheReaderResolves: a directive written in
// a directory the help names is a file the reader resolves, so the help cannot
// document a layout the config package does not read (gt-v1ln5).
func TestDirectiveHelp_DocumentsTheLayoutTheReaderResolves(t *testing.T) {
	t.Parallel()

	const role, rig = "polecat", "alpha"
	town := t.TempDir()
	var want []string
	for _, layer := range documentedDirectiveLayers {
		dir := expandHelpPlaceholders(layer.newDir, town, rig)
		path := filepath.Join(dir, role+".md")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
		if err := os.WriteFile(path, []byte("policy"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		want = append(want, path)
	}
	got := config.DirectivePaths(role, town, rig)
	for _, path := range want {
		if !slices.Contains(got, path) {
			t.Errorf("the reader does not resolve %s, which the help documents; it resolves %v", path, got)
		}
	}
}

// expandHelpPlaceholders answers the placeholders the help uses with a town on
// disk.
func expandHelpPlaceholders(dir, town, rig string) string {
	return strings.NewReplacer("<townRoot>", town, "<rig>", rig).Replace(dir)
}

// TestDirectiveHelp_NamesTheLegacyDirectoriesOnlyAsFallback: the pre-gt-oz73l
// directories still reach an operator, but only in the paragraph that says a
// file there is read as a fallback, never as the layout itself (gt-ivnbx).
func TestDirectiveHelp_NamesTheLegacyDirectoriesOnlyAsFallback(t *testing.T) {
	t.Parallel()

	for _, layer := range documentedDirectiveLayers {
		var named bool
		for _, para := range strings.Split(directiveCmd.Long, "\n\n") {
			if !strings.Contains(para, layer.legacyDir) {
				continue
			}
			named = true
			if !strings.Contains(para, "Legacy fallback") {
				t.Errorf("%s legacy directory named outside the fallback paragraph:\n%s", layer.name, para)
			}
		}
		if !named {
			t.Errorf("help does not name the %s legacy directory %q at all:\n%s", layer.name, layer.legacyDir, directiveCmd.Long)
		}
	}
}

// TestDirectiveHelp_PairsEachLegacyDirWithItsOwnLayer: the reader falls back
// from a layer's new directory to that same layer's legacy directory, so the
// help has to pair them the same way. Each pair stands in one sentence, which
// lets the test ask which new directory the sentence naming a legacy directory
// promises, without matching the prose around it (gt-v1ln5).
func TestDirectiveHelp_PairsEachLegacyDirWithItsOwnLayer(t *testing.T) {
	t.Parallel()

	for _, layer := range documentedDirectiveLayers {
		para := paragraphNaming(t, directiveCmd.Long, layer.legacyDir)
		sentence := sentenceNaming(t, para, layer.legacyDir)
		if !strings.Contains(sentence, layer.newDir) {
			t.Errorf("the sentence naming the %s legacy directory does not name that layer's new directory %q:\n%s",
				layer.name, layer.newDir, sentence)
		}
		for _, other := range documentedDirectiveLayers {
			if other.name == layer.name {
				continue
			}
			if strings.Contains(sentence, other.legacyDir) || strings.Contains(sentence, other.newDir) {
				t.Errorf("the sentence naming the %s legacy directory also names a %s directory:\n%s",
					layer.name, other.name, sentence)
			}
		}
	}
}

// paragraphNaming returns the one blank-line-separated paragraph of text that
// names target.
func paragraphNaming(t *testing.T, text, target string) string {
	t.Helper()

	var found []string
	for _, para := range strings.Split(text, "\n\n") {
		if strings.Contains(para, target) {
			found = append(found, para)
		}
	}
	if len(found) != 1 {
		t.Fatalf("text names %q in %d paragraphs, want exactly one:\n%s", target, len(found), text)
	}
	return found[0]
}

// sentenceNaming returns the one sentence of text that names target. The help
// wraps its lines, so whitespace is collapsed before the split at a period and
// a space.
func sentenceNaming(t *testing.T, text, target string) string {
	t.Helper()

	var found []string
	for _, sentence := range strings.Split(strings.Join(strings.Fields(text), " "), ". ") {
		if strings.Contains(sentence, target) {
			found = append(found, sentence)
		}
	}
	if len(found) != 1 {
		t.Fatalf("text names %q in %d sentences, want exactly one:\n%s", target, len(found), text)
	}
	return found[0]
}
