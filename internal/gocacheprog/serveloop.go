package gocacheprog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
)

// Serve answers the go command's requests from store until a close request or
// the end of in, and reports what it counted. It writes every response through
// one responder, so the counts are the responder's, and an exit that would
// return nil returns the responder's first write error instead.
func Serve(ctx context.Context, in io.Reader, out io.Writer, store Store) (Stats, error) {
	r := &responder{out: out}
	finish := func(err error) (Stats, error) {
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
	for {
		if err := ctx.Err(); err != nil {
			return finish(err)
		}
		req, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return finish(nil)
		}
		if err != nil {
			return finish(err)
		}
		var counts Stats
		resp, done := answer(req, store, &counts)
		if err := r.write(resp, counts); err != nil {
			return finish(err)
		}
		if done {
			return finish(nil)
		}
	}
}

// respond writes one response as one JSON line.
func respond(w io.Writer, resp *Response) error {
	return json.NewEncoder(w).Encode(resp)
}
