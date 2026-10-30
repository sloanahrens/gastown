package forgejoprobe

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// recordMode is the mode of the record and of the temporary file it is written
// through. The record names a verdict, never a credential, so 0600 is the rule.
const recordMode fs.FileMode = 0o600

// recordSHA is a commit as git names it: an abbreviated or full hex object name.
var recordSHA = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

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
// names the step. A rig, repo, commit or context the record cannot hold writes
// nothing.
func WriteRecord(fsys FS, clock Clock, configDir, rig, repo, sha, ctx string) (string, error) {
	if err := requireRig(rig); err != nil {
		return "", err
	}
	if err := requireRecordValues(repo, sha, ctx); err != nil {
		return "", err
	}
	if err := fsys.MkdirAll(configDir, 0o700); err != nil {
		return "", fmt.Errorf("could not create %s for the probe record: %w", configDir, err)
	}
	tmp, err := fsys.CreateTemp(configDir, ".probe-"+rig+".*")
	if err != nil {
		return "", fmt.Errorf("could not create a probe record under %s: %w", configDir, err)
	}
	if err := writeLines(tmp, recordContent(clock.Now(), rig, repo, sha, ctx)); err != nil {
		return "", fmt.Errorf("could not write the probe record under %s: %w", configDir, errors.Join(err, removeTemp(fsys, tmp.Name())))
	}
	path := recordPath(configDir, rig)
	if err := fsys.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("could not store the probe record at %s: %w", path, errors.Join(err, removeTemp(fsys, tmp.Name())))
	}
	return path, nil
}

// requireRecordValues refuses a repo, commit or context that would break the
// key=value lines of the record. A value holding a line break would write a key
// of its own, so the record takes only the shapes a line can carry whole.
func requireRecordValues(repo, sha, ctx string) error {
	if !validRepo(repo) {
		return usagef("the probe record's repo %q must be OWNER/NAME", repo)
	}
	if !recordSHA.MatchString(sha) {
		return usagef("the probe record's commit %q must be a hex object name", sha)
	}
	if ctx == "" || strings.Contains(ctx, "=") || strings.IndexFunc(ctx, unicode.IsControl) >= 0 {
		return usagef("the probe record's context %q must be one line of text with no '='", ctx)
	}
	return nil
}

// RemoveRecord removes the rig's green record, so a run that did not prove green
// leaves none for the cutover to read. A record that is already absent is success.
// A rig the command line would refuse removes nothing.
func RemoveRecord(fsys FS, configDir, rig string) error {
	if err := requireRig(rig); err != nil {
		return err
	}
	path := recordPath(configDir, rig)
	if err := fsys.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("could not remove the probe record at %s: %w", path, err)
	}
	return nil
}

// removeTemp removes a temporary record file. A failed removal names the file
// it left behind.
func removeTemp(fsys FS, name string) error {
	if err := fsys.Remove(name); err != nil {
		return fmt.Errorf("could not remove the temporary probe record %s: %w", name, err)
	}
	return nil
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
