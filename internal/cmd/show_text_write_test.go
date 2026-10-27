package cmd

import (
	"errors"
	"strings"
	"testing"
)

// The terminal path of gt show: bd's text is printed, and bd's failure comes
// back as the command's error, unchanged (gt-p85pw.10). The function takes the
// writer, so the test needs no terminal.
func TestWriteShowTextPrintsTheTextAndReturnsTheError(t *testing.T) {
	t.Parallel()
	bdErr := errors.New("bd show gt-x: exit status 1: no issue found")

	var out strings.Builder
	if err := writeShowText(&out, "◇ gt-x · title\n", nil); err != nil || out.String() != "◇ gt-x · title\n" {
		t.Errorf("success: wrote %q, err %v; want the text and no error", out.String(), err)
	}

	out.Reset()
	if err := writeShowText(&out, "partial text\n", bdErr); !errors.Is(err, bdErr) || out.String() != "partial text\n" {
		t.Errorf("failure with text: wrote %q, err %v; want the text and bd's error", out.String(), err)
	}

	out.Reset()
	if err := writeShowText(&out, "", bdErr); !errors.Is(err, bdErr) || out.Len() != 0 {
		t.Errorf("failure with no text: wrote %q, err %v; want nothing and bd's error", out.String(), err)
	}
}

// A writer that refuses the text makes writeShowText return the write error,
// not bd's: a closed stdout is reported instead of being lost (gt-p85pw.10,
// gt-g25ql).
func TestWriteShowTextReturnsTheWriteError(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("write |1: broken pipe")
	bdErr := errors.New("bd show gt-x: exit status 1: no issue found")

	got := writeShowText(refusingWriter{err: writeErr}, "partial text\n", bdErr)
	if !errors.Is(got, writeErr) {
		t.Errorf("err = %v, want the writer's error", got)
	}
	if errors.Is(got, bdErr) {
		t.Errorf("err = %v, want the write error, not bd's", got)
	}
}

// refusingWriter refuses every write with err.
type refusingWriter struct{ err error }

func (w refusingWriter) Write([]byte) (int, error) { return 0, w.err }
