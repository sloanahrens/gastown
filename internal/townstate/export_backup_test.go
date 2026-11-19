package townstate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// backupTestManifest is the backup tests' own manifest: the rows they walk, a
// rig row for the rig expansion, and two daemon rows the backup does not carry.
// The tests never assert against the production manifest's contents, which
// internal/manifestdata/backup_test.go pins.
func backupTestManifest(t *testing.T) *Manifest {
	t.Helper()
	m := &Manifest{Entries: []Entry{
		{Path: "settings", Owner: OwnerShared, Backup: true, Why: "test: the shared settings row a restore needs"},
		{Path: "<rig>/settings", Owner: OwnerShared, Backup: true, Why: "test: a rig's shared settings row"},
		{Path: "logs", Owner: OwnerDaemon, Why: "test: a daemon row the backup leaves out"},
		{Path: ".claude-town", Owner: OwnerDaemon, Why: "test: a daemon row the backup leaves out"},
	}}
	if err := m.Validate(); err != nil {
		t.Fatalf("the test manifest: %v", err)
	}
	return m
}

// writeRegistry gives town a registry that names the rig gastown, so a rig row
// expands instead of refusing with ErrNoRigs.
func writeRegistry(t *testing.T, town string) {
	t.Helper()
	writeStateFile(t, town, "mayor/town.json", `{"registry":{"version":1,"rigs":{"gastown":{}}}}`, 0o644)
}

// TestExportBackup drives the backup row set: a shared settings row and its rig
// row are carried, the files with a secret shape and the one Exclude refuses
// are left out and counted under the secret class with no name in the summary,
// the untagged daemon rows are not walked, and a symlink leaving the root is
// still skipped.
func TestExportBackup(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeRegistry(t, town)

	// Carried: the settings a restore needs, at the town and at the rig.
	writeStateFile(t, town, "settings/config.json", `{"theme":"dark"}`, 0o644)
	writeStateFile(t, town, "gastown/settings/config.json", `{"rig":"gastown"}`, 0o644)
	// Left out by their names.
	writeStateFile(t, town, "settings/daemon.env", "GT_NOT_A_REAL_TOKEN=x\n", 0o600)
	writeStateFile(t, town, "settings/x.key", "not a real key\n", 0o600)
	writeStateFile(t, town, "settings/forgejo-token.txt", "not a real token\n", 0o600)
	writeStateFile(t, town, "settings/a.pem", "not a real certificate\n", 0o600)
	// Left out because Exclude refuses it.
	writeStateFile(t, town, "settings/notes.md", "notes\n", 0o644)
	// Rows the backup does not walk.
	writeStateFile(t, town, "logs/daemon.log", "daemon log\n", 0o644)
	writeStateFile(t, town, ".claude-town/session1.json", `{"session":"1"}`, 0o644)
	// A link leaving the town root is skipped as it is for every export.
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(town, "settings", "outside-link")); err != nil {
		t.Fatalf("making the symlink: %v", err)
	}

	exclude := func(name string) bool { return name == "settings/notes.md" }
	var stream bytes.Buffer
	sum, err := Export(&stream, town, backupTestManifest(t), ExportOptions{Backup: true, Exclude: exclude})
	if err != nil {
		t.Fatalf("Export with Backup: %v", err)
	}

	names := archiveNames(t, stream.Bytes())
	for _, want := range []string{"settings/config.json", "gastown/settings/config.json"} {
		if !slices.Contains(names, want) {
			t.Errorf("the backup archive lacks %s: %v", want, names)
		}
	}
	gone := []string{
		"settings/daemon.env", "settings/x.key", "settings/forgejo-token.txt", "settings/a.pem",
		"settings/notes.md", "settings/outside-link", "logs/daemon.log", ".claude-town/session1.json",
	}
	for _, name := range gone {
		if slices.Contains(names, name) {
			t.Errorf("the backup archive carries %s: %v", name, names)
		}
	}

	// Four secret-shaped files and the excluded notes.md.
	if got := sum.Skipped[SkipSecret]; got != 5 {
		t.Errorf("summary SkipSecret = %d, want 5", got)
	}
	if got := sum.Skipped[SkipSymlinkOut]; got != 1 {
		t.Errorf("summary SkipSymlinkOut = %d, want 1", got)
	}
	line := sum.String()
	for _, secret := range []string{"daemon.env", "x.key", "forgejo-token.txt", "a.pem", "notes.md", "outside-link"} {
		if strings.Contains(line, secret) {
			t.Errorf("the summary names an excluded file (%q): %q", secret, line)
		}
	}
}

