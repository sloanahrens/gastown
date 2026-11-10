//go:build unix

// Package stateapi reads the town's file-backed state for the read-only state
// API. This file holds the line cursor that pages through a growing JSON-lines
// file. A read detects two replacements: a file whose device or inode differs
// from the cursor's, and a file smaller than the cursor's offset. Either one
// resets the read to the file's start. An in-place rewrite that keeps the inode
// and leaves the file at least as large as the offset is not detected.
package stateapi

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	// MaxLineBytes is the longest line a read returns, without its newline. A
	// longer line is an error, so one unfinished line cannot grow a read without
	// limit.
	MaxLineBytes = 1 << 20

	// MaxPageBytes bounds the bytes one page returns, newlines included, so a
	// page of many short lines cannot grow without limit either.
	MaxPageBytes = 4 * MaxLineBytes
)

// Cursor marks how far a client has read one file. Gen is the file's device and
// inode, written dev-ino, and Off is the byte offset just past the last line
// returned. A read resets to the file's start when Gen differs from the file's
// device and inode, or when the file is smaller than Off. An in-place rewrite
// that keeps the inode and a size of at least Off is not detected. The zero
// Cursor is a first read, which is never a reset.
type Cursor struct {
	Gen string
	Off int64
}

// String encodes the cursor as gen:offset, the form ParseCursor reads. The zero
// Cursor encodes as the empty string, which ParseCursor reads back as the zero
// Cursor.
func (c Cursor) String() string {
	if c == (Cursor{}) {
		return ""
	}
	return c.Gen + ":" + strconv.FormatInt(c.Off, 10)
}

// ParseCursor reads a cursor that Cursor.String wrote. The empty string is the
// zero cursor, because a first read has none; anything else that is not
// gen:offset is an error.
func ParseCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	i := strings.LastIndexByte(s, ':')
	if i <= 0 {
		return Cursor{}, fmt.Errorf("cursor %q is not in gen:offset form", s)
	}
	off, err := strconv.ParseInt(s[i+1:], 10, 64)
	if err != nil || off < 0 {
		return Cursor{}, fmt.Errorf("cursor %q is not in gen:offset form: offset must be a non-negative integer", s)
	}
	return Cursor{Gen: s[:i], Off: off}, nil
}

// Page is one read of a file. Lines are its complete lines after the cursor,
// without their newlines, empty lines included. Cursor is where the next read
// starts. More reports complete lines left past this page. Reset reports that
// the cursor's file was replaced or truncated, so this read started at the
// file's start.
type Page struct {
	Lines  []string
	Cursor Cursor
	More   bool
	Reset  bool
}

// File is a file a read may open. Only Confine makes one, so a read never takes
// a path a caller spelled out. The zero File is refused.
type File struct {
	path string
}

// errNoFileIdentity is returned when a file's stat carries no device and inode,
// so a cursor cannot name it and a read is refused rather than guessed.
var errNoFileIdentity = errors.New("file identity (device and inode) is not available")

// file is what a read goes through: an open file and its metadata. An *os.File
// satisfies it; a test wraps one to fail a call on cue.
type file interface {
	io.ReadSeeker
	io.ReaderAt
	Stat() (os.FileInfo, error)
	Close() error
}

// opener opens the file at path. ReadLines and ReadTail use the OS; a test
// substitutes one whose files fail on cue.
type opener func(path string) (file, error)

