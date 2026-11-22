package daemon

import (
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/townbackup"
	"github.com/steveyegge/gastown/internal/townstate"
)

const (
	// alertKeyTownStateConfigOmitted is the alert a night raises when its export
	// left the town config out for holding a credential-shaped value (gt-zz76o8).
	alertKeyTownStateConfigOmitted = "town_state_nightly:config-omitted"
	// townStateConfigName is the town-root-relative name the alert watches, as
	// the export reports it in ExportSummary.Omitted.
	townStateConfigName = "settings/config.json"
	// omittedNamesShown bounds the paths one omission log line carries, so a
	// night that omits a directory of files still logs one short line.
	omittedNamesShown = 20
	// omittedNameMax bounds one name in that line, in bytes.
	omittedNameMax = 80
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
	if err := preparePartialNightDir(partial, os.Chmod); err != nil {
		return err
	}
	archive := filepath.Join(partial, townbackup.ArchiveName)
	d.logger.Printf("scheduled_maintenance: town-state backup to %s", partial)

	sum, digest, err := d.exportTownStateArchive(archive, os.Chmod)
	if err != nil {
		return failedTownStateNight(partial, err)
	}
	// Counts only: the log never carries the town's file names.
	d.logger.Printf("scheduled_maintenance: town-state backup export: %s", townStateBackupCounts(sum))
	// An omission is reported, never fatal: the night still commits what the
	// export wrote, and a lost town config is the one omission that is alerted.
	// The names are logged because a backup that silently lacks a file is what
	// this line exists to prevent (gt-zz76o8).
	if len(sum.Omitted) > 0 {
		d.logger.Printf("town_state_backup: night omitted %d file(s): %s",
			len(sum.Omitted), townStateOmittedNames(sum.Omitted))
	}
	// A lost town config is the one omission that must be loud, so a delivery that
	// fails is not read as raised: the night still commits and verifies, then
	// reports the failure, which the scheduled step alerts on itself. A clean night
	// clears the alert on every run (an idempotent call, once a day): no record of
	// "raised" survives a restart, and a restart-proof one is out of scope.
	var alertErr error
	if slices.Contains(sum.Omitted, townStateConfigName) {
		if err := d.escalateAlertErr(alertKeyTownStateConfigOmitted, "town_state_nightly",
			"the town config was left out of tonight's backup because it holds a credential-shaped value; the town config is not backed up"); err != nil {
			d.logger.Printf("town_state_backup: the config-omitted alert did not reach the town: %v", err)
			alertErr = fmt.Errorf("town-state backup: the town config was left out of tonight's backup and the alert did not reach the town: %w", err)
		}
	} else {
		d.clearAlerts("the night carries the town config", alertKeyTownStateConfigOmitted)
	}

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
	return alertErr
}

// preparePartialNightDir makes partial and sets its mode to 0700, which the
// create mode cannot do under the daemon's umask, nor over a partial a dead run
// left wider; the commit's rename carries the mode onto the committed night.
// chmod is the mode setter (os.Chmod in production, a refusing one in a test).
func preparePartialNightDir(partial string, chmod func(string, fs.FileMode) error) error {
	if err := os.MkdirAll(partial, 0o700); err != nil {
		return fmt.Errorf("town-state backup: creating %s: %w", partial, err)
	}
	if err := chmod(partial, 0o700); err != nil {
		return failedTownStateNight(partial, fmt.Errorf("setting the mode of %s: %w", partial, err))
	}
	return nil
}

// exportTownStateArchive writes the town root's backup set into a gzip tar at
// archive, and returns the export's counts and the archive's hex sha256. chmod
// is the mode setter (os.Chmod in production, a refusing one in a test). A
// caller removes archive's directory when it reports an error.
func (d *Daemon) exportTownStateArchive(archive string, chmod func(string, fs.FileMode) error) (townstate.ExportSummary, string, error) {
	// The archive holds the town's state, so it is 0600 under any umask. A chmod
	// narrows a stale archive a dead run left wider, which the create mode below
	// never does, and runs before the first byte reaches the file. A missing
	// archive is not a refusal: the open below creates it at 0600.
	if cerr := chmod(archive, 0o600); cerr != nil && !errors.Is(cerr, fs.ErrNotExist) {
		return townstate.ExportSummary{}, "", fmt.Errorf("setting the mode of %s: %w", archive, cerr)
	}
	f, err := os.OpenFile(archive, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
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

// townStateOmittedNames renders the names of a night's omitted files for the
// log: the first omittedNamesShown paths, then the count left out.
func townStateOmittedNames(omitted []string) string {
	shown := omitted[:min(len(omitted), omittedNamesShown)]
	names := make([]string, len(shown))
	for i, name := range shown {
		names[i] = townStateOmittedName(name)
	}
	if len(omitted) <= omittedNamesShown {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(names, ", "), len(omitted)-omittedNamesShown)
}

// townStateOmittedName is one omitted file name as the log line carries it: a name
// is a file's own string and not a trusted one, so it is quoted (a newline, a
// carriage return, an escape sequence or a NUL shows escaped and one name is one
// token, never a second log line), cut to omittedNameMax bytes, and withheld
// altogether when it holds a credential-shaped value, judged on the whole name
// before the cut (gt-ea3yjo).
func townStateOmittedName(name string) string {
	if townstate.NameHoldsCredential(name) {
		return `"<name withheld: credential-shaped>"`
	}
	if len(name) > omittedNameMax {
		n := omittedNameMax
		for n > 0 && !utf8.RuneStart(name[n]) {
			n--
		}
		name = name[:n] + "..."
	}
	return strconv.Quote(name)
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
