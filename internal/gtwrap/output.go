package gtwrap

import (
	"bytes"
	"io"
)

// outputRewriter rewrites the pod town root of a tool's output to the Mac's town
// root, so a path the tool prints in the pod can be pasted on the Mac. It is
// line-oriented: a complete line is rewritten and written at once, so a stream such
// as `gt attention -f` keeps flowing. The rewrite is one substitution of the root as
// a whole path segment, so a path that merely starts with the same characters (the
// pod root /town and the directory /township) is left alone.
//
// In prompt mode an unterminated line is written as far as a later byte cannot
// change it, so a prompt the tool prints without a newline reaches the terminal
// before the tool waits for input. Only the end of the line that could begin or
// complete the pod root is held back. Prompt mode is for a terminal, which carries
// no binary stream: bytes it has already written cannot be withdrawn if a NUL byte
// arrives later, which is why the default holds a line until it ends.
//
// A stream that carries a NUL byte is binary: the pending line is written unchanged
// and everything after it passes through untouched, so an archive or an image is
// never rewritten.
type outputRewriter struct {
	// dst receives the rewritten stream.
	dst io.Writer
	// from is the pod's town root, and to is the Mac's.
	from, to []byte
	// line holds the bytes of the current line after its last written byte, which
	// are the only bytes not yet written.
	line []byte
	// prompt writes an unterminated line as far as a later byte cannot change it.
	prompt bool
	// prev is the last byte written, and hasPrev says there is one: together they
	// are the left context of the pending line when prompt mode writes part of it.
	prev    byte
	hasPrev bool
	// binary says a NUL byte was seen, so the rest of the stream is passed through.
	binary bool
}

// newOutputRewriter returns a rewriter that copies a tool's output to dst,
// substituting the pod town root with the Mac town root. prompt writes an
// unterminated line as far as a later byte cannot change it, so an interactive
// prompt shows before the tool waits; it is for a stream that is a terminal. The
// caller passes two roots that differ; equal roots need no rewriter, and a caller
// that builds one anyway gets a pass-through, since the substitution would replace
// a string with itself.
func newOutputRewriter(dst io.Writer, podRoot, town string, prompt bool) *outputRewriter {
	return &outputRewriter{dst: dst, from: []byte(podRoot), to: []byte(town), prompt: prompt}
}

