package deployerinbox

import (
	"errors"
	"fmt"
	"os"
)

// OSAnswerFS is the AnswerFS over the operating system's files. Each method is
// one os call, and its error names the path it failed on.
type OSAnswerFS struct{}

// var _ keeps OSAnswerFS in step with the interface: a change to AnswerFS
// breaks the build here.
var _ AnswerFS = OSAnswerFS{}

// CreateExclusive creates path with perm and writes data to it. It fails when
// path exists, and it removes nothing it made.
func (OSAnswerFS) CreateExclusive(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Close()); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Chmod sets the mode of path.
func (OSAnswerFS) Chmod(path string, mode os.FileMode) error {
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// Link makes newpath a second name for oldpath. It fails when newpath exists.
func (OSAnswerFS) Link(oldpath, newpath string) error {
	if err := os.Link(oldpath, newpath); err != nil {
		return fmt.Errorf("link %s to %s: %w", oldpath, newpath, err)
	}
	return nil
}

// Remove deletes path.
func (OSAnswerFS) Remove(path string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
