package gocacheprog

import (
	"io"
	"sync"
)

// responder serializes the responses parallel callers answer with and adds the
// counts each one reports.
type responder struct {
	mu    sync.Mutex // guards the write to out and total
	out   io.Writer
	total Stats
}

// write adds counts and writes resp as one line, so parallel callers never
// interleave, and returns the write's error.
func (r *responder) write(resp *Response, counts Stats) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	addStats(&r.total, counts)
	return respond(r.out, resp)
}

// read returns the counts the writes have added so far.
func (r *responder) read() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}

// addStats adds src's counts to dst.
func addStats(dst *Stats, src Stats) {
	dst.Gets += src.Gets
	dst.Hits += src.Hits
	dst.Misses += src.Misses
	dst.Puts += src.Puts
	dst.PutBytes += src.PutBytes
	dst.HitBytes += src.HitBytes
	dst.Errors += src.Errors
}
