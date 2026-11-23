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

// extractTownRoot extracts the night's archive into root with podUnpack, this
// package's pod unpack, so a name that would leave the root is refused here, as
// the pod's import refuses one.
func extractTownRoot(t *testing.T, archive, root string) string {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatalf("opening %s: %v", archive, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("%s is not a gzip stream: %v", archive, err)
	}
	if err := podUnpack(root, gz); err != nil {
		t.Fatalf("extracting %s: %v", archive, err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing %s: %v", archive, err)
	}
	return root
}

// restoreNight is the runbook's restore in order: the verify step judges the
// archive, and only one it accepts is extracted into root.
func restoreNight(t *testing.T, archive, root string) error {
	t.Helper()
	if err := runTownStateBackupVerifyIn(townStateBackupVerifyEnv{archive: archive, out: io.Discard}); err != nil {
		return err
	}
	extractTownRoot(t, archive, root)
	return nil
}

// TestTownStateBackupRestoreIsAcceptedBySeed proves a backup night restores: the
// archive the nightly step writes, extracted into a fresh town root, is accepted
// unchanged by gt town state seed into an empty claim.
func TestTownStateBackupRestoreIsAcceptedBySeed(t *testing.T) {
	t.Parallel()
	t.Run("the archive Verify reads holds the settings rows and no secret-shaped file", func(t *testing.T) {
		t.Parallel()
		town := backupRestoreTown(t)
		archive := backupNightArchive(t, town)
		if _, err := townbackup.Verify(archive); err != nil {
			t.Fatalf("the night's archive does not verify: %v", err)
		}
		extracted := townTreeSnapshot(t, extractTownRoot(t, archive, t.TempDir()))
		for _, rel := range []string{"settings/config.json", "gastown/settings/config.json"} {
			if got, want := extracted[rel], hostFileText(t, town, rel); got != want {
				t.Errorf("the extracted %s = %q, want the town's %q", rel, got, want)
			}
		}
		if _, ok := extracted["mayor/town.json"]; !ok {
			t.Errorf("the extracted archive has no mayor/town.json: %v", extracted)
		}
		for name := range extracted {
			if townstate.SecretShape(name) {
				t.Errorf("the extracted archive holds the secret-shaped %s: %v", name, extracted)
			}
		}
	})
	t.Run("the extracted root is seeded into an empty claim and the verify matches", func(t *testing.T) {
		t.Parallel()
		root := filepath.Join(t.TempDir(), "restored")
		if err := restoreNight(t, backupNightArchive(t, backupRestoreTown(t)), root); err != nil {
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
		err := restoreNight(t, missingIdentityArchive(t), root)
		if err == nil || !strings.Contains(err.Error(), "mayor/town.json") {
			t.Fatalf("restoreNight = %v, want the missing identity file refused", err)
		}
		if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
			t.Errorf("a refused archive left a root at %s (stat: %v), so a seed could run over it", root, statErr)
		}
	})
}
