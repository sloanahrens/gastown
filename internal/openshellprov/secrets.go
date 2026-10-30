package openshellprov

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
)

// maskText replaces every occurrence of the secret in the output.
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
		value = unquote(assignment)
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
// character key is hidden as surely as a long one. An occurrence split across
// two Writes is still replaced: the bytes that could begin an occurrence are
// held back until the next Write settles them. Output that ends part-way into
// the secret is therefore not written, which costs the tail of the stream but
// never shows the secret. An empty secret masks nothing, so Masked returns w
// unchanged; the caller refuses an empty secret before it gets here.
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
	// held is the tail of the last Write that is a proper prefix of the secret.
	held []byte
}

// Write masks p and writes what is settled to the underlying writer. It always
// reports all of p as consumed, because the held tail is written later.
func (m *maskWriter) Write(p []byte) (int, error) {
	buf := append(m.held, p...)
	var out []byte
	// Leftmost first: once no full occurrence remains, a later occurrence can
	// only start inside the held tail, which is kept below.
	for {
		i := bytes.Index(buf, m.secret)
		if i < 0 {
			break
		}
		out = append(out, buf[:i]...)
		out = append(out, maskText...)
		buf = buf[i+len(m.secret):]
	}
	keep := partialSuffix(buf, m.secret)
	out = append(out, buf[:len(buf)-keep]...)
	m.held = append([]byte(nil), buf[len(buf)-keep:]...)
	if len(out) == 0 {
		return len(p), nil
	}
	if _, err := m.w.Write(out); err != nil {
		return len(p), err
	}
	return len(p), nil
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
