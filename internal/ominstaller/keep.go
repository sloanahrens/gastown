package ominstaller

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// omPrevName is the file the installed om is kept as, beside om, for a rollback.
const omPrevName = "om.prev"

// keepTempName is the temp file the copy is written to before it is renamed to
// om.prev. The install lock runs one install at a time, so one name serves, and a
// temp file a crash leaves behind is overwritten by the next copy.
const keepTempName = ".om.prev.tmp"

// errKeepPrevious is the failure to keep the installed om as om.prev. The install
// stops before the swap, so om is untouched.
var errKeepPrevious = errors.New("cannot keep the previous om")

// keepPrevious copies the installed om at omPath to om.prev beside it, so a failed
// smoke check can put it back. The copy is written to a temp file and renamed into
// place: om.prev is never partly written, and a failed copy leaves any earlier
// om.prev as it was. With no installed om, a first install, it keeps nothing and
// logs that it kept nothing.
func keepPrevious(cfg Config, omPath string) error {
	files := cfg.files()
	dir := filepath.Dir(omPath)
	prev := filepath.Join(dir, omPrevName)
	tmp := filepath.Join(dir, keepTempName)

	perm, err := files.Stat(omPath)
	if errors.Is(err, fs.ErrNotExist) {
		cfg.logf("[install-om] no om at %s to keep: first install, so there is nothing to roll back to", omPath)
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: cannot read the mode of %s: %w", errKeepPrevious, omPath, err)
	}
	data, err := files.ReadFile(omPath)
	if err != nil {
		return fmt.Errorf("%w: cannot read %s: %w", errKeepPrevious, omPath, err)
	}
	if err := writeThenRename(files, tmp, prev, data, perm.Perm()); err != nil {
		return fmt.Errorf("%w: cannot copy %s to %s: %w", errKeepPrevious, omPath, prev, err)
	}
	cfg.logf("[install-om] kept the previous om as %s", prev)
	return nil
}

// writeThenRename writes data to tmp with perm, then renames tmp to dst. A failure
// at any step removes tmp, so the only thing left beside dst is dst itself.
func writeThenRename(files Files, tmp, dst string, data []byte, perm fs.FileMode) error {
	err := files.WriteFile(tmp, data, perm)
	if err == nil {
		err = files.Chmod(tmp, perm)
	}
	if err == nil {
		err = files.Rename(tmp, dst)
	}
	if err != nil {
		// A leftover temp is never dst, and the next copy overwrites it, so a
		// failed remove changes nothing the caller can act on.
		_ = files.Remove(tmp)
	}
	return err
}
