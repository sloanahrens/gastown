package forgejocut

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

const settingsPath = "/town/rig/settings/config.json"

// settingsCutover is a cutover over a rig whose options carry what the block is
// made from.
func settingsCutover(fsys Files, dry bool) (*Cutover, *[]string) {
	c, log := repointCutover(newMemFiles(), &repointGit{}, dry)
	c.Files = fsys
	c.Opts.GateWorkflow = "gate"
	c.Opts.BotPrefix = "bot-"
	c.Opts.PromoteTarget = "git@github.example:acme/rig.git"
	c.Opts.PromoteKey = "/home/op/.config/gt/promote-rig.key"
	return c, log
}

// forgejoOf decodes merge_queue.forgejo of a settings document through the real
// config type, refusing a key the type does not know.
func forgejoOf(t *testing.T, doc []byte) config.ForgejoConfig {
	t.Helper()
	var file struct {
		MergeQueue struct {
			Forgejo json.RawMessage `json:"forgejo"`
		} `json:"merge_queue"`
	}
	if err := json.Unmarshal(doc, &file); err != nil {
		t.Fatalf("settings do not parse: %v\n%s", err, doc)
	}
	dec := json.NewDecoder(bytes.NewReader(file.MergeQueue.Forgejo))
	dec.DisallowUnknownFields()
	var got config.ForgejoConfig
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("merge_queue.forgejo does not decode through config.ForgejoConfig: %v\n%s", err, file.MergeQueue.Forgejo)
	}
	return got
}

// The block the step writes parses with the real type, key for key, and carries
// the values the options name. A renamed key turns this red.
func TestSettingsBlockDecodesThroughConfigForgejoConfig(t *testing.T) {
	t.Parallel()
	fsys := newMemFiles()
	fsys.addDir("/town/rig/settings")
	c, _ := settingsCutover(fsys, false)

	if err := c.WriteSettings(); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	got := forgejoOf(t, fsys.files[settingsPath])
	want := config.ForgejoConfig{
		RemoteURL:      forgejoURL,
		GateWorkflow:   "gate",
		Bots:           map[string]string{"polecat": "bot-polecat", "landing": "bot-landing", "registry": "bot-registry"},
		PromoteTarget:  "git@github.example:acme/rig.git",
		PromoteKeyFile: "/home/op/.config/gt/promote-rig.key",
	}
	if got.RemoteURL != want.RemoteURL || got.GateWorkflow != want.GateWorkflow ||
		got.PromoteTarget != want.PromoteTarget || got.PromoteKeyFile != want.PromoteKeyFile ||
		len(got.Bots) != len(want.Bots) || got.MirrorTarget != "" {
		t.Fatalf("decoded block = %+v, want %+v", got, want)
	}
	for role, login := range want.Bots {
		if got.Bots[role] != login {
			t.Errorf("bot %s = %q, want %q", role, got.Bots[role], login)
		}
	}
}

// settingsBefore is a rig settings file with keys in a deliberate order, an old
// forgejo block between two other keys, and text a re-encoder would change.
const settingsBefore = `{
  "type": "rig-settings",
  "version": 1,
  "note": "caf\u00e9 <rig>",
  "merge_queue": {
    "enabled": true,
    "forgejo": {
      "remote_url": "https://github.example/acme/rig.git"
    },
    "retry": 3
  },
  "theme": {}
}
`

// Only merge_queue.forgejo changes: the other keys keep their order, their text and
// their layout, and the new block sits where the old one was.
func TestSettingsPreservesOtherKeys(t *testing.T) {
	t.Parallel()
	fsys := newMemFiles()
	fsys.addFile(settingsPath, settingsBefore, 0o640)
	c, _ := settingsCutover(fsys, false)

	if err := c.WriteSettings(); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	got := string(fsys.files[settingsPath])
	want := strings.Replace(settingsBefore, `"remote_url": "https://github.example/acme/rig.git"`,
		`"remote_url": "`+forgejoURL+`",
      "gate_workflow": "gate",
      "bots": {
        "polecat": "bot-polecat",
        "landing": "bot-landing",
        "registry": "bot-registry"
      },
      "promote_target": "git@github.example:acme/rig.git",
      "promote_key_file": "/home/op/.config/gt/promote-rig.key"`, 1)
	if got != want {
		t.Errorf("settings =\n%s\nwant\n%s", got, want)
	}
	if fsys.modes[settingsPath] != 0o640 {
		t.Errorf("settings mode = %v, want 0640 kept", fsys.modes[settingsPath])
	}
}

// A block that already means the same thing, in another key order and layout, is
// neither rewritten nor backed up. Always writing turns this red.
func TestSettingsAlreadyMatchingIsUntouched(t *testing.T) {
	t.Parallel()
	fsys := newMemFiles()
	fsys.addFile(settingsPath, `{"merge_queue":{"forgejo":{"promote_key_file":"/home/op/.config/gt/promote-rig.key","promote_target":"git@github.example:acme/rig.git","bots":{"registry":"bot-registry","landing":"bot-landing","polecat":"bot-polecat"},"gate_workflow":"gate","remote_url":"`+forgejoURL+`"}}}`, 0o600)
	c, log := settingsCutover(fsys, false)

	if err := c.WriteSettings(); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	if len(fsys.writes) != 0 {
		t.Errorf("wrote %q for a matching block", fsys.writes)
	}
	if !strings.Contains(strings.Join(*log, "\n"), "merge_queue.forgejo already matches") {
		t.Errorf("log does not say the block matched: %q", *log)
	}
}