// openOS is the opener for files on disk. It returns a nil file on error, so a
// failed open is never a non-nil interface holding a nil *os.File.
func openOS(path string) (file, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Confine returns the File at path under root, which must be absolute, or an
// error when the path would read outside root. It refuses a path with a ".."
// element, resolves symlinks in root and in the path's directory, and refuses a
// file that resolves outside the resolved root, including through a symlink. It
// refuses a file that is not a regular file: opening a FIFO can block until a
// writer arrives, and a directory or device is not a file of lines. A missing
// file is accepted, since a read of one is an empty page. Confine checks links
// and file types as they are when it is called; a link or file swapped in after
// that is not checked again.
func Confine(root, path string) (File, error) {
	if !filepath.IsAbs(root) {
		return File{}, fmt.Errorf("root %q is not an absolute path", root)
	}
	for _, el := range strings.FieldsFunc(path, isSeparator) {
		if el == ".." {
			return File{}, fmt.Errorf("path %q has a .. element", path)
		}
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return File{}, fmt.Errorf("resolving root %s: %w", root, err)
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(realRoot, path)
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return File{}, fmt.Errorf("resolving directory of %s: %w", path, err)
	}
	full := filepath.Join(dir, filepath.Base(abs))
	info, err := os.Lstat(full)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		if full, err = filepath.EvalSymlinks(full); err != nil {
			return File{}, fmt.Errorf("resolving %s: %w", path, err)
		}
		// The resolved path holds no symlink, so its Lstat is the file's own.
		info, err = os.Lstat(full)
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return File{}, fmt.Errorf("checking %s: %w", path, err)
	}
	if !within(realRoot, full) {
		return File{}, fmt.Errorf("path %q is outside the root", path)
	}
	// info is nil only when the file is missing, which is accepted above.
	if info != nil && !info.Mode().IsRegular() {
		return File{}, fmt.Errorf("path %q is not a regular file", path)
	}
	return File{path: full}, nil
}

func isSeparator(r rune) bool {
	return r == '/' || r == filepath.Separator
}

// within reports whether p is strictly inside root. Both must be resolved.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ReadLines returns up to max complete lines of f that follow cur. A trailing
// line with no newline yet is held back for a later read. A missing file has no
// lines and is not an error: the page echoes cur. When a line is longer than
// MaxLineBytes the error names the cap, and the page holds the lines before that
// line with a cursor that stops before it.
func ReadLines(f File, cur Cursor, max int) (Page, error) {
	if max <= 0 {
		return Page{}, fmt.Errorf("max lines must be positive, got %d", max)
	}
	return read(openOS, f, cur, max, 0)
}

// ReadTail returns the complete lines of f that begin within its last tailBytes,
// as a first read, so the cursor it returns is where the next ReadLines starts.
// The page starts on a line start, so a line the window cuts is left out whole.
// tailBytes must be between 1 and MaxPageBytes.
func ReadTail(f File, tailBytes int) (Page, error) {
	if tailBytes <= 0 || tailBytes > MaxPageBytes {
		return Page{}, fmt.Errorf("tailBytes must be between 1 and %d, got %d", MaxPageBytes, tailBytes)
	}
	return read(openOS, f, Cursor{}, math.MaxInt, int64(tailBytes))
}

