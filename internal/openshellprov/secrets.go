package openshellprov

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
)

// maskText is what each run of occurrences of the secret is replaced with.
const maskText = "***"

// KeyValue returns the value of key in a KEY=value env file. The file is read
// as data and never sourced: a line that is not an assignment to key is
// ignored, the last assignment wins, a leading "export " and one pair of
// surrounding single or double quotes are stripped, and nothing in the value
// is expanded. Whitespace around a line, and a carriage return at its end, is
// not part of the key or the value. A key the file does not set, or sets to
// nothing, returns "" and no error; the caller refuses an empty secret.
func KeyValue(file, key string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("%s: reading key file: %w", Name, err)
	}
	value := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "export ")
		if !ok {
			rest = line
		}
		assignment, ok := strings.CutPrefix(rest, key+"=")
		if !ok {
			continue
		}
		value = unquote(strings.TrimSpace(assignment))
	}
	return value, nil
}

// unquote strips one pair of matching single or double quotes around s. A lone
// quote is not a pair and is kept.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// Masked returns a writer that copies to w with every occurrence of secret
// replaced by ***. It matches the secret itself, not a pattern, so a one-
// character key is hidden as surely as a long one. Occurrences that overlap or
// touch form one maximal run of covered bytes, and each run is written as one
// ***. An occurrence split across Writes is still replaced: the writer holds
// back only the bytes that could still begin an occurrence, never more than
// len(secret)-1 of them, however long the stream runs. Each Write costs
// O(len(p)*len(secret)).
//
// Close must be called to flush what is held. A held tail is always a prefix of
// the secret, so it is written as *** unless the run before it already masks it:
// a partial secret is a secret, and it is never written in the clear. Close
// ends the stream: the writer holds nothing after it. An empty secret masks
// nothing, so Masked passes w through; the caller refuses an empty secret before
// it gets here.
func Masked(secret string, w io.Writer) io.WriteCloser {
	if secret == "" {
		return nopCloser{w}
	}
	return &maskWriter{secret: []byte(secret), w: w}
}

// nopCloser is the writer Masked returns for an empty secret: it passes every
// byte through and has nothing to flush.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// maskWriter holds back the bytes that might still become part of the secret.
type maskWriter struct {
	secret []byte
	w      io.Writer
	// held is the undecided tail: a proper prefix of secret, starting at the first
	// byte a later write could still complete into an occurrence.
	held []byte
	// inRun reports that a run of occurrences is open at the start of held.
	// covered is how far into held that run reaches; a byte at or past covered
	// starts no run and is written.
	inRun   bool
	covered int
}

// Write masks p and writes what is settled. The undecided tail stays held for a
// later Write. When the underlying write fails, Write returns 0 and keeps the
// writer's state, so the same bytes can be written again, as io.Writer requires.
func (m *maskWriter) Write(p []byte) (int, error) {
	next := *m
	out := next.settle(append(append([]byte(nil), m.held...), p...), false)
	if err := writeOut(m.w, out); err != nil {
		return 0, err
	}
	*m = next
	return len(p), nil
}

// Close writes the held tail, masked as described on Masked, and ends the
// stream. It writes nothing when no tail is held.
func (m *maskWriter) Close() error {
	next := *m
	out := next.settle(m.held, true)
	if err := writeOut(m.w, out); err != nil {
		return err
	}
	*m = next
	return nil
}

// writeOut writes out to w, and writes nothing for an empty out.
func writeOut(w io.Writer, out []byte) error {
	if len(out) == 0 {
		return nil
	}
	_, err := w.Write(out)
	return err
}

// settle masks every byte of buf that no later byte can change, and holds the
// rest in m.held. final means no bytes follow buf, so a tail that is a prefix
// of the secret is decided as it stands. It returns the masked output.
//
// Each position is decided once, in order. An occurrence that starts there
// opens a run, or extends the open one when it overlaps or touches it, so the
// run is written as one ***. A byte inside the open run is covered and not
// written. Any other byte is written. A position where buf ends inside a
// possible occurrence is held, because a later byte could complete it.
func (m *maskWriter) settle(buf []byte, final bool) []byte {
	var out []byte
	for i := 0; i < len(buf); i++ {
		occ, partial := occursAt(buf[i:], m.secret)
		switch {
		case partial && !final:
			// A later byte could complete an occurrence here, so hold from i on.
			m.held = buf[i:]
			m.covered = max(m.covered-i, 0)
			return out
		case partial:
			// The input ends inside an occurrence. Its bytes are masked by the run
			// before them when there is one, and by a mask of their own otherwise.
			if !m.inRun {
				out = append(out, maskText...)
			}
			m.held, m.inRun, m.covered = nil, false, 0
			return out
		case occ:
			if !m.inRun {
				out = append(out, maskText...)
			}
			m.inRun = true
			m.covered = max(m.covered, i+len(m.secret))
		case i < m.covered:
			// Inside the open run: already masked, so not written.
		default:
			m.inRun = false
			out = append(out, buf[i])
		}
	}
	m.held = nil
	m.covered = max(m.covered-len(buf), 0)
	return out
}

// occursAt reports whether secret starts at the front of rest. partial reports
// that rest is shorter than secret and is a prefix of it, so the answer depends
// on bytes not yet written.
func occursAt(rest, secret []byte) (occ, partial bool) {
	if len(rest) >= len(secret) {
		return bytes.Equal(rest[:len(secret)], secret), false
	}
	return false, bytes.HasPrefix(secret, rest)
}