// The file is copied before it is edited: the backup holds the original bytes and
// is written first, then the replacement beside the file, then the rename.
func TestSettingsBacksUpBeforeEditing(t *testing.T) {
	t.Parallel()
	fsys := newMemFiles()
	fsys.addFile(settingsPath, settingsBefore, 0o600)
	c, _ := settingsCutover(fsys, false)

	if err := c.WriteSettings(); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	backup := settingsPath + ".bak-20261101-140509"
	if got := string(fsys.files[backup]); got != settingsBefore {
		t.Errorf("backup =\n%s\nwant the original", got)
	}
	if want := []string{backup, settingsPath + ".new-20261101-140509"}; !slices.Equal(fsys.writes, want) {
		t.Errorf("writes = %q, want the backup first, then %q", fsys.writes, want)
	}
}

// A dry run logs the backup and the write it would make and changes nothing.
func TestSettingsDryRun(t *testing.T) {
	t.Parallel()
	fsys := newMemFiles()
	fsys.addFile(settingsPath, settingsBefore, 0o600)
	c, log := settingsCutover(fsys, true)

	if err := c.WriteSettings(); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	if len(fsys.writes) != 0 || len(fsys.renames) != 0 {
		t.Errorf("a dry run wrote %q and renamed %q", fsys.writes, fsys.renames)
	}
	joined := strings.Join(*log, "\n")
	for _, want := range []string{"would back up " + settingsPath, "would write merge_queue.forgejo to " + settingsPath} {
		if !strings.Contains(joined, want) {
			t.Errorf("log lacks %q:\n%s", want, joined)
		}
	}
}

// A settings file that does not exist yet is created with its type, version and
// merge_queue, and there is nothing to back up.
func TestSettingsCreatesAMissingFile(t *testing.T) {
	t.Parallel()
	fsys := newMemFiles()
	c, log := settingsCutover(fsys, false)

	if err := c.WriteSettings(); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	var file config.RigSettings
	if err := json.Unmarshal(fsys.files[settingsPath], &file); err != nil {
		t.Fatalf("created settings do not parse: %v", err)
	}
	if file.Type != "rig-settings" || file.Version != 1 || file.MergeQueue == nil || file.MergeQueue.Forgejo == nil {
		t.Errorf("created settings = %+v", file)
	}
	for p := range fsys.files {
		if strings.Contains(p, ".bak-") {
			t.Errorf("a backup %s was made of a file that did not exist", p)
		}
	}
	if !strings.Contains(strings.Join(*log, "\n"), "no "+settingsPath+" to back up") {
		t.Errorf("log = %q", *log)
	}
}

// A file the step cannot edit is refused with the file untouched: not JSON, a
// merge_queue that is not an object, a read that fails.
func TestSettingsRefusesWhatItCannotEdit(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		content string
		fail    bool
		want    string
	}{
		"not json":               {content: `{"merge_queue": `, want: "is not usable"},
		"merge_queue not object": {content: `{"merge_queue": 5}`, want: "is not usable"},
		"unreadable":             {content: `{}`, fail: true, want: "could not read"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mem := newMemFiles()
			mem.addFile(settingsPath, tc.content, 0o600)
			c, _ := settingsCutover(mem, false)
			if tc.fail {
				c.Files = failFiles{memFiles: mem, fail: map[string]error{"read " + settingsPath: errDisk}}
			}

			err := c.WriteSettings()

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("WriteSettings error = %v, want one containing %q", err, tc.want)
			}
			if len(mem.writes) != 0 {
				t.Errorf("wrote %q for a file it refused", mem.writes)
			}
		})
	}
}

// withForgejoBlock names which part it could not walk: the document, the
// merge_queue, or the block it was given.
func TestWithForgejoBlockRefusals(t *testing.T) {
	t.Parallel()
	if _, _, _, err := withForgejoBlock([]byte(`{`), json.RawMessage(`{}`)); err == nil {
		t.Error("withForgejoBlock edited a document that is not JSON")
	}
	if _, _, _, err := withForgejoBlock([]byte(`{"merge_queue": 5}`), json.RawMessage(`{}`)); err == nil {
		t.Error("withForgejoBlock went into a merge_queue that is not an object")
	}
	if _, _, _, err := withForgejoBlock([]byte(`{}`), json.RawMessage(`{`)); err == nil {
		t.Error("withForgejoBlock wrote a block that is not JSON")
	}
}

// A backup or a replacement that cannot be written ends the step with the cause
// named and config.json whole.
func TestSettingsWriteFailuresLeaveTheFileWhole(t *testing.T) {
	t.Parallel()
	for name, fail := range map[string]string{
		"backup":      "write " + settingsPath + ".bak-20261101-140509",
		"replacement": "write " + settingsPath + ".new-20261101-140509",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mem := newMemFiles()
			mem.addFile(settingsPath, settingsBefore, 0o600)
			c, _ := settingsCutover(failFiles{memFiles: mem, fail: map[string]error{fail: errDisk}}, false)

			err := c.WriteSettings()

			if !errors.Is(err, errDisk) {
				t.Errorf("WriteSettings error = %v, want the disk error", err)
			}
			if got := string(mem.files[settingsPath]); got != settingsBefore {
				t.Errorf("config.json changed to\n%s", got)
			}
		})
	}
}
