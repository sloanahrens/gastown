package daemon

// limitedBuffer is an io.Writer that keeps the first limit bytes written and
// drops the rest. A write past the limit still reports its full length and no
// error, so the child process writing to it never sees a short write, and the
// memory held never grows past limit bytes.
type limitedBuffer struct {
	limit     int
	buf       []byte
	truncated bool
}

// Write keeps what fits under the limit and notes when it drops the rest.
func (b *limitedBuffer) Write(p []byte) (int, error) {
	full := len(p)
	room := b.limit - len(b.buf)
	if room < 0 {
		room = 0
	}
	if len(p) > room {
		b.truncated = true
		p = p[:room]
	}
	b.buf = append(b.buf, p...)
	return full, nil
}

// Bytes returns the kept bytes.
func (b *limitedBuffer) Bytes() []byte { return b.buf }

// Truncated reports whether any byte was dropped.
func (b *limitedBuffer) Truncated() bool { return b.truncated }
