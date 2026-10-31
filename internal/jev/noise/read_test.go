package noise

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/logtriage"
)

// TestLogPathRefusesABatteryWithoutRepeats: only logtriage writes a repeat log,
// so any other battery name is refused rather than read as an empty log.
func TestLogPathRefusesABatteryWithoutRepeats(t *testing.T) {
	t.Parallel()
	_, err := LogPath("dedupe", t.TempDir())
	assert.ErrorContains(t, err, `battery "dedupe" writes no repeat log`)

	got, err := LogPath(logtriageName, "/town")
	require.NoError(t, err)
	assert.Equal(t, logtriage.LogPath("/town"), got)
}

// TestReadRefusesAnUnreadableLog: a log that cannot be read is an error, never
// an empty log that would report a clean repeat set.
func TestReadRefusesAnUnreadableLog(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T, logPath string)
		want  string
	}{
		{
			name: "battery log is a directory",
			setup: func(t *testing.T, logPath string) {
				require.NoError(t, os.MkdirAll(logPath, 0o700))
			},
			want: "reading the battery log",
		},
		{
			name: "repeat log cannot be inspected",
			setup: func(t *testing.T, logPath string) {
				// A symlink that points at itself: stat fails with a loop, not "missing".
				reask := battery.ReaskPath(logPath)
				require.NoError(t, os.Symlink(filepath.Base(reask), reask))
			},
			want: "checking the repeat log",
		},
		{
			name: "repeat log is a directory",
			setup: func(t *testing.T, logPath string) {
				require.NoError(t, os.MkdirAll(battery.ReaskPath(logPath), 0o700))
			},
			want: "reading the repeat log",
		},
		{
			name: "repeat answers are not an object",
			setup: func(t *testing.T, logPath string) {
				require.NoError(t, battery.AppendReask(logPath, battery.ReaskLine{
					TS: at(1), Key: "k", QuestionVersion: logtriage.Version, Answers: "oops",
				}))
			},
			want: "decoding a repeat's answers",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logPath := filepath.Join(t.TempDir(), "logtriage.jsonl")
			tc.setup(t, logPath)

			_, err := Read(logPath)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

// TestFromReaskRefusesAnswersThatWillNotEncode: answers that cannot be written as
// JSON are an error, so a repeat is never converted from a partial value.
func TestFromReaskRefusesAnswersThatWillNotEncode(t *testing.T) {
	t.Parallel()
	_, err := fromReask(battery.ReaskLine{TS: at(1), Key: "k", Answers: make(chan int)})
	assert.ErrorContains(t, err, "encoding a repeat's answers")
}

// failingWriter accepts the first ok writes and fails every write after them.
type failingWriter struct{ ok int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.ok <= 0 {
		return 0, errors.New("disk full")
	}
	w.ok--
	return len(p), nil
}

// TestWriteTextReportsAWriteFailure: each write the table makes can fail, and
// the failure is returned instead of a truncated table reported as complete.
func TestWriteTextReportsAWriteFailure(t *testing.T) {
	t.Parallel()
	oneRow := Report{Battery: "logtriage", Cut: 0.5, Rows: []Row{{Question: "q", Kind: KindNoul, Items: 1}}}
	cases := []struct {
		name string
		rep  Report
		ok   int
	}{
		{name: "header", rep: Report{Battery: "logtriage"}, ok: 0},
		{name: "no rows line", rep: Report{Battery: "logtriage"}, ok: 1},
		{name: "question header", rep: oneRow, ok: 1},
		{name: "question line", rep: oneRow, ok: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := WriteText(&failingWriter{ok: tc.ok}, tc.rep)
			assert.ErrorContains(t, err, "disk full")
		})
	}
	// The same report to a writer that never fails is not an error.
	var out bytes.Buffer
	require.NoError(t, WriteText(&out, oneRow))
	assert.Contains(t, out.String(), "q (noul)")
}