// Write takes the next bytes of the tool's output, writes every line it completes,
// and in prompt mode writes the part of an unterminated line a later byte cannot
// change. It returns the number of bytes it took: a byte it holds counts as taken,
// since the rewriter owns it until it is written.
func (r *outputRewriter) Write(p []byte) (int, error) {
	if r.binary {
		return r.dst.Write(p)
	}
	if bytes.IndexByte(p, 0) >= 0 {
		// The stream is binary, so nothing in it may be rewritten. What is pending
		// was never rewritten, since a byte is rewritten only once it is written, so
		// it is written as it came.
		if err := r.writePending(); err != nil {
			return 0, err
		}
		r.binary = true
		return r.dst.Write(p)
	}
	r.line = append(r.line, p...)
	for {
		end := bytes.IndexByte(r.line, '\n')
		if end < 0 {
			break
		}
		line := r.line[:end+1]
		if err := r.writeLine(line); err != nil {
			return 0, err
		}
		r.line = r.line[end+1:]
	}
	if r.prompt {
		if err := r.writePartial(); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// writeLine writes one complete line, rewritten.
func (r *outputRewriter) writeLine(line []byte) error {
	return r.emit(r.rewrite(line, r.left(), 0, false, true), line[len(line)-1])
}

// writePartial writes the pending line up to the point a later byte could still
// change what is written, and keeps the rest. Only the end of the line that could
// begin or complete the pod root is held, so a prompt reaches the terminal while the
// tool waits for input.
func (r *outputRewriter) writePartial() error {
	if len(r.line) == 0 {
		return nil
	}
	limit := r.partialLimit(r.line)
	if limit == 0 {
		return nil
	}
	next, hasNext := byte(0), limit < len(r.line)
	if hasNext {
		next = r.line[limit]
	}
	out := r.rewrite(r.line[:limit], r.left(), next, hasNext, false)
	if err := r.emit(out, r.line[limit-1]); err != nil {
		return err
	}
	r.line = r.line[limit:]
	return nil
}

// partialLimit returns the index where the part of line a later write cannot change
// ends: the start of a suffix that could still grow into the pod root, or of an
// occurrence whose right boundary the next byte decides, so the occurrence is held
// from its own start. The bytes before it can be written now.
func (r *outputRewriter) partialLimit(line []byte) int {
	limit := r.partialStart(line)
	// An occurrence that ends the buffer needs the byte after it to decide whether
	// it ends a token, so it is held from its start.
	if at := len(line) - len(r.from); at >= 0 && bytes.Equal(line[at:], r.from) && r.leftOK(line, at, r.left()) {
		limit = min(limit, at)
	}
	// A root that repeats its own prefix can begin before the held suffix and end
	// inside it. A match is at most len(from) long, so every occurrence that
	// overlaps limit starts within the last len(from) bytes before it.
	for i := limit - 1; i >= 0 && i > limit-len(r.from); i-- {
		if i+len(r.from) <= len(line) && bytes.Equal(line[i:i+len(r.from)], r.from) && r.leftOK(line, i, r.left()) {
			limit = i
		}
	}
	return limit
}

// partialStart returns the index where a suffix of line that could still grow into
// the pod root begins, or len(line) when no suffix could.
func (r *outputRewriter) partialStart(line []byte) int {
	longest := min(len(r.from)-1, len(line))
	for n := longest; n > 0; n-- {
		if bytes.Equal(line[len(line)-n:], r.from[:n]) {
			return len(line) - n
		}
	}
	return len(line)
}

// left reports whether the byte before the pending line ends a path segment: the
// start of the stream or of a line, or a byte that is not part of a path.
func (r *outputRewriter) left() bool {
	return !r.hasPrev || !pathByte(r.prev)
}

// emit writes out and records the last input byte it covers, which is the left
// context of the bytes still pending.
func (r *outputRewriter) emit(out []byte, last byte) error {
	if len(out) > 0 {
		if _, err := r.dst.Write(out); err != nil {
			return err
		}
	}
	r.prev, r.hasPrev = last, true
	return nil
}

// Flush writes the line the stream ended on when it has no newline of its own. A
// stream that ends after a newline has nothing pending, so Flush writes nothing.
func (r *outputRewriter) Flush() error {
	if r.binary {
		return r.writePending()
	}
	if len(r.line) == 0 {
		return nil
	}
	line := r.line
	r.line = nil
	return r.emit(r.rewrite(line, r.left(), 0, false, true), line[len(line)-1])
}

// writePending writes the buffered line unchanged and empties the buffer.
func (r *outputRewriter) writePending() error {
	if len(r.line) == 0 {
		return nil
	}
	line := r.line
	r.line = nil
	_, err := r.dst.Write(line)
	return err
}

// rewrite returns line with each occurrence of the pod town root that names the
// root as a whole path segment replaced by the Mac town root. The result is line
// itself when nothing matches. Every occurrence in line is decided: the caller
// holds back an occurrence whose right boundary a later byte would decide.
//
// left says whether the byte before line ends a path segment. The byte after line
// is next when hasNext, and the end of the line is a token boundary when end.
func (r *outputRewriter) rewrite(line []byte, left bool, next byte, hasNext, end bool) []byte {
	if len(r.from) == 0 {
		return line
	}
	var out []byte
	last := 0
	for i := 0; i+len(r.from) <= len(line); {
		k := bytes.Index(line[i:], r.from)
		if k < 0 {
			break
		}
		at := i + k
		e := at + len(r.from)
		if !r.leftOK(line, at, left) || !r.rightOK(line, e, next, hasNext, end) {
			i = at + 1
			continue
		}
		if out == nil {
			out = make([]byte, 0, len(line)+len(r.to))
		}
		out = append(out, line[last:at]...)
		out = append(out, r.to...)
		last = e
		i = e
	}
	if out == nil {
		return line
	}
	return append(out, line[last:]...)
}

// leftOK reports whether the pod town root at at in line names the root as a whole
// path segment at its left: the byte before it does not continue a path (so
// /srv/town is not the root /town). left is the context when at is the first byte.
func (r *outputRewriter) leftOK(line []byte, at int, left bool) bool {
	if at == 0 {
		return left
	}
	return !pathByte(line[at-1])
}

// rightOK reports whether the pod town root ending at e in line names the root as a
// whole path segment at its right: the byte after it is a path separator or a byte
// that ends the token (so /town/rig and a bare /town do match, and /township does
// not). The byte after line is next when hasNext, and the end of the line is a
// token boundary when end.
func (r *outputRewriter) rightOK(line []byte, e int, next byte, hasNext, end bool) bool {
	switch {
	case e < len(line):
		return line[e] == '/' || !pathByte(line[e])
	case hasNext:
		return next == '/' || !pathByte(next)
	default:
		return end
	}
}

// pathByte reports whether b can be part of a path segment: the letters and digits
// of a name, and the punctuation a file name carries. A byte that is not one ends
// the token: a space, a quote, a colon or a bracket.
func pathByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '/', b == '.', b == '-', b == '_', b == '~', b == '+', b == '@':
		return true
	}
	return false
}
