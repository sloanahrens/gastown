package cmd

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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

// seedSettingsTown is a host town with the settings files the pod carried go
// missing (models, placement, regime events, a custom theme) beside the ones the
// copy must leave out (daemon.env, a .bak copy, a theme's .lock, a non-theme file).
func seedSettingsTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	writeSeedSettingsFile(t, town, "mayor/rigs.json", `{"version":1,"rigs":{}}`)
	writeSeedSettingsFile(t, town, "settings/config.json", `{}`)
	writeSeedSettingsFile(t, town, "settings/models.json", `{"profiles":{}}`)
	writeSeedSettingsFile(t, town, "settings/placement.json", `{}`)
	writeSeedSettingsFile(t, town, "settings/regime-events.json", `[]`)
	writeSeedSettingsFile(t, town, "settings/themes/gastown-names.txt", "furiosa-two\n")
	writeSeedSettingsFile(t, town, "settings/daemon.env", "GT_TOKEN=secret\n")
	writeSeedSettingsFile(t, town, "settings/models.json.bak-20260101", `{"old":true}`)
	writeSeedSettingsFile(t, town, "settings/themes/gastown-names.txt.lock", "")
	writeSeedSettingsFile(t, town, "settings/themes/notes.md", "not a theme\n")
	return town
}

// The live copy's settings step sends each town settings file the pod needs and
// each custom theme, byte for byte, and leaves out daemon.env, the backup and
// lock copies and the files that are not themes.
func TestSeedCutoverPrepareCopiesEveryTownSettingsFile(t *testing.T) {
	t.Parallel()
	town := seedSettingsTown(t)

	c, err := seedCutoverPrepare(town, nil)
	if err != nil {
		t.Fatalf("seedCutoverPrepare: %v", err)
	}
	sent := map[string]seedCutoverFile{}
	for _, f := range c.files {
		sent[f.rel] = f
	}

	for _, rel := range []string{
		"settings/config.json",
		"settings/models.json",
		"settings/placement.json",
		"settings/regime-events.json",
		"settings/themes/gastown-names.txt",
	} {
		f, ok := sent[rel]
		if !ok {
			t.Errorf("the live copy does not send %s", rel)
			continue
		}
		if rel == "settings/config.json" {
			// The town's config file is sent with its supervisor removed, so its
			// bytes are not the host's.
			continue
		}
		if want := mustReadSeedFile(t, town, rel); string(f.data) != want {
			t.Errorf("%s is sent as %q, want the host's bytes %q", rel, f.data, want)
		}
	}
	for _, rel := range []string{
		"settings/daemon.env",
		"settings/models.json.bak-20260101",
		"settings/themes/gastown-names.txt.lock",
		"settings/themes/notes.md",
	} {
		if _, ok := sent[rel]; ok {
			t.Errorf("the live copy sends %s, want it left out", rel)
		}
	}
}

// A town with no settings/themes directory sends no theme and is not an error:
// the pod then names from the built-in themes alone.
func TestSeedCutoverPrepareWithoutAThemesDirectory(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeSeedSettingsFile(t, town, "mayor/rigs.json", `{"version":1,"rigs":{}}`)
	writeSeedSettingsFile(t, town, "settings/config.json", `{}`)

	c, err := seedCutoverPrepare(town, nil)
	if err != nil {
		t.Fatalf("seedCutoverPrepare: %v", err)
	}
	if len(c.files) != 1 || c.files[0].rel != "settings/config.json" {
		t.Errorf("files = %v, want only settings/config.json", seedCutoverRels(c.files))
	}
}

// A settings entry that is a link is refused, not skipped: the copy would
// otherwise send less than the town has and say nothing.
func TestSeedCutoverPrepareRefusesALinkedSettingsFile(t *testing.T) {
	t.Parallel()
	town := seedSettingsTown(t)
	target := filepath.Join(town, "settings", "models.json")
	link := filepath.Join(town, "settings", "linked.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("creating the link: %v", err)
	}

	_, err := seedCutoverPrepare(town, nil)
	if err == nil || !strings.Contains(err.Error(), "settings/linked.json") {
		t.Fatalf("seedCutoverPrepare = %v, want a refusal naming settings/linked.json", err)
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

// A settings directory that cannot be listed is a refusal, not an empty copy: the
// claim would otherwise miss every file the directory holds. The lister is the
// fake here, since a real unreadable settings directory also hides config.json
// that the copy reads first.
func TestSeedCutoverSettingNamesRefusesAnUnlistableSettingsDirectory(t *testing.T) {
	t.Parallel()
	listErr := errors.New("listing refused")
	_, err := seedCutoverSettingNames(t.TempDir(), func(string) ([]fs.DirEntry, error) {
		return nil, listErr
	})
	if !errors.Is(err, listErr) || !strings.Contains(err.Error(), "listing the town's settings directory") {
		t.Fatalf("seedCutoverSettingNames = %v, want a refusal naming the settings directory", err)
	}
}

// A settings directory that does not exist holds no settings to copy, which is
// not a refusal.
func TestSeedCutoverSettingNamesWithoutASettingsDirectory(t *testing.T) {
	t.Parallel()
	rels, err := seedCutoverSettingNames(t.TempDir(), os.ReadDir)
	if err != nil || len(rels) != 0 {
		t.Fatalf("seedCutoverSettingNames = %v, %v; want no names and no error", rels, err)
	}
}

// An unreadable themes directory is a refusal too: a custom theme left out of the
// copy would make the pod refuse the rig's slings with no file to restore.
func TestSeedCutoverPrepareRefusesAnUnreadableThemesDirectory(t *testing.T) {
	t.Parallel()
	town := seedSettingsTown(t)
	chmodUnreadable(t, filepath.Join(town, "settings", "themes"))

	_, err := seedCutoverPrepare(town, nil)
	if err == nil || !strings.Contains(err.Error(), "listing the host's custom themes") {
		t.Fatalf("seedCutoverPrepare = %v, want a refusal naming the themes directory", err)
	}
}

// chmodUnreadable makes dir unlistable (write-only, no read bit), and restores its
// mode when the test ends so the temporary town can be removed. A process that can
// still list the directory cannot test the refusal.
func chmodUnreadable(t *testing.T, dir string) {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	original := info.Mode().Perm()
	if err := os.Chmod(dir, 0o200); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, original); err != nil {
			t.Errorf("restoring %s: %v", dir, err)
		}
	})
	if _, err := os.ReadDir(dir); err == nil {
		t.Fatalf("%s is still readable at mode 0200: this test needs a process that cannot read it", dir)
	}
}
