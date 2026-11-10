// Package stateapi reads the town's file-backed state for the read-only state
// API. This file holds the line cursor that pages through a growing JSON-lines
// file, the same rule the landings reader and gt tail apply: a line is returned
// once its newline is written, and a file that was replaced or truncated is
// read again from its start.
package stateapi

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
)

// Cursor marks how far a client has read one file. Gen names the file by its
// device and inode, so a replaced file is recognized; Off is the byte offset
// just past the last line returned. The zero Cursor is a first read.
type Cursor struct {
	Gen string
	Off int64
}

// String encodes the cursor as gen:offset, the form ParseCursor reads.
func (c Cursor) String() string {
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
// without their newlines and otherwise as written, empty lines included. Cursor
// is where the next read starts. More reports complete lines left past this
// page. Reset reports that the cursor's file was replaced or truncated, so this
// read started again at the file's start.
type Page struct {
	Lines  []string
	Cursor Cursor
	More   bool
	Reset  bool
}

// scanChunk is how many bytes lineEndBefore reads at a time, walking backward.
const scanChunk = 4096

// errNoFileIdentity is returned when the platform gives no device and inode for
// a file, so a cursor cannot name it and a read is refused rather than guessed.
var errNoFileIdentity = errors.New("file identity (device and inode) is not available on this platform")

// file is what a read goes through: an open file and its metadata. An *os.File
// satisfies it; a test wraps one to fail a call on cue.
type file interface {
	io.ReadSeeker
	io.ReaderAt
	Stat() (os.FileInfo, error)
	Close() error
}

// opener opens the file at path. ReadLines uses the OS; a test substitutes one
// whose files fail on cue.
type opener func(path string) (file, error)

// openOS is the opener for files on disk. It returns a nil file on error, so
// a failed open is never a non-nil interface holding a nil *os.File.
func openOS(path string) (file, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// ReadLines returns up to max complete lines of the file at path that follow
// cur. A trailing line with no newline yet is held back for a later read. A
// missing file has no lines and is not an error: the page echoes cur.
func ReadLines(path string, cur Cursor, max int) (Page, error) {
	return readLines(openOS, path, cur, max, 0)
}

// ReadTail is ReadLines for a first read. With the zero cursor it starts at the
// first complete line that begins within the file's last tailBytes, so a client
// sees recent history without reading the whole file. A non-zero cursor reads
// as ReadLines does, and tailBytes 0 reads from the start.
func ReadTail(path string, cur Cursor, max int, tailBytes int64) (Page, error) {
	return readLines(openOS, path, cur, max, tailBytes)
}

func readLines(open opener, path string, cur Cursor, max int, tailBytes int64) (Page, error) {
	if max <= 0 {
		return Page{}, fmt.Errorf("max lines must be positive, got %d", max)
	}
	if cur.Off < 0 {
		return Page{}, fmt.Errorf("cursor offset %d is negative", cur.Off)
	}
	if tailBytes < 0 {
		return Page{}, fmt.Errorf("tailBytes must not be negative, got %d", tailBytes)
	}
	f, err := open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Page{Cursor: cur}, nil
	}
	if err != nil {
		return Page{}, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return Page{}, fmt.Errorf("checking %s: %w", path, err)
	}
	gen, err := fileGen(info)
	if err != nil {
		return Page{}, fmt.Errorf("checking %s: %w", path, err)
	}
	size := info.Size()

	first := cur == (Cursor{})
	reset := !first && (cur.Gen != gen || size < cur.Off)
	off := cur.Off
	switch {
	case reset:
		off = 0
	case first && tailBytes > 0:
		if off, err = tailStart(f, size, tailBytes); err != nil {
			return Page{}, fmt.Errorf("reading tail of %s: %w", path, err)
		}
	}

	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return Page{}, fmt.Errorf("seeking %s: %w", path, err)
	}
	// Reading stops at the size checked above, so a line appended meanwhile is
	// left for the next read rather than half-read now.
	r := bufio.NewReader(io.LimitReader(f, size-off))
	page := Page{Cursor: Cursor{Gen: gen, Off: off}, Reset: reset}
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			// No newline yet: the writer has not finished this line.
			break
		}
		if err != nil {
			return Page{}, fmt.Errorf("reading %s: %w", path, err)
		}
		if len(page.Lines) == max {
			page.More = true
			break
		}
		page.Lines = append(page.Lines, string(line[:len(line)-1]))
		page.Cursor.Off += int64(len(line))
	}
	return page, nil
}

// tailStart returns the offset a first read starts at, so that its lines are
// the complete ones beginning within the file's last tail bytes. The offset is
// always a line start: a line the window cuts is left out whole. When no line
// begins in the window, the offset is the end of the last complete line, so the
// cut line is not returned by a later read either.
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
	return lineEndBefore(f, size)
}

// lineEndBefore returns the offset just past the last newline in the first end
// bytes of f, or 0 when there is none.
func lineEndBefore(f file, end int64) (int64, error) {
	buf := make([]byte, scanChunk)
	for end > 0 {
		start := max(0, end-scanChunk)
		chunk := buf[:end-start]
		if _, err := f.ReadAt(chunk, start); err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(chunk, '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		end = start
	}
	return 0, nil
}

// fileGen names the file by its device and inode. The two are read by field
// name, not through syscall.Stat_t, so this file builds on Windows too; there
// the identity is unavailable and the read is refused rather than guessed.
func fileGen(info os.FileInfo) (string, error) {
	sys := reflect.ValueOf(info.Sys())
	if sys.Kind() == reflect.Pointer && !sys.IsNil() {
		sys = sys.Elem()
	}
	if sys.Kind() != reflect.Struct {
		return "", errNoFileIdentity
	}
	dev, ino := sys.FieldByName("Dev"), sys.FieldByName("Ino")
	if !dev.IsValid() || !ino.IsValid() {
		return "", errNoFileIdentity
	}
	return fmt.Sprintf("%v-%v", dev.Interface(), ino.Interface()), nil
}
