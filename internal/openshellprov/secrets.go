package openshellprov

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
)

// maskText is what each occurrence of the secret is replaced with.
const maskText = "***"

// KeyValue returns the value of key in a KEY=value env file. The file is read
// as data and never sourced: a line that is not an assignment to key is
// ignored, the last assignment wins, a leading "export " and one pair of
// surrounding single or double quotes are stripped, and nothing in the value
// is expanded. A key the file does not set, or sets to nothing, returns "" and
// no error; the caller refuses an empty secret.
func KeyValue(file, key string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("%s: reading key file: %w", Name, err)
	}
	value := ""
	for _, line := range strings.Split(string(data), "\n") {
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
// character key is hidden as surely as a long one. Occurrences that overlap are
// one run and get one ***. An occurrence split across two Writes is still
// replaced: the bytes that could still begin or extend an occurrence are held
// back until a later Write settles them. Output that ends while bytes are held
// is not written, which costs the tail of the stream but never shows the
// secret. An empty secret masks nothing, so Masked returns w unchanged; the
// caller refuses an empty secret before it gets here.
func Masked(secret string, w io.Writer) io.Writer {
	if secret == "" {
		return w
	}
	return &maskWriter{secret: []byte(secret), w: w}
}

// maskWriter holds back the bytes that might still become part of the secret.
type maskWriter struct {
	secret []byte
	w      io.Writer
	// held is the tail of the input not yet written: from the start of the first
	// occurrence that a later byte could still extend.
	held []byte
}

// Write masks p and writes what is settled to the underlying writer. The held
// tail is written by a later Write. When the underlying write fails, Write
// consumes nothing and keeps its state, as io.Writer requires.
func (m *maskWriter) Write(p []byte) (int, error) {
	buf := append(append([]byte(nil), m.held...), p...)
	cut, out := settle(buf, m.secret)
	if len(out) > 0 {
		if _, err := m.w.Write(out); err != nil {
			return 0, err
		}
	}
	m.held = append([]byte(nil), buf[cut:]...)
	return len(p), nil
}

// settle returns the masked prefix of buf that no later byte can change, and
// the length of buf it covers. A later byte can only begin an occurrence at or
// after the partial suffix of buf, so a run of occurrences that reaches past
// that point is not settled: the cut moves back to its start and the run is
// held whole, to be masked when a later Write completes it.
func settle(buf, secret []byte) (int, []byte) {
	limit := len(buf) - partialSuffix(buf, secret)
	runs := occurrenceRuns(buf, secret)
	cut := limit
	for _, r := range runs {
		if r[1] > limit {
			cut = min(cut, r[0])
		}
	}
	var out []byte
	done := 0
	for _, r := range runs {
		if r[1] > cut {
			break
		}
		out = append(out, buf[done:r[0]]...)
		out = append(out, maskText...)
		done = r[1]
	}
	out = append(out, buf[done:cut]...)
	return cut, out
}

// occurrenceRuns returns the [start, end) span of each run of occurrences of
// secret in buf, in order. Occurrences that overlap share a run, so the run is
// masked once and no part of it is shown; occurrences that only touch are
// separate runs.
func occurrenceRuns(buf, secret []byte) [][2]int {
	var runs [][2]int
	for i := 0; ; {
		j := bytes.Index(buf[i:], secret)
		if j < 0 {
			return runs
		}
		start := i + j
		end := start + len(secret)
		if n := len(runs); n > 0 && start < runs[n-1][1] {
			runs[n-1][1] = end
		} else {
			runs = append(runs, [2]int{start, end})
		}
		i = start + 1
	}
}

// partialSuffix returns the length of the longest suffix of buf that is a
// proper prefix of secret: the bytes that could still grow into an occurrence.
func partialSuffix(buf, secret []byte) int {
	for n := min(len(buf), len(secret)-1); n > 0; n-- {
		if bytes.Equal(buf[len(buf)-n:], secret[:n]) {
			return n
		}
	}
	return 0
}
