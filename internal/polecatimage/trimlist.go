package polecatimage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// A removal list past one set of ConfigMap shards is carried in the claim itself (gt-2rq36). This
// file is the writer: something with the claim mounted writes PlanTrim's list into the claim, and
// the trim Job's container then reads it with trim-paths --list-file (trimpaths.go).

// TrimListFiles is the filesystem the list writer touches: the directory under the claim and the
// file it puts the list in. A test answers the calls, so each failure is injected at the call it
// belongs to.
type TrimListFiles interface {
	MkdirAll(name string, perm os.FileMode) error
	WriteFile(name string, data []byte, perm os.FileMode) error
	Rename(oldpath, newpath string) error
}

// osTrimListFiles is TrimListFiles on the filesystem of the pod that writes the list.
type osTrimListFiles struct{}

func (osTrimListFiles) MkdirAll(p string, m os.FileMode) error { return os.MkdirAll(p, m) }

func (osTrimListFiles) WriteFile(p string, b []byte, m os.FileMode) error {
	return os.WriteFile(p, b, m)
}

func (osTrimListFiles) Rename(o, n string) error { return os.Rename(o, n) }

const (
	// trimListDirPerm is the mode of the directory under the claim the list sits in.
	trimListDirPerm = 0o755
	// trimListFilePerm is the mode of the written list: the trim container reads it as the user the
	// claim belongs to.
	trimListFilePerm = 0o644
)

// WriteTrimList writes paths, PlanTrim's removal list, into the claim at root, where a
// claim-carried trim reads it.
//
// The list is written under a temporary name and renamed into place, so a write that fails part way
// leaves no list a Job could read as the whole one. A path a walk of the claim could not give is
// refused whole rather than written, since the container would refuse the list it read.
func WriteTrimList(root string, paths []string, files TrimListFiles) error {
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

	dir := filepath.Join(root, filepath.FromSlash(cachevol.TrimClaimListDir))
	if err := files.MkdirAll(dir, trimListDirPerm); err != nil {
		return fmt.Errorf("creating the trim list directory %s: %w", dir, err)
	}
	dest := filepath.Join(root, filepath.FromSlash(cachevol.TrimClaimListPath))
	tmp := dest + ".tmp"
	if err := files.WriteFile(tmp, data, trimListFilePerm); err != nil {
		return fmt.Errorf("writing the trim list under %s: %w", dir, err)
	}
	if err := files.Rename(tmp, dest); err != nil {
		return fmt.Errorf("moving the trim list into place under %s: %w", dir, err)
	}
	return nil
}