// read is the shared body of ReadLines and ReadTail. maxLines bounds the lines
// returned; tail, when positive, makes this a first read of the window it names.
func read(open opener, f File, cur Cursor, maxLines int, tail int64) (page Page, err error) {
	if f.path == "" {
		return Page{}, errors.New("file was not returned by Confine")
	}
	if cur.Off < 0 {
		return Page{}, fmt.Errorf("cursor offset %d is negative", cur.Off)
	}
	fh, err := open(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return Page{Cursor: cur}, nil
	}
	if err != nil {
		return Page{}, fmt.Errorf("opening %s: %w", f.path, err)
	}
	defer func() {
		// A close error on a file read only cannot change the lines returned,
		// but it is reported when nothing else already is.
		if cerr := fh.Close(); cerr != nil && err == nil {
			page, err = Page{}, fmt.Errorf("closing %s: %w", f.path, cerr)
		}
	}()
	info, err := fh.Stat()
	if err != nil {
		return Page{}, fmt.Errorf("checking %s: %w", f.path, err)
	}
	gen, err := fileGen(info)
	if err != nil {
		return Page{}, fmt.Errorf("checking %s: %w", f.path, err)
	}
	size := info.Size()

	first := cur == Cursor{}
	reset := !first && (cur.Gen != gen || size < cur.Off)
	var off int64
	switch {
	case reset:
	case first && tail > 0:
		if off, err = tailStart(fh, size, tail); err != nil {
			return Page{}, fmt.Errorf("reading tail of %s: %w", f.path, err)
		}
	case !first:
		if err := checkLineStart(fh, cur.Off); err != nil {
			return Page{}, fmt.Errorf("cursor for %s: %w", f.path, err)
		}
		off = cur.Off
	}

	if _, err := fh.Seek(off, io.SeekStart); err != nil {
		return Page{}, fmt.Errorf("seeking %s: %w", f.path, err)
	}
	// Reading stops at the size checked above, so a line appended meanwhile is
	// left for the next read rather than half-read now. The buffer is one byte
	// longer than a line may be, so a ReadSlice that fills it has seen a line
	// past the cap and has read no more than cap+1 bytes of it.
	r := bufio.NewReaderSize(io.LimitReader(fh, size-off), MaxLineBytes+1)
	page = Page{Cursor: Cursor{Gen: gen, Off: off}, Reset: reset}
	var pageBytes int64
	for {
		raw, err := r.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			return page, fmt.Errorf("line at byte %d of %s is longer than the %d-byte line cap", page.Cursor.Off, f.path, MaxLineBytes)
		case errors.Is(err, io.EOF):
			// No newline yet: the writer has not finished this line.
			return page, nil
		case err != nil:
			return Page{}, fmt.Errorf("reading %s: %w", f.path, err)
		}
		n := int64(len(raw))
		if len(page.Lines) == maxLines {
			page.More = true
			return page, nil
		}
		if len(page.Lines) > 0 && pageBytes+n > MaxPageBytes {
			page.More = true
			return page, nil
		}
		page.Lines = append(page.Lines, string(raw[:len(raw)-1]))
		pageBytes += n
		page.Cursor.Off += n
	}
}

// checkLineStart refuses a cursor offset that is not at a line start. A cursor is
// only ever set just past a newline, so a byte before the offset that is not one
// means the cursor does not name a line of this file.
func checkLineStart(f file, off int64) error {
	if off == 0 {
		return nil
	}
	var b [1]byte
	if _, err := f.ReadAt(b[:], off-1); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading the byte before offset %d: %w", off, err)
	}
	if b[0] != '\n' {
		return fmt.Errorf("cursor offset %d is not at a line start", off)
	}
	return nil
}

// tailStart returns the offset a first read starts at, so that its lines begin
// within the file's last tail bytes, each on a line boundary. When the window
// holds no newline, the offset is just past the file's last newline, so the
// partial line after it stays for a later read.
func tailStart(f file, size, tail int64) (int64, error) {
	s := size - tail
	if s <= 0 {
		return 0, nil
	}
	// The byte before the window says whether the window opens a line.
	win := make([]byte, size-(s-1))
	if _, err := f.ReadAt(win, s-1); err != nil {
		return 0, err
	}
	if win[0] == '\n' {
		return s, nil
	}
	if i := bytes.IndexByte(win[1:], '\n'); i >= 0 {
		return s + int64(i) + 1, nil
	}
	return lastLineStart(f, size)
}

// lastLineStart returns the offset just past the file's last newline, looking
// only within MaxLineBytes+1 bytes of the end. A file of more than MaxLineBytes
// bytes with no newline in that span ends in a line longer than the cap, which
// is refused. A file of MaxLineBytes or fewer bytes with no newline is one line,
// which starts at 0.
func lastLineStart(f file, size int64) (int64, error) {
	lo := max(0, size-(MaxLineBytes+1))
	span := make([]byte, size-lo)
	if _, err := f.ReadAt(span, lo); err != nil {
		return 0, err
	}
	if i := bytes.LastIndexByte(span, '\n'); i >= 0 {
		return lo + int64(i) + 1, nil
	}
	if size-lo > MaxLineBytes {
		return 0, fmt.Errorf("the last line of the file is longer than the %d-byte line cap", MaxLineBytes)
	}
	return 0, nil
}

// fileGen names a file by its device and inode, read from the platform stat the
// file system reports. A FileInfo without one is refused, not guessed.
func fileGen(info os.FileInfo) (string, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errNoFileIdentity
	}
	return fmt.Sprintf("%d-%d", st.Dev, st.Ino), nil
}
