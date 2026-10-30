package forgejoprobe

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"time"
)

// recordMode is the mode of the record and of the temporary file it is written
// through. The record names a verdict, never a credential, so 0600 is the rule.
const recordMode fs.FileMode = 0o600

// FS is the filesystem the green record is written through. The run puts the
// operating system's behind it; a test puts an in-memory fake.
type FS interface {
	MkdirAll(path string, perm fs.FileMode) error
	CreateTemp(dir, pattern string) (TempFile, error)
	Rename(oldpath, newpath string) error
	Remove(name string) error
}

// TempFile is a file CreateTemp made. Its mode is set and its content written
// before it is closed and renamed into place.
type TempFile interface {
	io.Writer
	Name() string
	Chmod(mode fs.FileMode) error
	Close() error
}

// recordPath is the record the cutover reads: <config-dir>/probe-<rig>.record.
func recordPath(configDir, rig string) string {
	return filepath.Join(configDir, "probe-"+rig+".record")
}

// WriteRecord stores the green verdict of rig's probe of sha as key=value lines
// under configDir and returns the record's path. The lines go to a temporary
// file that is renamed into place, so a cutover never reads a half-written
// record. A failed step removes the temporary file and returns an error that
// names the step.
func WriteRecord(fsys FS, clock Clock, configDir, rig, repo, sha, ctx string) (string, error) {
	if err := fsys.MkdirAll(configDir, 0o700); err != nil {
		return "", fmt.Errorf("could not create %s for the probe record: %w", configDir, err)
	}
	tmp, err := fsys.CreateTemp(configDir, ".probe-"+rig+".*")
	if err != nil {
		return "", fmt.Errorf("could not create a probe record under %s: %w", configDir, err)
	}
	if err := writeLines(tmp, recordContent(clock.Now(), rig, repo, sha, ctx)); err != nil {
		return "", fmt.Errorf("could not write the probe record under %s: %w", configDir, errors.Join(err, fsys.Remove(tmp.Name())))
	}
	path := recordPath(configDir, rig)
	if err := fsys.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("could not store the probe record at %s: %w", path, errors.Join(err, fsys.Remove(tmp.Name())))
	}
	return path, nil
}

// recordContent is the seven key=value lines of a green record, in the order the
// cutover reads them. The time is UTC, written twice: as text and as epoch seconds.
func recordContent(now time.Time, rig, repo, sha, ctx string) string {
	now = now.UTC()
	return fmt.Sprintf("rig=%s\nrepo=%s\ncommit=%s\ncontext=%s\nverdict=green\ntime=%s\nepoch=%d\n",
		rig, repo, sha, ctx, now.Format("2006-01-02T15:04:05Z"), now.Unix())
}

// writeLines sets f's mode, writes content and closes f. Close runs on every
// path, so a failed write never leaves f open for the caller to remove.
func writeLines(f TempFile, content string) error {
	if err := f.Chmod(recordMode); err != nil {
		return errors.Join(err, f.Close())
	}
	if _, err := io.WriteString(f, content); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}
