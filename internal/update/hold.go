package update

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// holdFileMode is the dispatch hold's mode. The hold is read by the dispatcher
// and by a person at a shell, so it is not owner-only like state.json.
const holdFileMode os.FileMode = 0o644

// FileOps is the file system the hold is read and written through, so a test
// drives every branch against a fake and never touches a real hold file.
type FileOps interface {
	// Stat returns nil when path exists, and an error satisfying
	// errors.Is(err, fs.ErrNotExist) when it does not.
	Stat(path string) error
	ReadFile(path string) ([]byte, error)
	// CreateExclusive creates path and fails with an error satisfying
	// errors.Is(err, fs.ErrExist) when it already exists, so the file system
	// decides which of two racing runs owns the hold.
	CreateExclusive(path string, data []byte, mode os.FileMode) error
	// WriteFile overwrites path.
	WriteFile(path string, data []byte, mode os.FileMode) error
	Remove(path string) error
}

// Hold is one run's handle on the operator's dispatch hold file.
type Hold struct {
	// Path is the hold file: dispatch.HoldFilePath(townRoot).
	Path string
	// Files is the file system the hold is read and written through.
	Files FileOps

	// owned records that this Hold's Acquire created the file. SetNote rewrites
	// the file and Release removes it only while this Hold owns it, so a hold a
	// person set by hand survives the run.
	owned bool
}

// NewHold returns the hold for townRoot, at the path the dispatcher reads.
func NewHold(townRoot string, files FileOps) *Hold {
	return &Hold{Path: dispatch.HoldFilePath(townRoot), Files: files}
}

// Acquire sets the hold with note when no hold exists, and reports whether this
// Hold owns it.
//
// When the file already exists, Acquire leaves it untouched and returns the
// file's content as the note; Owned stays true only when this Hold created the
// file on an earlier Acquire, so Release still removes it. When another run
// creates the file between the stat and the exclusive create, Acquire returns
// the content with Owned false. Any other error is returned and nothing is
// assumed: an unreadable hold is never read as "no hold" (fail closed), because
// dispatching through a hold is the outcome it exists to prevent.
func (h *Hold) Acquire(note string) (HoldRecord, error) {
	err := h.Files.Stat(h.Path)
	switch {
	case err == nil:
		return h.readExisting()
	case !errors.Is(err, fs.ErrNotExist):
		return HoldRecord{}, fmt.Errorf("stat dispatch hold %s: %w", h.Path, err)
	}
	if err := h.Files.CreateExclusive(h.Path, []byte(note), holdFileMode); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return HoldRecord{}, fmt.Errorf("create dispatch hold %s: %w", h.Path, err)
		}
		// The file existed when the create ran: a person or another run set it
		// between the stat and the create, and it is theirs, not ours.
		h.owned = false
		return h.readExisting()
	}
	h.owned = true
	return HoldRecord{Owned: true, Note: note}, nil
}

// readExisting reports the current hold with its content as the note. Owned is
// whatever this Hold already holds, so a read failure leaves ownership in place
// for a later Release to act on.
func (h *Hold) readExisting() (HoldRecord, error) {
	data, err := h.Files.ReadFile(h.Path)
	if err != nil {
		return HoldRecord{}, fmt.Errorf("read dispatch hold %s: %w", h.Path, err)
	}
	return HoldRecord{Owned: h.owned, Note: string(data)}, nil
}

// SetNote rewrites the note of a hold this Hold owns; on a foreign hold it does
// nothing and returns nil.
func (h *Hold) SetNote(note string) error {
	if !h.owned {
		return nil
	}
	if err := h.Files.WriteFile(h.Path, []byte(note), holdFileMode); err != nil {
		return fmt.Errorf("write dispatch hold %s: %w", h.Path, err)
	}
	return nil
}

// Release removes the hold when this Hold owns it, and leaves a foreign hold in
// place. Releasing an owned hold whose file is already gone is not an error; a
// remove that fails for another reason returns the error and keeps ownership,
// so a later Release can retry.
func (h *Hold) Release() error {
	if !h.owned {
		return nil
	}
	if err := h.Files.Remove(h.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove dispatch hold %s: %w", h.Path, err)
	}
	h.owned = false
	return nil
}

// WithHold acquires the hold, runs fn with the record and releases the hold
// when fn returns, so an owned hold is removed even when fn fails or panics. A
// foreign hold still runs fn, with Owned false, and is not removed.
//
// ctx is the run's context, which fn observes; Release takes no context, so a
// canceled run still clears a hold it owns.
func WithHold(ctx context.Context, h *Hold, note string, fn func(HoldRecord) error) (err error) {
	rec, err := h.Acquire(note)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := h.Release(); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()
	return fn(rec)
}
