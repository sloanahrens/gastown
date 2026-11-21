package daemon

import (
	"bytes"
	"errors"
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

// TestTownStateBackupRedactCleanFailureKeepsTheError: a failing tick whose
// snapshot carried no credential returns the tick's own error, with no note
// appended to the alert text.
func TestTownStateBackupRedactCleanFailureKeepsTheError(t *testing.T) {
	t.Parallel()
	d, _, town := townStateDaemon(t)
	rec := notifyfake.New()
	d.notifier = rec
	tree := townStateRedactTree()
	tree["settings/config.json"] = &fstest.MapFile{Data: []byte(`{"model":"m"}`)}
	delete(tree, "settings/themes/notes.txt")
	d.townStateFS = func(string) fs.FS { return tree }
	gitRepo := filepath.Join(town, ".dolt-archive", "git")
	if err := os.MkdirAll(gitRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	initEmptyGitRepo(t, d, gitRepo)

	d.syncTownStateBackup()

	escalations := rec.Escalations()
	if len(escalations) != 1 {
		t.Fatalf("escalations = %d, want one (the push failure)", len(escalations))
	}
	reason := escalations[0].Escalation.Reason
	if strings.Contains(reason, "redacted") || strings.Contains(reason, "refused") {
		t.Errorf("alert text = %q, want no credential note on a clean snapshot", reason)
	}
	if !strings.Contains(reason, "origin") {
		t.Errorf("alert text = %q, want the push failure itself", reason)
	}
}

// TestTownStateBackupRedactRefusedPathLeavesNoStoredCopy drives one path
// through a clean tick that stores it and a credential-bearing tick that
// refuses it: the backup repository keeps no copy in its tree or index, the
// deletion rides in the refusing tick's commit, and the note names the path
// without its value.
func TestTownStateBackupRedactRefusedPathLeavesNoStoredCopy(t *testing.T) {
	t.Parallel()
	d, f, town := townStateDaemon(t)
	const exported = "gastown/.beads/config.yaml"
	tree := fstest.MapFS{
		"mayor/town.json": {Data: []byte(`{"name":"town"}`)},
		exported:          {Data: []byte("prefix: gt\n")},
	}
	d.townStateFS = func(string) fs.FS { return tree }
	gitRepo := filepath.Join(town, ".dolt-archive", "git")
	seedTownStateOrigin(t, f, town, gitRepo)
	inRepo := townStateBackupDir + "/" + exported
	stored := filepath.Join(gitRepo, townStateBackupDir, filepath.FromSlash(exported))

	d.syncTownStateBackup()
	head1 := townStateHead(t, f, gitRepo)
	if got := f.Tree(head1)[inRepo]; got != "prefix: gt\n" {
		t.Fatalf("after the clean tick, HEAD stores %s = %q; want the file", exported, got)
	}
	if _, err := os.Stat(stored); err != nil {
		t.Fatalf("after the clean tick, %s is not on disk: %v", stored, err)
	}

	var logs bytes.Buffer
	d.logger = log.New(&logs, "", 0)
	tree[exported] = &fstest.MapFile{Data: []byte("DEEPSEEK_API_KEY=PLANTED-123\n")}
	d.syncTownStateBackup()
	head2 := townStateHead(t, f, gitRepo)

	if head2 == head1 {
		t.Fatalf("the refusing tick committed nothing; HEAD stayed %s", head1)
	}
	parents := f.Parents(head2)
	if len(parents) != 1 || parents[0] != head1 {
		t.Fatalf("tick 2's parents = %v, want [%s]", parents, head1)
	}
	if _, ok := f.Tree(head1)[inRepo]; !ok {
		t.Errorf("the parent commit does not hold %s; the deletion is not tick 2's", exported)
	}
	if _, ok := f.Tree(head2)[inRepo]; ok {
		t.Errorf("HEAD still holds a copy of %s after it was refused", exported)
	}
	if _, err := os.Stat(stored); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s is still on disk after the refusing tick (stat err = %v)", stored, err)
	}
	staged, err := d.backupGitAt(gitRepo, 0).StagedChanges()
	if err != nil {
		t.Fatalf("StagedChanges: %v", err)
	}
	if len(staged) != 0 {
		t.Errorf("the index still differs from HEAD after the commit: %v", staged)
	}

	subjects := townStateSubjects(t, d, gitRepo)
	if len(subjects) != 2 || !strings.Contains(subjects[0], exported) {
		t.Errorf("subjects = %q; want the newest to name %s", subjects, exported)
	}
	const note = "refused 1 secret-bearing file(s): " + exported
	if strings.Count(logs.String(), note) != 1 {
		t.Errorf("log = %q, want the note to name %s once", logs.String(), exported)
	}
	if strings.Contains(logs.String(), "PLANTED-123") {
		t.Errorf("the log holds the planted credential")
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

// TestTownStateBackupRedactRefusesFreeTextCredential: a JSON export whose
// free-text string holds a token-shaped word is refused, not stored, and the log
// and the note name it without any part of the value.
func TestTownStateBackupRedactRefusesFreeTextCredential(t *testing.T) {
	t.Parallel()
	const token = "sk-abcdefghijklmnopqrst"
	const refused = "settings/themes/notes.json"
	d, f, town := townStateDaemon(t)
	var logs bytes.Buffer
	d.logger = log.New(&logs, "", 0)
	rec := notifyfake.New()
	d.notifier = rec
	tree := townStateRedactTree()
	delete(tree, "settings/themes/notes.txt")
	tree[refused] = &fstest.MapFile{Data: []byte(`{"note":"use ` + token + ` for the build"}`)}
	d.townStateFS = func(string) fs.FS { return tree }
	gitRepo := filepath.Join(town, ".dolt-archive", "git")
	seedTownStateOrigin(t, f, town, gitRepo)

	snap, err := collectTownState(tree, []string{"gastown"})
	if err != nil {
		t.Fatalf("collectTownState: %v", err)
	}
	if _, ok := snap.Files[refused]; ok {
		t.Errorf("%s was stored; a free-text credential must be refused", refused)
	}
	if !containsString(snap.Refused, refused) {
		t.Errorf("refused = %v, want %s among them", snap.Refused, refused)
	}
	note := townStateCredentialNote(snap)
	if !strings.Contains(note, "refused 1 secret-bearing file(s): "+refused) {
		t.Errorf("note = %q, want it to count and name the refused file", note)
	}

	d.syncTownStateBackup()

	if !strings.Contains(logs.String(), "refused 1 secret-bearing file(s): "+refused) {
		t.Errorf("log = %q, want it to count and name the refused file", logs.String())
	}
	for name, text := range map[string]string{"log": logs.String(), "note": note} {
		if strings.Contains(text, "abcdefghijklmnopqrst") {
			t.Errorf("%s holds part of the token", name)
		}
	}
}
