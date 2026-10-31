package dashboard

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// The header's hold chip: the operator's dispatch hold file at the town root,
// the file whose existence parks every automatic sling (dispatch.HoldFileName).
// The queue pane says a hold skipped its rows far down the page, and the health
// line says "dispatch=silent" ten minutes later; the chip says it in the header
// the moment the reader looks (gt-zzkhh).

const (
	// holdNoteMax is the note the chip carries, in runes: the file's first line
	// says who set the hold and why, and the title prints no more.
	holdNoteMax = 120
	// holdReadMax bounds what the reader takes off the file before the note is
	// cut. The file is checked to be regular first, so this only spares the
	// poll a note somebody pasted a log into.
	holdReadMax = 4 << 10
)

// errHoldNotRegular is a hold path that is not a regular file — a directory, a
// FIFO or a device — from which no note can be read.
var errHoldNotRegular = errors.New("the hold path is not a regular file")

// Hold is the operator's dispatch hold as the header chip draws it: Present is
// nil when the file could not be read, so a failed read answers with neither a
// hold nor a clear town, and the page draws "hold?" rather than "dispatch on",
// which would tell the operator a hold is off that nothing has ruled out
// (gt-ifijm).
type Hold struct {
	// Present is whether the hold file exists at the town root. It is nil, not
	// false, when the read failed.
	Present *bool `json:"present"`
	// Note is the file's first line, cut to holdNoteMax runes: it carries who
	// set the hold and why. It is empty when the file is empty.
	Note string `json:"note,omitempty"`
	// AgeSeconds is how long ago the file was last written, from its mtime, so
	// the chip can say how long the hold has stood.
	AgeSeconds int64 `json:"age_seconds"`
	// Error is why the file could not be read, for the chip's title. It is
	// empty for a file that was read, present or not.
	Error string `json:"error,omitempty"`
}

// HoldReader reads the operator's dispatch hold file at the town root, and only
// reads it: the dashboard never creates, edits or removes the file.
type HoldReader struct {
	townRoot string
	// open is how the file is opened, os.Open unless a test sets it: the open
	// and read failures a permission bit or a bad disk produce are not ones a
	// test can make on demand, and the chip has to draw them as itself.
	open func(string) (io.ReadCloser, error)
}

// NewHoldReader returns a reader of the hold file at townRoot.
func NewHoldReader(townRoot string) *HoldReader { return &HoldReader{townRoot: townRoot} }

// opener is the reader's file opener.
func (r *HoldReader) opener() func(string) (io.ReadCloser, error) {
	if r.open != nil {
		return r.open
	}
	return func(path string) (io.ReadCloser, error) { return os.Open(path) }
}

// Read returns the hold as it stands at now: a town with no hold file answers
// Present false and a file the reader cannot use answers Error, so only a
// reader with no town told to it reports nothing at all.
func (r *HoldReader) Read(now time.Time) *Hold {
	if r == nil || r.townRoot == "" {
		return nil
	}
	note, written, err := readHold(dispatch.HoldFilePath(r.townRoot), r.opener())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		absent := false
		return &Hold{Present: &absent}
	case err != nil:
		return &Hold{Error: holdError(err)}
	}
	age := now.Sub(written)
	if age < 0 {
		age = 0
	}
	present := true
	return &Hold{Present: &present, Note: note, AgeSeconds: int64(age / time.Second)}
}

// readHold reads the first line of the hold file, cut to holdNoteMax runes, and
// the mtime its age comes from. A path that is not a regular file is refused
// before it is opened: opening a FIFO at the hold path would stall the poll.
func readHold(path string, open func(string) (io.ReadCloser, error)) (string, time.Time, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", time.Time{}, err
	}
	if !fi.Mode().IsRegular() {
		return "", time.Time{}, errHoldNotRegular
	}
	f, err := open(path)
	if err != nil {
		return "", time.Time{}, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, holdReadMax))
	if err != nil {
		return "", time.Time{}, err
	}
	line := string(raw)
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	return holdText(line), fi.ModTime(), nil
}

// holdText is a note or an error as the chip may show it: control characters
// dropped, trimmed, and cut to holdNoteMax runes with an ellipsis inside the
// cap, so a cut note is visibly cut and still prints no more than the cap.
func holdText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= holdNoteMax {
		return s
	}
	return string(r[:holdNoteMax-1]) + "…"
}

// holdError is a read failure as the chip's title carries it: one line, capped.
// It names the failure, never the file's contents.
func holdError(err error) string { return holdText(err.Error()) }
