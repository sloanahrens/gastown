package daemon

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/townbackup"
)

// nightlyTestNow is the fixed clock every nightly backup test runs at: three
// in the morning on a date no seeded night holds.
func nightlyTestNow() time.Time {
	return time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC)
}

// nightlyTestTown writes a town the backup export walks: the registry that
// names its rig, the identity file Verify requires, and settings at the town
// and at the rig.
func nightlyTestTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	writeTownFile(t, town, "mayor/town.json", `{"registry":{"version":1,"rigs":{"gastown":{}}}}`)
	writeTownFile(t, town, "settings/config.json", `{"theme":"dark"}`)
	writeTownFile(t, town, "gastown/settings/config.json", `{"rig":"gastown"}`)
	return town
}

// nightlyTestDaemon is a daemon whose town root is town, logging to logs.
func nightlyTestDaemon(town string, logs *strings.Builder) *Daemon {
	return &Daemon{config: &Config{TownRoot: town}, logger: log.New(logs, "", 0)}
}

// TestTownStateNightlyWritesVerifiedNight is the first acceptance box: a run
// writes the dated night with the archive and its manifest, the archive
// verifies, and the manifest counts and digest describe the archive on disk.
func TestTownStateNightlyWritesVerifiedNight(t *testing.T) {
	t.Parallel()
	town := nightlyTestTown(t)
	root := t.TempDir()
	now := nightlyTestNow()
	var logs strings.Builder
	d := nightlyTestDaemon(town, &logs)

	if err := d.maintenanceTownStateBackup(root, now); err != nil {
		t.Fatalf("maintenanceTownStateBackup: %v", err)
	}

	night := townbackup.Dir(root, now)
	archive := filepath.Join(night, townbackup.ArchiveName)
	for _, name := range []string{townbackup.ArchiveName, townbackup.ManifestName} {
		if _, err := os.Stat(filepath.Join(night, name)); err != nil {
			t.Errorf("the night has no %s: %v", name, err)
		}
	}
	if _, err := os.Stat(townbackup.PartialDir(root, now)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the partial directory is still there: %v", err)
	}
	verified, err := townbackup.Verify(archive)
	if err != nil {
		t.Fatalf("the night's archive does not verify: %v", err)
	}
	newest, ok, err := townbackup.Newest(root)
	if err != nil || !ok {
		t.Fatalf("Newest = %+v, %v, %v; want the night just written", newest, ok, err)
	}
	if newest.Path != night {
		t.Errorf("Newest path = %s, want %s", newest.Path, night)
	}
	if got := newest.Manifest; got.Taken != now || got.Files != verified.Files || got.Bytes != verified.Bytes {
		t.Errorf("manifest = %+v, want taken %v with the archive's %d files and %d bytes",
			got, now, verified.Files, verified.Bytes)
	}
	if want := nightlyTestFileSum(t, archive); newest.Manifest.SHA256 != want {
		t.Errorf("manifest sha256 = %q, want the archive's %q", newest.Manifest.SHA256, want)
	}

	// The log carries counts, never a name from the town.
	if logged := logs.String(); !strings.Contains(logged, "files") {
		t.Errorf("the log carries no export counts: %q", logged)
	}
	for _, name := range []string{"config.json", "town.json"} {
		if strings.Contains(logs.String(), name) {
			t.Errorf("the log names the town's %s: %q", name, logs.String())
		}
	}
}

// TestTownStateNightlyLeavesOutSecretFiles is the second acceptance box's
// first half: a secret-shaped file under settings/ never reaches the archive.
func TestTownStateNightlyLeavesOutSecretFiles(t *testing.T) {
	t.Parallel()
	town := nightlyTestTown(t)
	writeTownFile(t, town, "settings/daemon.env", "GT_NOT_A_REAL_TOKEN=x\n")
	writeTownFile(t, town, "settings/x.key", "not a real key\n")
	root := t.TempDir()
	now := nightlyTestNow()
	var logs strings.Builder
	d := nightlyTestDaemon(town, &logs)

	if err := d.maintenanceTownStateBackup(root, now); err != nil {
		t.Fatalf("maintenanceTownStateBackup: %v", err)
	}

	names := nightlyTestArchiveNames(t, filepath.Join(townbackup.Dir(root, now), townbackup.ArchiveName))
	if !slices.Contains(names, "settings/config.json") {
		t.Errorf("the archive lacks settings/config.json: %v", names)
	}
	for _, secret := range []string{"settings/daemon.env", "settings/x.key"} {
		if slices.Contains(names, secret) {
			t.Errorf("the archive carries the secret-shaped %s: %v", secret, names)
		}
	}
}

