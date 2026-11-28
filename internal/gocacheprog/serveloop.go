package gocacheprog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
)

// Serve answers the go command's requests from store until a close request or
// the end of in, and reports what it counted.
func Serve(ctx context.Context, in io.Reader, out io.Writer, store Store) (Stats, error) {
	var stats Stats
	if err := respond(out, &Response{ID: 0, KnownCommands: []Cmd{CmdGet, CmdPut, CmdClose}}); err != nil {
		return stats, err
	}
	r := NewReader(in)
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		req, err := r.Next()
		if errors.Is(err, io.EOF) {
			return stats, nil
		}
		if err != nil {
			return stats, err
		}
		resp, done := answer(req, store, &stats)
		if err := respond(out, resp); err != nil {
			return stats, err
		}
		if done {
			return stats, nil
		}
	}
}

// respond writes one response as one JSON line.
func respond(w io.Writer, resp *Response) error {
	return json.NewEncoder(w).Encode(resp)
}
