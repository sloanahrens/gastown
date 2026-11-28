package gocacheprog

import (
	"io"
	"sync"
)

// responder serializes the responses parallel callers answer with and adds the
// counts each one reports.
type responder struct {
	mu    sync.Mutex // guards the write to out, total and err
	out   io.Writer
	total Stats
	err   error // the first write error; once set, out is left alone (gt-sa1msh)
}

// write adds counts and writes resp as one line, so parallel callers never
// interleave. A write error sticks, so no line follows a truncated one, and
// later calls add their counts and return that first error without writing.
func (r *responder) write(resp *Response, counts Stats) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	addStats(&r.total, counts)
	if r.err != nil {
		return r.err
	}
	if err := respond(r.out, resp); err != nil {
		r.err = err
		return err
	}
	return nil
}

// read returns the counts the writes have added so far and the first write
// error, nil if none failed.
func (r *responder) read() (Stats, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total, r.err
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
