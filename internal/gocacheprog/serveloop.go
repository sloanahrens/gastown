package gocacheprog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// maxInFlight is the most requests Serve answers at once. It is 1, so requests
// are still answered one at a time.
const maxInFlight = 1

// Serve answers the go command's requests from store until a close request or
// the end of in, and reports what it counted. It writes every response through
// one responder, so the counts are the responder's, and an exit that would
// return nil returns the responder's first write error instead.
func Serve(ctx context.Context, in io.Reader, out io.Writer, store Store) (Stats, error) {
	r := &responder{out: out}
	var wg sync.WaitGroup
	// finish waits for every answer in flight, so no goroutine outlives Serve,
	// and only then asks the responder for its counts and first write error.
	finish := func(err error) (Stats, error) {
		wg.Wait()
		total, werr := r.read()
		if err == nil {
			err = werr
		}
		return total, err
	}
	if err := r.write(&Response{ID: 0, KnownCommands: []Cmd{CmdGet, CmdPut, CmdClose}}, Stats{}); err != nil {
		return finish(err)
	}
	rd := NewReader(in)
	sem := make(chan struct{}, maxInFlight)
	for {
		if err := ctx.Err(); err != nil {
			return finish(err)
		}
		sem <- struct{}{}
		if _, werr := r.read(); werr != nil {
			// A write error ends Serve and starts no further request.
			<-sem
			return finish(werr)
		}
		req, err := rd.Next()
		if err != nil {
			<-sem
			if errors.Is(err, io.EOF) {
				return finish(nil)
			}
			return finish(err)
		}
		if req.Command == CmdClose {
			<-sem
			wg.Wait() // the store's Close runs after every earlier answer
			var counts Stats
			resp, done := answer(req, store, &counts)
			if err := r.write(resp, counts); err != nil {
				return finish(err)
			}
			if done {
				return finish(nil)
			}
			continue // the answer did not end Serve: keep reading
		}
		wg.Add(1)
		go func(req *Request) {
			defer wg.Done()
			defer func() { <-sem }()
			var counts Stats
			resp, _ := answer(req, store, &counts)
			if err := r.write(resp, counts); err != nil {
				// The responder keeps the first write error for Serve to return.
				return
			}
		}(req)
	}
}

// respond writes one response as one JSON line.
func respond(w io.Writer, resp *Response) error {
	return json.NewEncoder(w).Encode(resp)
}
