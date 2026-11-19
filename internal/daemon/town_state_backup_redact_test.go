package daemon

import (
	"bytes"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// townStateRedactTree is a fake town whose settings carry a planted credential
// in one JSON file and one text file, beside clean files and a refused secret.
func townStateRedactTree() fstest.MapFS {
	return fstest.MapFS{
		"mayor/town.json":              {Data: []byte(`{"name":"town"}`)},
		"settings/config.json":         {Data: []byte(`{"api_key":"PLANTED-123","model":"m"}`)},
		"settings/models.json":         {Data: []byte(`{"model":"plain"}`)},
		"settings/placement.json":      {Data: []byte(`{}`)},
		"settings/regime-events.json":  {Data: []byte(`[]`)},
		"settings/themes/dark.json":    {Data: []byte(`{"bg":"#000"}`)},
		"settings/themes/notes.txt":    {Data: []byte("DEEPSEEK_API_KEY=PLANTED-123\n")},
		"gastown/settings/config.json": {Data: []byte(`{"rig":true}`)},
		"gastown/.beads/config.yaml":   {Data: []byte("prefix: gt\n")},
	}
}

// TestTownStateBackupRedactStoresRedactedJSON: a JSON export carrying a
// credential is stored with the marker in the value's place, counted and named,
// and no stored byte holds the value.
func TestTownStateBackupRedactStoresRedactedJSON(t *testing.T) {
	t.Parallel()
	snap, err := collectTownState(townStateRedactTree(), []string{"gastown"})
	if err != nil {
		t.Fatalf("collectTownState: %v", err)
	}

	const want = `{"api_key":"<redacted>","model":"m"}`
	if got := string(snap.Files["settings/config.json"]); got != want {
		t.Errorf("stored settings/config.json = %q\nwant                     %q", got, want)
	}
	for name, data := range snap.Files {
		if strings.Contains(string(data), "PLANTED-123") {
			t.Errorf("stored %s holds the planted credential", name)
		}
	}
	if !reflect.DeepEqual(snap.Redacted, []string{"settings/config.json"}) {
		t.Errorf("redacted = %v, want [settings/config.json]", snap.Redacted)
	}
	if snap.Redactions != 1 {
		t.Errorf("redactions = %d, want 1", snap.Redactions)
	}
	if got := townStateCredentialNote(snap); !strings.Contains(got, "redacted 1 credential value(s) in 1 JSON file(s): settings/config.json") {
		t.Errorf("note = %q, want it to count and name the redacted file", got)
	}
}

// TestTownStateBackupRedactRefusesCredentialText: a non-JSON export holding a
// credential assignment is not copied and appears in Refused.
func TestTownStateBackupRedactRefusesCredentialText(t *testing.T) {
	t.Parallel()
	snap, err := collectTownState(townStateRedactTree(), []string{"gastown"})
	if err != nil {
		t.Fatalf("collectTownState: %v", err)
	}
	const refused = "settings/themes/notes.txt"
	if _, ok := snap.Files[refused]; ok {
		t.Errorf("%s was copied; a file carrying a credential must be refused", refused)
	}
	if !reflect.DeepEqual(snap.Refused, []string{refused}) {
		t.Errorf("refused = %v, want [%s]", snap.Refused, refused)
	}
	if got := townStateCredentialNote(snap); !strings.Contains(got, "refused 1 secret-bearing file(s): "+refused) {
		t.Errorf("note = %q, want it to count and name the refused file", got)
	}
}

// TestTownStateBackupRedactCopiesCleanFileVerbatim: a file without credentials
// is copied byte for byte, JSON and non-JSON alike.
func TestTownStateBackupRedactCopiesCleanFileVerbatim(t *testing.T) {
	t.Parallel()
	tree := townStateRedactTree()
	snap, err := collectTownState(tree, []string{"gastown"})
	if err != nil {
		t.Fatalf("collectTownState: %v", err)
	}
	for _, name := range []string{"settings/themes/dark.json", "gastown/.beads/config.yaml"} {
		if got, want := string(snap.Files[name]), string(tree[name].Data); got != want {
			t.Errorf("%s = %q, want the file as read %q", name, got, want)
		}
		if containsString(snap.Redacted, name) || containsString(snap.Refused, name) {
			t.Errorf("%s was redacted or refused; a clean file is copied untouched", name)
		}
	}
	if snap.Redactions != 1 {
		t.Errorf("redactions = %d, want 1 (only settings/config.json)", snap.Redactions)
	}
}

// TestTownStateBackupRedactRefusesUnparsableJSON: a JSON export the redactor
// cannot parse is refused like a credential-bearing file, never copied as-is.
func TestTownStateBackupRedactRefusesUnparsableJSON(t *testing.T) {
	t.Parallel()
	tree := townStateRedactTree()
	tree["settings/models.json"] = &fstest.MapFile{Data: []byte(`{"model":`)}
	snap, err := collectTownState(tree, []string{"gastown"})
	if err != nil {
		t.Fatalf("collectTownState: %v", err)
	}
	const refused = "settings/models.json"
	if _, ok := snap.Files[refused]; ok {
		t.Errorf("%s was copied; invalid JSON must be refused", refused)
	}
	if !containsString(snap.Refused, refused) {
		t.Errorf("refused = %v, want %s among them", snap.Refused, refused)
	}
}

// TestTownStateBackupRedactLogsOnACleanTick: a tick that pushes successfully
// still logs how many files were redacted and refused, by path, and no value.
func TestTownStateBackupRedactLogsOnACleanTick(t *testing.T) {
	t.Parallel()
	d, f, town := townStateDaemon(t)
	var logs bytes.Buffer
	d.logger = log.New(&logs, "", 0)
	rec := notifyfake.New()
	d.notifier = rec
	d.townStateFS = func(string) fs.FS { return townStateRedactTree() }
	gitRepo := filepath.Join(town, ".dolt-archive", "git")
	seedTownStateOrigin(t, f, town, gitRepo)

	d.syncTownStateBackup()

	if len(rec.Escalations()) != 0 {
		t.Fatalf("a successful tick raised an alert")
	}
	if strings.Contains(logs.String(), "PLANTED-123") {
		t.Errorf("the log holds the planted credential")
	}
	if !strings.Contains(logs.String(), "redacted 1 credential value(s) in 1 JSON file(s): settings/config.json") {
		t.Errorf("log = %q, want it to count and name the redacted file", logs.String())
	}
	if !strings.Contains(logs.String(), "refused 1 secret-bearing file(s): settings/themes/notes.txt") {
		t.Errorf("log = %q, want it to count and name the refused file", logs.String())
	}
}

// TestTownStateBackupRedactLogAndAlertHoldNoValue drives a whole tick that
// redacts one file and refuses another and then fails at the push, so the log
// and the alert text both exist. Neither, and no stored byte, holds the value;
// both count and name the files.
func TestTownStateBackupRedactLogAndAlertHoldNoValue(t *testing.T) {
	t.Parallel()
	d, _, town := townStateDaemon(t)
	var logs bytes.Buffer
	d.logger = log.New(&logs, "", 0)
	rec := notifyfake.New()
	d.notifier = rec
	d.townStateFS = func(string) fs.FS { return townStateRedactTree() }
	gitRepo := filepath.Join(town, ".dolt-archive", "git")
	if err := os.MkdirAll(gitRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	initEmptyGitRepo(t, d, gitRepo)

	d.syncTownStateBackup()

	stored, err := os.ReadFile(filepath.Join(gitRepo, townStateBackupDir, "settings", "config.json"))
	if err != nil {
		t.Fatalf("reading the stored settings: %v", err)
	}
	if !strings.Contains(string(stored), "<redacted>") {
		t.Errorf("stored settings = %q, want the marker in place of the value", stored)
	}
	escalations := rec.Escalations()
	if len(escalations) != 1 {
		t.Fatalf("escalations = %d, want one (the push failure)", len(escalations))
	}
	alert := escalations[0].Escalation

	surfaces := map[string]string{
		"stored settings": string(stored),
		"log":             logs.String(),
		"alert text":      alert.Reason,
	}
	for name, text := range surfaces {
		if strings.Contains(text, "PLANTED-123") {
			t.Errorf("%s holds the planted credential", name)
		}
	}
	for _, text := range []string{logs.String(), alert.Reason} {
		if !strings.Contains(text, "redacted 1 credential value(s) in 1 JSON file(s): settings/config.json") {
			t.Errorf("text = %q, want it to count and name the redacted file", text)
		}
		if !strings.Contains(text, "refused 1 secret-bearing file(s): settings/themes/notes.txt") {
			t.Errorf("text = %q, want it to count and name the refused file", text)
		}
	}
}