// TestExportWithoutBackupKeepsTheDaemonSet: the switch is the only thing that
// changes the row set, so an export without Backup walks the daemon rows, never
// reaches the settings row, and applies no secret shape of its own.
func TestExportWithoutBackupKeepsTheDaemonSet(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeRegistry(t, town)
	writeStateFile(t, town, "logs/daemon.log", "daemon log\n", 0o644)
	writeStateFile(t, town, "settings/daemon.env", "GT_NOT_A_REAL_TOKEN=x\n", 0o600)
	writeStateFile(t, town, "settings/config.json", `{"theme":"dark"}`, 0o644)

	var stream bytes.Buffer
	sum, err := Export(&stream, town, backupTestManifest(t), ExportOptions{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	names := archiveNames(t, stream.Bytes())
	if !slices.Contains(names, "logs/daemon.log") {
		t.Errorf("the plain export lacks the daemon row's file: %v", names)
	}
	for _, name := range []string{"settings/config.json", "settings/daemon.env"} {
		if slices.Contains(names, name) {
			t.Errorf("the plain export walked the shared settings row: %v", names)
		}
	}
	if got := sum.Skipped[SkipSecret]; got != 0 {
		t.Errorf("summary SkipSecret = %d, want 0: the plain export applies no shape", got)
	}
}

// TestExportBackupLeavesOutASecretShapedDirectory: a directory whose own name
// has the shape is left out whole, so a file with an ordinary name below it
// never reaches the archive, and the directory is counted once.
func TestExportBackupLeavesOutASecretShapedDirectory(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeRegistry(t, town)
	writeStateFile(t, town, "settings/config.json", `{"theme":"dark"}`, 0o644)
	writeStateFile(t, town, "settings/secrets/session.json", `{"password":"not a real one"}`, 0o600)

	var stream bytes.Buffer
	sum, err := Export(&stream, town, backupTestManifest(t), ExportOptions{Backup: true})
	if err != nil {
		t.Fatalf("Export with Backup: %v", err)
	}
	names := archiveNames(t, stream.Bytes())
	if slices.Contains(names, "settings/secrets/") || slices.Contains(names, "settings/secrets/session.json") {
		t.Errorf("the backup archive carries the secret-shaped directory: %v", names)
	}
	if got := sum.Skipped[SkipSecret]; got != 1 {
		t.Errorf("summary SkipSecret = %d, want 1 for the directory", got)
	}
	if line := sum.String(); strings.Contains(line, "session.json") {
		t.Errorf("the summary names the excluded file: %q", line)
	}
}

// TestExportBackupLeavesOutARowBelowASecretShapedSegment: an ancestor of a row
// that the walk never visits as an entry still decides the row's fate, so a
// file named ordinarily below a secret-shaped segment of its row path is left
// out too.
func TestExportBackupLeavesOutARowBelowASecretShapedSegment(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeStateFile(t, town, "store/token-store/session.json", `{"password":"not a real one"}`, 0o600)
	m := &Manifest{Entries: []Entry{
		{Path: "store/token-store/session.json", Owner: OwnerDaemon, Backup: true, Why: "test: a row below a secret-shaped segment"},
	}}
	if err := m.Validate(); err != nil {
		t.Fatalf("the test manifest: %v", err)
	}

	var stream bytes.Buffer
	sum, err := Export(&stream, town, m, ExportOptions{Backup: true, AllowNoRigs: true})
	if err != nil {
		t.Fatalf("Export with Backup: %v", err)
	}
	if names := archiveNames(t, stream.Bytes()); slices.Contains(names, "store/token-store/session.json") {
		t.Errorf("the backup archive carries the row below token-store: %v", names)
	}
	if got := sum.Skipped[SkipSecret]; got != 1 {
		t.Errorf("summary SkipSecret = %d, want 1", got)
	}
}

// TestExportBackupKeepsTheRowsOfARigNamedForAToken: the first segment of a row
// is its root, a rig. A rig whose own name holds "token" or "secret" keeps its
// ordinary rows; only a secret-shaped file name, or a secret-shaped directory
// below the root, is left out (gt-4kf6kk, om round 3).
func TestExportBackupKeepsTheRowsOfARigNamedForAToken(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeStateFile(t, town, "token-svc/settings/config.json", `{"ok":true}`, 0o600)
	writeStateFile(t, town, "token-svc/settings/secrets/session.json", `{"password":"not a real one"}`, 0o600)
	m := &Manifest{Entries: []Entry{
		{Path: "token-svc/settings", Owner: OwnerDaemon, Backup: true, Why: "test: a rig named for a token"},
	}}
	if err := m.Validate(); err != nil {
		t.Fatalf("the test manifest: %v", err)
	}

	var stream bytes.Buffer
	sum, err := Export(&stream, town, m, ExportOptions{Backup: true, AllowNoRigs: true})
	if err != nil {
		t.Fatalf("Export with Backup: %v", err)
	}
	names := archiveNames(t, stream.Bytes())
	if !slices.Contains(names, "token-svc/settings/config.json") {
		t.Errorf("the rig named for a token lost its ordinary row: %v", names)
	}
	if slices.Contains(names, "token-svc/settings/secrets/session.json") {
		t.Errorf("the archive carries a file below a secret-shaped directory: %v", names)
	}
	if got := sum.Skipped[SkipSecret]; got != 1 {
		t.Errorf("summary SkipSecret = %d, want 1 (the secrets directory)", got)
	}
}

// TestExportBackupRefusesATownWithNoRigs drives the backup path's refusal: a
// backup row carrying the rig placeholder with no registered rig is refused
// with ErrNoRigs, and AllowNoRigs counts the walked set's rig rows in the
// summary's warning.
func TestExportBackupRefusesATownWithNoRigs(t *testing.T) {
	t.Parallel()
	newTown := func(t *testing.T) string {
		t.Helper()
		town := t.TempDir()
		writeStateFile(t, town, "settings/config.json", `{"theme":"dark"}`, 0o644)
		return town
	}
	m := backupTestManifest(t)

	t.Run("refused and writes nothing", func(t *testing.T) {
		t.Parallel()
		var stream bytes.Buffer
		sum, err := Export(&stream, newTown(t), m, ExportOptions{Backup: true})
		if !errors.Is(err, ErrNoRigs) {
			t.Fatalf("Export error = %v, want ErrNoRigs", err)
		}
		if names := archiveNames(t, stream.Bytes()); len(names) != 0 {
			t.Errorf("a refused export wrote %v, want an empty archive", names)
		}
		if sum.RigRowsDropped != 1 {
			t.Errorf("RigRowsDropped = %d, want the backup set's one rig row", sum.RigRowsDropped)
		}
	})

	t.Run("AllowNoRigs counts the walked set's rig rows", func(t *testing.T) {
		t.Parallel()
		var stream bytes.Buffer
		sum, err := Export(&stream, newTown(t), m, ExportOptions{Backup: true, AllowNoRigs: true})
		if err != nil {
			t.Fatalf("Export with AllowNoRigs: %v", err)
		}
		if sum.RigRowsDropped != 1 || !strings.Contains(sum.String(), "WARNING: the town registers no rigs") {
			t.Errorf("summary = %q (RigRowsDropped %d), want the missing rig row named", sum.String(), sum.RigRowsDropped)
		}
	})
}

// TestExportBackupRefusesABirdRow: a backup names no bird, so a backup-tagged
// row carrying the bird placeholder is refused by name rather than expanded to
// nothing.
func TestExportBackupRefusesABirdRow(t *testing.T) {
	t.Parallel()
	m := &Manifest{Entries: []Entry{
		{Path: "settings", Owner: OwnerShared, Backup: true, Why: "test: a settings row"},
		{Path: ".runtime/<bird>", Owner: OwnerDaemon, Backup: true, Why: "test: a bird row a backup must refuse"},
	}}
	if err := m.Validate(); err != nil {
		t.Fatalf("the test manifest: %v", err)
	}
	_, err := Export(&bytes.Buffer{}, t.TempDir(), m, ExportOptions{Backup: true})
	if err == nil || !strings.Contains(err.Error(), ".runtime/<bird>") {
		t.Fatalf("Export error = %v, want a refusal naming the bird row", err)
	}
}
