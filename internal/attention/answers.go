package attention

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// answersTailBytes is how much of the consumed log the reader keeps and re-checks
// before reading on, so a log rewritten in place is noticed as well as one that
// got shorter.
const answersTailBytes = 256

// AnswersPath is the log Jev's attention battery writes one line per item to,
// {ts, key, first_seen, answers:{needs_human,...}}. The shared attention readers
// read their scores from it, so the path and the fields they need live here
// rather than in the battery's own package (gt-b9j3o.2).
func AnswersPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "attention.jsonl")
}

// answerLine is the part of one log line the scores come from. The battery's
// line carries more (the item's kind, its model, the other answers); a reader
// that needs those reads the battery's own type.
type answerLine struct {
	Key       string    `json:"key"`
	FirstSeen time.Time `json:"first_seen"`
	Answers   *struct {
		NeedsHuman float64 `json:"needs_human"`
	} `json:"answers"`
}

// ScoreReader returns the newest needs_human answer for each item life from one
// answers log, keyed by LifeKey. It reads only the lines appended since its last
// call, so a reader that runs on every dashboard publish costs one stat while
// the log is quiet.
//
// It starts over when the file got shorter, or when the bytes it already
// consumed are no longer the bytes at those offsets, so an item is never scored
// by an answer from a file that was replaced. It is safe for concurrent use, and
// each call returns its own map: a caller may keep or change what it was given
// without touching the next call's answers.
type ScoreReader struct {
	mu sync.Mutex
	// tail is the read position in one answers log, shared with the landing
	// risk reader because the two logs are read the same way.
	tail   consumedTail
	scores Scores
}

// NewScoreReader reads the scores from the answers log at path.
func NewScoreReader(path string) *ScoreReader {
	return &ScoreReader{tail: consumedTail{path: path}}
}

// Scores returns the needs_human answer per item life as the log stands, and
// the error of a log that exists but could not be read. A missing log is no
// answers and no error: the battery has not run, so no item has an answer yet.
// An unreadable one returns the answers of the last read beside the error, so a
// caller can say the scores are unreadable rather than draw them as unanswered
// (gt-b9j3o.2).
func (r *ScoreReader) Scores() (Scores, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	data, restart, err := r.tail.next()
	if err != nil {
		return r.copy(), err
	}
	if restart {
		r.scores = nil
	}
	if len(data) == 0 {
		return r.copy(), nil
	}
	if r.scores == nil {
		r.scores = Scores{}
	}
	for _, raw := range bytes.Split(data, []byte("\n")) {
		var l answerLine
		if err := json.Unmarshal(raw, &l); err != nil || l.Key == "" || l.FirstSeen.IsZero() || l.Answers == nil {
			continue // a failed call has no answer to score with
		}
		r.scores[LifeKey(l.Key, l.FirstSeen)] = l.Answers.NeedsHuman
	}
	return r.copy(), nil
}

// copy returns the reader's answers as a map of its own, so a caller holding the
// last result does not see it change under the next read.
func (r *ScoreReader) copy() Scores {
	if len(r.scores) == 0 {
		return nil
	}
	out := make(Scores, len(r.scores))
	for life, score := range r.scores {
		out[life] = score
	}
	return out
}

// consumedTail is the incremental read the answer readers share: the whole
// lines appended after the offset it already consumed, and the state that
// notices a log which shrank or was replaced. A reader holds one under its own
// lock and folds the lines into the answers it keeps.
type consumedTail struct {
	path   string
	offset int64
	// tail is the last answersTailBytes of the consumed log, checked before the
	// read continues: a replacement that is not shorter would otherwise be
	// read from the middle of a line.
	tail []byte
	// stat is the file as it stood at the last read, for the quiet check.
	stat os.FileInfo
}

// next returns the whole lines appended since the last call and whether the
// read started over. A start-over means every offset read so far names a line
// that is gone or changed under it, so the lines returned are the log from its
// first byte and the caller drops everything it kept from it. A missing log is
// a start-over with no lines.
//
// A log that exists but cannot be read returns the error with the read
// position untouched, so a later call continues from the last good read. Only
// whole lines are returned: a pass killed mid-write leaves its last line cut
// short, and that line waits for the read after the write finished.
func (t *consumedTail) next() (data []byte, restart bool, err error) {
	// The stat is the quiet check alone: a path it cannot stat is opened anyway,
	// so the open's error is what names why the log could not be read.
	fi, _ := os.Stat(t.path)
	if fi != nil && t.stat != nil && os.SameFile(fi, t.stat) &&
		fi.ModTime().Equal(t.stat.ModTime()) && fi.Size() == t.offset {
		return nil, false, nil // nothing appended since the last read
	}

	f, err := os.Open(t.path) //nolint:gosec // G304: the path is a town runtime file
	if err != nil {
		if os.IsNotExist(err) {
			// the battery has not run, or the log was removed: nothing it said holds
			t.offset, t.tail, t.stat = 0, nil, nil
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("reading %s: %w", t.path, err)
	}
	defer func() { _ = f.Close() }()

	start := t.offset
	if (fi != nil && fi.Size() < start) || !t.continues(f) {
		start, restart = 0, true
	}
	all, err := io.ReadAll(io.NewSectionReader(f, start, math.MaxInt64))
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", t.path, err)
	}
	t.stat = fi
	end := bytes.LastIndexByte(all, '\n') + 1
	if end == 0 {
		if restart {
			t.offset, t.tail = start, nil
		}
		return nil, restart, nil
	}
	if restart {
		t.tail = nil
	}
	t.offset = start + int64(end)
	t.tail = lastBytes(append(t.tail, all[:end]...), answersTailBytes)
	return all[:end], restart, nil
}

// continues reports whether the bytes before the read offset are still the
// bytes that were consumed: a log swapped for another that is not shorter is
// otherwise read on from the middle of whatever line now sits at the offset.
func (t *consumedTail) continues(f *os.File) bool {
	if len(t.tail) == 0 {
		return true
	}
	got := make([]byte, len(t.tail))
	if _, err := f.ReadAt(got, t.offset-int64(len(t.tail))); err != nil {
		return false
	}
	return bytes.Equal(got, t.tail)
}

// lastBytes returns a copy of b's last n bytes.
func lastBytes(b []byte, n int) []byte {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return append([]byte(nil), b...)
}
