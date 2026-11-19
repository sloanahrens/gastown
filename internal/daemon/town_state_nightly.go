package daemon

import (
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/townbackup"
	"github.com/steveyegge/gastown/internal/townstate"
)

// The nightly town-state backup (gt-xg1avw): one date's backup set, exported
// and verified before it is committed. The layout, the partial-directory
// rename and rotation belong to internal/townbackup; the caller decides when a
// night runs (the scheduled-maintenance step, gt-5dgbcy). A night is committed
// only after its archive verifies, so a directory named for a date is always a
// night a restore can read.

// maintenanceTownStateBackup takes one verified town-state backup night under
// root at now. The night's date is now's date; a date that already has a night
// is left untouched and reports success.
func (d *Daemon) maintenanceTownStateBackup(root string, now time.Time) error {
	if townbackup.Taken(root, now) {
		return nil
	}
	partial := townbackup.PartialDir(root, now)
	// A partial left by a daemon that died is reused: the archive is created
	// empty and the manifest is written last, so neither a stale archive nor a
	// stale manifest can make the reused directory look complete.
	if err := os.MkdirAll(partial, 0o755); err != nil {
		return fmt.Errorf("town-state backup: creating %s: %w", partial, err)
	}
	archive := filepath.Join(partial, townbackup.ArchiveName)
	d.logger.Printf("scheduled_maintenance: town-state backup to %s", partial)

	sum, digest, err := d.exportTownStateArchive(archive)
	if err != nil {
		return failedTownStateNight(partial, err)
	}
	// Counts only: the log never carries the town's file names.
	d.logger.Printf("scheduled_maintenance: town-state backup export: %s", townStateBackupCounts(sum))

	verified, err := townbackup.Verify(archive)
	if err != nil {
		return failedTownStateNight(partial, err)
	}

	// A commit that fails leaves the partial night behind; the next run and
	// rotation both remove a partial, so the failure is the error alone.
	night, err := townbackup.Commit(root, townbackup.Manifest{
		Taken: now, Files: verified.Files, Bytes: verified.Bytes, SHA256: digest,
	})
	if err != nil {
		return err
	}
	d.logger.Printf("scheduled_maintenance: town-state backup complete: %s (%d files, %d bytes)",
		night, verified.Files, verified.Bytes)

	removed, err := townbackup.Rotate(root, townbackup.Keep)
	if len(removed) > 0 {
		d.logger.Printf("scheduled_maintenance: town-state backup rotation removed %s (keeping %d)",
			strings.Join(removed, ", "), townbackup.Keep)
	}
	if err != nil {
		// The night is committed and verified; a rotation failure only costs
		// space, as the Dolt nightly backup treats it.
		d.logger.Printf("scheduled_maintenance: WARNING: town-state backup rotation: %v", err)
	}
	return nil
}

// exportTownStateArchive writes the town root's backup set into a gzip tar at
// archive, and returns the export's counts and the archive's hex sha256. A
// caller removes archive's directory when it reports an error.
func (d *Daemon) exportTownStateArchive(archive string) (townstate.ExportSummary, string, error) {
	f, err := os.Create(archive)
	if err != nil {
		return townstate.ExportSummary{}, "", err
	}
	digest := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(f, digest))
	m := &townstate.Manifest{Entries: townstate.Default().Backup()}
	sum, err := townstate.Export(gz, d.config.TownRoot, m, townstate.ExportOptions{Backup: true})
	// Close both writers whatever the export did, so nothing below runs behind
	// an open descriptor; a close error is the night's only if the export
	// itself did not fail.
	if cerr := gz.Close(); cerr != nil && err == nil {
		err = fmt.Errorf("closing the gzip stream of %s: %w", archive, cerr)
	}
	if serr := f.Sync(); serr != nil && err == nil {
		err = fmt.Errorf("flushing %s: %w", archive, serr)
	}
	if cerr := f.Close(); cerr != nil && err == nil {
		err = fmt.Errorf("closing %s: %w", archive, cerr)
	}
	if err != nil {
		return sum, "", err
	}
	return sum, fmt.Sprintf("%x", digest.Sum(nil)), nil
}

// failedTownStateNight removes an uncommitted partial night and returns cause,
// so a failed export or verify never leaves a directory a later night would
// read as its own work in progress.
func failedTownStateNight(partial string, cause error) error {
	if err := os.RemoveAll(partial); err != nil {
		cause = fmt.Errorf("%w (and the partial directory %s could not be removed: %v)", cause, partial, err)
	}
	return fmt.Errorf("town-state backup: %w", cause)
}

// townStateBackupCounts renders an export's counts for the daemon log.
// ExportSummary.String also names the manifest entries and refused links, so
// the log uses this instead: it never carries the town's names.
func townStateBackupCounts(s townstate.ExportSummary) string {
	skipped := 0
	for _, n := range s.Skipped {
		skipped += n
	}
	return fmt.Sprintf("%d files, %d directories, %d symlinks, %d bytes, %d entries skipped",
		s.Files, s.Dirs, s.Symlinks, s.Bytes, skipped)
}