// TestTownStateNightlyRefusesToCommit is the second acceptance box's second
// half, and every other way a night fails: the run reports the cause, commits
// no night, and removes the partial unless the failure was the commit itself.
func TestTownStateNightlyRefusesToCommit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// town and root build the fixture; nil takes the good town and a temp
		// backup root.
		town, root func(t *testing.T) string
		// wants is a phrase the error must carry; empty accepts any error.
		wants         string
		wantNoPartial bool
	}{
		{
			// The registry cannot be read, so the export itself fails.
			name: "export fails",
			town: func(t *testing.T) string {
				t.Helper()
				town := t.TempDir()
				writeTownFile(t, town, "mayor/town.json", "{not json")
				writeTownFile(t, town, "settings/config.json", `{"theme":"dark"}`)
				return town
			},
			wants:         "not a JSON object",
			wantNoPartial: true,
		},
		{
			// The export succeeds but the archive holds no identity file, so
			// Verify refuses it.
			name: "verify fails",
			town: func(t *testing.T) string {
				t.Helper()
				town := t.TempDir()
				writeTownFile(t, town, "mayor/rigs.json", `{"version":1,"rigs":{"gastown":{}}}`)
				writeTownFile(t, town, "settings/config.json", `{"theme":"dark"}`)
				return town
			},
			wants:         "has no mayor/town.json",
			wantNoPartial: true,
		},
		{
			// A backup root that is a file cannot hold the partial directory.
			name: "backup root is a file",
			root: func(t *testing.T) string {
				t.Helper()
				path := filepath.Join(t.TempDir(), "town-state")
				if err := os.WriteFile(path, []byte("not a directory"), 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			},
			wants: "not a directory",
		},
		{
			// A stale partial holds a directory where the archive goes.
			name: "archive cannot be created",
			root: func(t *testing.T) string {
				t.Helper()
				return nightlyTestPartialEntry(t, townbackup.ArchiveName)
			},
			wants:         "is a directory",
			wantNoPartial: true,
		},
		{
			// A stale partial holds a directory where the manifest goes, so
			// the commit fails; the partial it leaves is removed by the next
			// run or by rotation.
			name: "commit fails",
			root: func(t *testing.T) string {
				t.Helper()
				return nightlyTestPartialEntry(t, townbackup.ManifestName)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := nightlyTestNow()
			town := nightlyTestTown(t)
			if tt.town != nil {
				town = tt.town(t)
			}
			root := t.TempDir()
			if tt.root != nil {
				root = tt.root(t)
			}
			var logs strings.Builder
			d := nightlyTestDaemon(town, &logs)

			err := d.maintenanceTownStateBackup(root, now)
			if err == nil {
				t.Fatal("maintenanceTownStateBackup = nil, want the failure")
			}
			if tt.wants != "" && !strings.Contains(err.Error(), tt.wants) {
				t.Errorf("error = %v, want it to say %q", err, tt.wants)
			}
			if fi, statErr := os.Stat(townbackup.Dir(root, now)); statErr == nil && fi.IsDir() {
				t.Errorf("a night was committed despite the failure")
			}
			if tt.wantNoPartial {
				if _, statErr := os.Stat(townbackup.PartialDir(root, now)); statErr == nil {
					t.Errorf("the partial directory is still there")
				}
			}
		})
	}
}

// nightlyTestPartialEntry makes a backup root whose tonight partial holds a
// directory named entry, and returns the root.
func nightlyTestPartialEntry(t *testing.T, entry string) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(townbackup.PartialDir(root, nightlyTestNow()), entry)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
	return root
}

