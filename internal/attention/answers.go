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
	path string

	mu     sync.Mutex
	offset int64
	// tail is the last answersTailBytes of the consumed log, checked before the
	// read continues: a replacement that is not shorter would otherwise be
	// read from the middle of a line.
	tail []byte
	// stat is the file as it stood at the last read, for the quiet check.
	stat   os.FileInfo
	scores Scores
}

// NewScoreReader reads the scores from the answers log at path.
func NewScoreReader(path string) *ScoreReader {
	return &ScoreReader{path: path}
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

	// The stat is the quiet check alone: a path it cannot stat is opened anyway,
	// so the open's error is what names why the log could not be read.
	fi, _ := os.Stat(r.path)
	if fi != nil && r.stat != nil && os.SameFile(fi, r.stat) &&
		fi.ModTime().Equal(r.stat.ModTime()) && fi.Size() == r.offset {
		return r.copy(), nil // nothing appended since the last read
	}

	f, err := os.Open(r.path) //nolint:gosec // G304: the path is the town's own runtime file
	if err != nil {
		if os.IsNotExist(err) {
			// the battery has not run, or the log was removed: no item has an answer,
			// so the answers last read from it are dropped rather than served
			r.offset, r.scores, r.tail, r.stat = 0, nil, nil, nil
			return r.copy(), nil
		}
		return r.copy(), fmt.Errorf("reading %s: %w", r.path, err)
	}
	defer func() { _ = f.Close() }()
	// A truncated or replaced log makes every offset read so far name a line
	// that is gone or changed under it, so the read starts over. The reset is
	// applied only once the read succeeded: a failed read leaves the last good
	// answers in place beside its error.
	start, scores, tail := r.offset, r.scores, r.tail
	if (fi != nil && fi.Size() < start) || !r.continues(f) {
		start, scores, tail = 0, nil, nil
	}
	data, err := io.ReadAll(io.NewSectionReader(f, start, math.MaxInt64))
	if err != nil {
		return r.copy(), fmt.Errorf("reading %s: %w", r.path, err)
	}
	r.stat = fi
	// A pass killed mid-write can leave its last line cut short. Consuming only
	// whole lines leaves that line for the next read, after the write finished.
	end := bytes.LastIndexByte(data, '\n') + 1
	if end == 0 {
		r.offset, r.scores, r.tail = start, scores, tail
		return r.copy(), nil
	}
	if scores == nil {
		scores = Scores{}
	}
	for _, raw := range bytes.Split(data[:end], []byte("\n")) {
		var l answerLine
		if err := json.Unmarshal(raw, &l); err != nil || l.Key == "" || l.FirstSeen.IsZero() || l.Answers == nil {
			continue // a failed call has no answer to score with
		}
		scores[LifeKey(l.Key, l.FirstSeen)] = l.Answers.NeedsHuman
	}
	r.offset = start + int64(end)
	r.scores = scores
	r.tail = lastBytes(append(tail, data[:end]...), answersTailBytes)
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

// continues reports whether the bytes before the read offset are still the
// bytes that were consumed: a log swapped for another that is not shorter is
// otherwise read on from the middle of whatever line now sits at the offset.
func (r *ScoreReader) continues(f *os.File) bool {
	if len(r.tail) == 0 {
		return true
	}
	got := make([]byte, len(r.tail))
	if _, err := f.ReadAt(got, r.offset-int64(len(r.tail))); err != nil {
		return false
	}
	return bytes.Equal(got, r.tail)
}

// lastBytes returns a copy of b's last n bytes.
func lastBytes(b []byte, n int) []byte {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return append([]byte(nil), b...)
}
