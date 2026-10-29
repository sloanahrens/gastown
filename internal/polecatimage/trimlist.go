package polecatimage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// A removal list past one set of ConfigMap shards is carried in the claim itself (gt-2rq36). This
// file is the writer: something with the claim mounted writes PlanTrim's list into the claim, and
// the trim Job's container then reads it with trim-paths --list-file (trimpaths.go).

// TrimListFiles is the claim the list writer touches, by names relative to the claim root. A test
// answers the calls, so each failure is injected at the call it belongs to; the claim itself answers
// through an os.Root (rootTrimListFiles), so a symlink planted in the shared claim cannot take a call
// out of it.
type TrimListFiles interface {
	MkdirAll(name string, perm os.FileMode) error
	Remove(name string) error
	WriteFile(name string, data []byte, perm os.FileMode) error
	Rename(oldpath, newpath string) error
}

// rootTrimListFiles is TrimListFiles on the claim: every name resolves inside the root, and a name
// that would leave it through a symlink is refused by the walk the kernel makes.
type rootTrimListFiles struct{ root *os.Root }

func (f rootTrimListFiles) MkdirAll(name string, perm os.FileMode) error {
	return f.root.MkdirAll(name, perm)
}

func (f rootTrimListFiles) Remove(name string) error { return f.root.Remove(name) }

func (f rootTrimListFiles) WriteFile(name string, data []byte, perm os.FileMode) error {
	return f.root.WriteFile(name, data, perm)
}

func (f rootTrimListFiles) Rename(oldpath, newpath string) error {
	return f.root.Rename(oldpath, newpath)
}

const (
	// trimListDirPerm is the mode of the directory under the claim the list sits in.
	trimListDirPerm = 0o755
	// trimListFilePerm is the mode of the written list: the trim container reads it as the user the
	// claim belongs to.
	trimListFilePerm = 0o644
)

// WriteTrimList writes paths, PlanTrim's removal list, into the claim files answers for, where a
// claim-carried trim reads it.
//
// The list is written under a temporary name and renamed into place, so a write that fails part way
// leaves no list a Job could read as the whole one. A path a walk of the claim could not give is
// refused whole rather than written, since the container would refuse the list it read.
func WriteTrimList(paths []string, files TrimListFiles) error {
	if len(paths) == 0 {
		return fmt.Errorf("the trim list names no path: a claim-carried trim with nothing to remove would apply as a success")
	}
	for _, p := range paths {
		if !claimRelative(p) {
			return fmt.Errorf("the trim list names %q: want a path a walk of the claim root gives, such as gocache/ab/cdef", p)
		}
	}
	// Encoding a slice of strings cannot fail, so the error is dropped.
	data, _ := json.Marshal(paths)

	if err := files.MkdirAll(cachevol.TrimClaimListDir, trimListDirPerm); err != nil {
		return fmt.Errorf("creating the trim list directory %s: %w", cachevol.TrimClaimListDir, err)
	}
	dest := cachevol.TrimClaimListPath
	tmp := dest + ".tmp"
	// A temporary left by an earlier write is removed before this one is made: a symlink at its name
	// would otherwise take the write to whatever it points at, even inside the claim.
	if err := files.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clearing the stale trim list %s: %w", tmp, err)
	}
	if err := files.WriteFile(tmp, data, trimListFilePerm); err != nil {
		return fmt.Errorf("writing the trim list under %s: %w", cachevol.TrimClaimListDir, err)
	}
	if err := files.Rename(tmp, dest); err != nil {
		return fmt.Errorf("moving the trim list into place under %s: %w", cachevol.TrimClaimListDir, err)
	}
	return nil
}

// writeTrimListInto writes paths into the claim at root, through an os.Root on it, so every name the
// writer makes is answered inside the claim.
func writeTrimListInto(root string, paths []string) (err error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("opening the claim %s: %w", root, err)
	}
	defer func() {
		if cerr := r.Close(); err == nil {
			err = cerr
		}
	}()
	return WriteTrimList(paths, rootTrimListFiles{root: r})
}
