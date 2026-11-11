package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeSeedSettingsFile writes content to rel under town, making its directories.
func writeSeedSettingsFile(t *testing.T, town, rel, content string) {
	t.Helper()
	p := filepath.Join(town, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedSettingsTown is a host town whose registry names alpha and beta and that
// holds every file of the town copy's allowlist, beside the files that must stay
// on the host: daemon.env, a backup copy, a key, a settings file no row names,
// and a file under themes/ that is not a theme (gt-pno1e6).
func seedSettingsTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	writeTownStateFileContent(t, town, "mayor/rigs.json", rigsJSON)
	writeSeedSettingsFile(t, town, "mayor/town.json", rigsJSON)
	writeSeedSettingsFile(t, town, "settings/config.json", `{"daemon":{"supervisor":"launchd"}}`)
	writeSeedSettingsFile(t, town, "settings/models.json", `{"profiles":{}}`)
	writeSeedSettingsFile(t, town, "settings/placement.json", `{}`)
	writeSeedSettingsFile(t, town, "settings/regime-events.json", `[]`)
	writeSeedSettingsFile(t, town, "settings/themes/gastown-names.txt", "furiosa-two\n")
	writeSeedSettingsFile(t, town, "directives/_common.md", "be kind\n")
	writeSeedSettingsFile(t, town, "directives/crew/deep.md", "deeper\n")
	writeSeedSettingsFile(t, town, ".beads/config.yaml", "issue-prefix: gt\n")
	writeSeedSettingsFile(t, town, "alpha/settings/config.json", `{"landing":{"branch":"main"}}`)
	writeSeedSettingsFile(t, town, "beta/settings/config.json", `{}`)

	writeSeedSettingsFile(t, town, "settings/daemon.env", "GT_TOKEN=secret\n")
	writeSeedSettingsFile(t, town, "settings/models.json.bak-20260101", `{"old":true}`)
	writeSeedSettingsFile(t, town, "settings/extra.json", `{"new":true}`)
	writeSeedSettingsFile(t, town, "settings/themes/notes.md", "not a theme\n")
	writeSeedSettingsFile(t, town, "settings/themes/gastown-names.txt.lock", "")
	writeSeedSettingsFile(t, town, "settings/themes/secret.key", "k")
	writeSeedSettingsFile(t, town, "alpha/settings/daemon.env", "GT_TOKEN=secret\n")
	return town
}

// TestSeedCutoverPrepareSendsExactlyTheAllowlistsSharedRows: the live copy's
// settings step sends the shared rows of internal/townstate.TownCopyRows, byte
// for byte, and nothing else: no daemon row, no secret, no backup, no unlisted
// settings file, no file under themes/ that is not a theme (gt-pno1e6).
func TestSeedCutoverPrepareSendsExactlyTheAllowlistsSharedRows(t *testing.T) {
	t.Parallel()
	town := seedSettingsTown(t)

	c, err := seedCutoverPrepare(town, nil)
	if err != nil {
		t.Fatalf("seedCutoverPrepare: %v", err)
	}
	got := seedCutoverRels(c.files)
	want := []string{
		"settings/config.json",
		"settings/models.json",
		"settings/placement.json",
		"settings/regime-events.json",
		"settings/themes/gastown-names.txt",
		"alpha/settings/config.json",
		"beta/settings/config.json",
		"directives/_common.md",
		"directives/crew/deep.md",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the live copy sends %v, want exactly the allowlist's shared rows %v", got, want)
	}
	if len(c.absent) != 0 {
		t.Errorf("absent = %v, want none: every shared row has a file", c.absent)
	}
	for _, f := range c.files {
		if f.rel == seedCutoverTownRel {
			if strings.Contains(string(f.data), "supervisor") {
				t.Errorf("the town settings file is sent with its supervisor: %s", f.data)
			}
			continue
		}
		if want := mustReadSeedFile(t, town, f.rel); string(f.data) != want {
			t.Errorf("%s is sent as %q, want the host's bytes %q", f.rel, f.data, want)
		}
	}
}

// TestSeedCutoverPrepareWithoutAThemesDirectory: a town with no settings/themes
// directory sends its settings file and no theme, which is not an error: the pod
// then names from the built-in themes alone.
func TestSeedCutoverPrepareWithoutAThemesDirectory(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeSeedSettingsFile(t, town, "settings/config.json", `{}`)

	c, err := seedCutoverPrepare(town, nil)
	if err != nil {
		t.Fatalf("seedCutoverPrepare: %v", err)
	}
	if len(c.files) != 1 || c.files[0].rel != "settings/config.json" {
		t.Errorf("files = %v, want only settings/config.json", seedCutoverRels(c.files))
	}
}

// A settings row that is a link is refused, not skipped: the copy would otherwise
// send less than the town has and say nothing.
func TestSeedCutoverPrepareRefusesALinkedSettingsFile(t *testing.T) {
	t.Parallel()
	town := seedSettingsTown(t)
	writeSeedSettingsFile(t, town, "elsewhere.json", `{}`)
	target := filepath.Join(town, "elsewhere.json")
	models := filepath.Join(town, "settings", "models.json")
	if err := os.Remove(models); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, models); err != nil {
		t.Fatalf("creating the link: %v", err)
	}

	_, err := seedCutoverPrepare(town, nil)
	if err == nil || !strings.Contains(err.Error(), "settings/models.json") {
		t.Fatalf("seedCutoverPrepare = %v, want a refusal naming settings/models.json", err)
	}
}

// A themes path that cannot be listed is a refusal: a custom theme left out of
// the copy would make the pod refuse the rig's slings with no file to restore.
// A regular file where the directory belongs fails the listing as an unreadable
// directory does, and it fails the same way for a process running as root.
func TestSeedCutoverPrepareRefusesAThemesPathThatIsNotADirectory(t *testing.T) {
	t.Parallel()
	town := seedSettingsTown(t)
	themes := filepath.Join(town, "settings", "themes")
	if err := os.RemoveAll(themes); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(themes, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := seedCutoverPrepare(town, nil)
	if err == nil || !strings.Contains(err.Error(), "settings/themes") {
		t.Fatalf("seedCutoverPrepare = %v, want a refusal naming settings/themes", err)
	}
}

func mustReadSeedFile(t *testing.T, town, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(town, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func seedCutoverRels(files []seedCutoverFile) []string {
	rels := make([]string, 0, len(files))
	for _, f := range files {
		rels = append(rels, f.rel)
	}
	return rels
}