// TestTownStateNightlySkipsTakenDate is the third acceptance box's first half:
// a second run on the same date writes nothing.
func TestTownStateNightlySkipsTakenDate(t *testing.T) {
	t.Parallel()
	town := nightlyTestTown(t)
	root := t.TempDir()
	now := nightlyTestNow()
	var logs strings.Builder
	d := nightlyTestDaemon(town, &logs)

	if err := d.maintenanceTownStateBackup(root, now); err != nil {
		t.Fatalf("first maintenanceTownStateBackup: %v", err)
	}
	archive := filepath.Join(townbackup.Dir(root, now), townbackup.ArchiveName)
	// A file the second export would carry, and a stamp on the archive only a
	// rewrite would change.
	writeTownFile(t, town, "settings/added.json", `{"added":true}`)
	past := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(archive, past, past); err != nil {
		t.Fatalf("stamping the archive: %v", err)
	}

	if err := d.maintenanceTownStateBackup(root, now); err != nil {
		t.Fatalf("second maintenanceTownStateBackup: %v", err)
	}

	info, err := os.Stat(archive)
	if err != nil {
		t.Fatalf("stat the archive: %v", err)
	}
	if !info.ModTime().Equal(past) {
		t.Errorf("the archive was rewritten: modtime %v, want %v", info.ModTime(), past)
	}
	if names := nightlyTestArchiveNames(t, archive); slices.Contains(names, "settings/added.json") {
		t.Errorf("the archive grew on the second run: %v", names)
	}
}

// TestTownStateNightlyRotatesTo14 is the third acceptance box's second half: a
// run on the 15th date leaves 14 nights.
func TestTownStateNightlyRotatesTo14(t *testing.T) {
	t.Parallel()
	town := nightlyTestTown(t)
	root := t.TempDir()
	for day := 1; day <= townbackup.Keep; day++ {
		nightlyTestSeedNight(t, root, time.Date(2026, 11, day, 3, 0, 0, 0, time.UTC))
	}
	now := nightlyTestNow()
	var logs strings.Builder
	d := nightlyTestDaemon(town, &logs)

	if err := d.maintenanceTownStateBackup(root, now); err != nil {
		t.Fatalf("maintenanceTownStateBackup: %v", err)
	}

	nights, err := townbackup.List(root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(nights) != townbackup.Keep {
		t.Fatalf("List = %d nights, want %d", len(nights), townbackup.Keep)
	}
	oldest := townbackup.Dir(root, time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC))
	if _, err := os.Stat(oldest); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("rotation kept the oldest night %s: %v", oldest, err)
	}
	if _, err := os.Stat(townbackup.Dir(root, now)); err != nil {
		t.Errorf("rotation removed tonight's night: %v", err)
	}
}

// nightlyTestSeedNight commits a complete fake night at t, so rotation has
// history to trim without running an export per night.
func nightlyTestSeedNight(t *testing.T, root string, at time.Time) {
	t.Helper()
	partial := townbackup.PartialDir(root, at)
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatalf("seeding the partial night %s: %v", partial, err)
	}
	if err := os.WriteFile(filepath.Join(partial, townbackup.ArchiveName), []byte("seeded"), 0o644); err != nil {
		t.Fatalf("seeding the archive in %s: %v", partial, err)
	}
	if _, err := townbackup.Commit(root, townbackup.Manifest{Taken: at, Files: 1, Bytes: 6, SHA256: "seeded"}); err != nil {
		t.Fatalf("committing the seeded night %s: %v", at.Format(time.DateOnly), err)
	}
}

// nightlyTestArchiveNames lists the entries a gzip tar archive holds.
func nightlyTestArchiveNames(t *testing.T, archive string) []string {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatalf("opening %s: %v", archive, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("reading %s as gzip: %v", archive, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatalf("reading the archive %s: %v", archive, err)
		}
		names = append(names, strings.TrimSuffix(hdr.Name, "/"))
	}
}

// nightlyTestFileSum is the hex sha256 of the file at path.
func nightlyTestFileSum(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
