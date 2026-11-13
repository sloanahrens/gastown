package gtwrap

import (
	"bytes"
	"io"
)

// outputRewriter rewrites the pod town root of a tool's output to the Mac's town
// root, so a path the tool prints in the pod can be pasted on the Mac. It is
// line-oriented: a complete line is rewritten and written at once, so a stream such
// as `gt attention -f` keeps flowing, and only the bytes of an unterminated line are
// held back. The rewrite is one substitution of the root as a whole path segment, so
// a path that merely starts with the same characters (the pod root /town and the
// directory /township) is left alone.
//
// A stream that carries a NUL byte is binary: the pending line is written unchanged
// and everything after it passes through untouched, so an archive or an image is
// never rewritten. Bytes that hold no root pass through as they arrive.
type outputRewriter struct {
	// dst receives the rewritten stream.
	dst io.Writer
	// from is the pod's town root, and to is the Mac's.
	from, to []byte
	// line holds the bytes of the current line after its last newline, which are
	// the only bytes not yet written.
	line []byte
	// binary says a NUL byte was seen, so the rest of the stream is passed through.
	binary bool
}

// newOutputRewriter returns a rewriter that copies a tool's output to dst,
// substituting the pod town root with the Mac town root. The caller passes two
// roots that differ; equal roots need no rewriter, and a caller that builds one
// anyway gets a pass-through, since the substitution would replace a string with
// itself.
func newOutputRewriter(dst io.Writer, podRoot, town string) *outputRewriter {
	return &outputRewriter{dst: dst, from: []byte(podRoot), to: []byte(town)}
}

// Write takes the next bytes of the tool's output, writes every line it completes,
// and returns the number of bytes it took. A byte it buffers counts as taken: the
// rewriter owns it until the line ends or Flush writes it.
func (r *outputRewriter) Write(p []byte) (int, error) {
	if r.binary {
		return r.dst.Write(p)
	}
	if bytes.IndexByte(p, 0) >= 0 {
		// The stream is binary, so nothing in it may be rewritten. What is pending
		// was never rewritten, since a line is rewritten only when it ends, so it is
		// written as it came.
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
			return len(p), nil
		}
		line := r.line[:end+1]
		r.line = r.line[end+1:]
		if _, err := r.dst.Write(r.rewrite(line)); err != nil {
			return 0, err
		}
	}
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
	_, err := r.dst.Write(r.rewrite(line))
	return err
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
// itself when nothing matches.
func (r *outputRewriter) rewrite(line []byte) []byte {
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
		if r.rootAt(line, at) {
			if out == nil {
				out = make([]byte, 0, len(line)+len(r.to))
			}
			out = append(out, line[last:at]...)
			out = append(out, r.to...)
			last = at + len(r.from)
			i = last
			continue
		}
		i = at + 1
	}
	if out == nil {
		return line
	}
	return append(out, line[last:]...)
}

// rootAt reports whether the pod town root at at in line names the root as a whole
// path segment: the byte before it does not continue a path (so /srv/town is not
// the root /town), and the byte after it is a path separator, the end of the line,
// or a byte that ends the token (so /town/rig and a bare /town do match, and
// /township does not).
func (r *outputRewriter) rootAt(line []byte, at int) bool {
	if at > 0 && pathByte(line[at-1]) {
		return false
	}
	end := at + len(r.from)
	if end == len(line) {
		return true
	}
	return line[end] == '/' || !pathByte(line[end])
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
