package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/townbackup"
	"github.com/steveyegge/gastown/internal/townstate"
)

// backupRestoreTown is the town a restore test backs up: seedTown's daemon state
// and settings, plus the registry and the rig settings file a backup walks.
func backupRestoreTown(t *testing.T) string {
	t.Helper()
	town := seedTown(t)
	writeTownStateFileContent(t, town, "mayor/town.json", `{"registry":{"version":1,"rigs":{"gastown":{}}}}`)
	writeTownStateFileContent(t, town, "gastown/settings/config.json", `{"rig":"gastown"}`+"\n")
	return town
}

// gzipArchive writes one gzip stream at the night's archive name and returns its
// path, so a test builds the nightly archive and a broken fixture the same way.
func gzipArchive(t *testing.T, write func(io.Writer) error) string {
	t.Helper()
	archive := filepath.Join(t.TempDir(), townbackup.ArchiveName)
	f, err := os.Create(archive)
	if err != nil {
		t.Fatalf("creating %s: %v", archive, err)
	}
	gz := gzip.NewWriter(f)
	err = write(gz)
	if cerr := gz.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatalf("writing the archive %s: %v", archive, err)
	}
	return archive
}

// backupNightArchive writes a night's archive from the production export command.
func backupNightArchive(t *testing.T, town string) string {
	t.Helper()
	var set bytes.Buffer
	if err := runTownStateExportIn(townStateExportEnv{town: town, backup: true, out: &set, summary: io.Discard}); err != nil {
		t.Fatalf("exporting the backup set with the export command: %v", err)
	}
	return gzipArchive(t, func(w io.Writer) error {
		_, err := w.Write(set.Bytes())
		return err
	})
}

// missingIdentityArchive writes a fixture night holding a settings row and no
// mayor/town.json, so a test can show Verify refuses the night before a seed.
func missingIdentityArchive(t *testing.T) string {
	t.Helper()
	return gzipArchive(t, func(w io.Writer) error {
		tw := tar.NewWriter(w)
		body := []byte(profileTestSettings)
		err := tw.WriteHeader(&tar.Header{Name: "settings/config.json", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		if err == nil {
			_, err = tw.Write(body)
		}
		if cerr := tw.Close(); cerr != nil && err == nil {
			err = cerr
		}
		return err
	})
}

// TestTownStateBackupRestoreIsAcceptedBySeed proves a backup night restores: the
// production restore command extracts the archive the nightly step writes into a
// fresh town root, and gt town state seed accepts it into an empty claim.
func TestTownStateBackupRestoreIsAcceptedBySeed(t *testing.T) {
	t.Parallel()
	t.Run("the restored root holds the settings rows and no secret-shaped file", func(t *testing.T) {
		t.Parallel()
		town := backupRestoreTown(t)
		archive := backupNightArchive(t, town)
		if _, err := townbackup.Verify(archive); err != nil {
			t.Fatalf("the night's archive does not verify: %v", err)
		}
		root := filepath.Join(t.TempDir(), "restored")
		if err := restoreBackupNight(archive, root, io.Discard); err != nil {
			t.Fatalf("restoring the night's archive: %v", err)
		}
		restored := townTreeSnapshot(t, root)
		if got, want := restored["gastown/settings/config.json"], hostFileText(t, town, "gastown/settings/config.json"); got != want {
			t.Errorf("the restored rig settings = %q, want the town's %q", got, want)
		}
		// Import writes the town's settings file without the Mac's
		// daemon.supervisor: a restore root has no Mac, and the seed writes the
		// claim without it too.
		if settings := restored["settings/config.json"]; !strings.Contains(settings, "landing_worker") || strings.Contains(settings, "supervisor") {
			t.Errorf("the restored settings = %q, want the archived patrols and no daemon.supervisor", settings)
		}
		if _, ok := restored["mayor/town.json"]; !ok {
			t.Errorf("the restored archive has no mayor/town.json: %v", restored)
		}
		for name := range restored {
			if townstate.SecretShape(name) {
				t.Errorf("the restored archive holds the secret-shaped %s: %v", name, restored)
			}
		}
	})
	t.Run("the restored root is seeded into an empty claim and the verify matches", func(t *testing.T) {
		t.Parallel()
		root := filepath.Join(t.TempDir(), "restored")
		if err := restoreBackupNight(backupNightArchive(t, backupRestoreTown(t)), root, io.Discard); err != nil {
			t.Fatalf("restoring the night's archive: %v", err)
		}
		s := newSeedSession(t, root)
		if err := runTownStateSeedIn(s.env); err != nil {
			t.Fatalf("seeding the restored root: %v\n%s", err, s.out.String())
		}
		if !strings.Contains(s.out.String(), "claim town-state matches the host copy set") {
			t.Errorf("output does not report the claim matches:\n%s", s.out.String())
		}
		if _, err := os.Stat(filepath.Join(s.fv.dir, "settings", "config.json")); err != nil {
			t.Errorf("the claim has no settings/config.json after the seed: %v", err)
		}
	})
	t.Run("the verify step refuses an archive with no mayor/town.json, so no root is extracted", func(t *testing.T) {
		t.Parallel()
		root := filepath.Join(t.TempDir(), "restored")
		err := restoreBackupNight(missingIdentityArchive(t), root, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "mayor/town.json") {
			t.Fatalf("restoreBackupNight = %v, want the missing identity file refused", err)
		}
		if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
			t.Errorf("a refused archive left a root at %s (stat: %v), so a seed could run over it", root, statErr)
		}
	})
}
