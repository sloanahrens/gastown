package main

import (
	"bytes"
	"io"
)

// lineFeeder is an io.Writer that feeds each complete line to feed, without
// its terminator. A line may arrive split across writes, so the partial line
// is held until its newline; Flush feeds a final unterminated line.
type lineFeeder struct {
	feed    func(line string)
	partial []byte
}

var _ io.Writer = (*lineFeeder)(nil)

// Write feeds every complete line in p, plus any partial line Write held
// from earlier chunks, and keeps the rest as the new partial line.
func (l *lineFeeder) Write(p []byte) (int, error) {
	l.partial = append(l.partial, p...)
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			break
		}
		l.feed(lineText(l.partial[:i]))
		l.partial = l.partial[i+1:]
	}
	return len(p), nil
}

// Flush feeds the held partial line if any, then clears it.
func (l *lineFeeder) Flush() {
	if len(l.partial) > 0 {
		l.feed(lineText(l.partial))
	}
	l.partial = nil
}

// lineText returns b as a string without one trailing carriage return.
func lineText(b []byte) string {
	return string(bytes.TrimSuffix(b, []byte("\r")))
}
