// Package gocacheprog reads the request stream the go command writes to a
// GOCACHEPROG program. Field names and JSON tags copy cmd/go/internal/cacheprog,
// which an external program cannot import.
package gocacheprog

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Cmd is a request command.
type Cmd string

const (
	CmdGet   = Cmd("get")
	CmdPut   = Cmd("put")
	CmdClose = Cmd("close")
)

// MaxBodySize is the largest body a put request may carry: 1 GiB.
const MaxBodySize int64 = 1 << 30

// Request is one request from the go command; Body holds its decoded bytes.
type Request struct {
	ID       int64
	Command  Cmd
	ActionID []byte `json:",omitempty"`
	OutputID []byte `json:",omitempty"`
	Body     []byte `json:"-"`
	BodySize int64  `json:",omitempty"`
}

// Response is one response to the go command.
type Response struct {
	ID            int64
	Err           string     `json:",omitempty"`
	KnownCommands []Cmd      `json:",omitempty"`
	Miss          bool       `json:",omitempty"`
	OutputID      []byte     `json:",omitempty"`
	Size          int64      `json:",omitempty"`
	Time          *time.Time `json:",omitempty"`
	DiskPath      string     `json:",omitempty"`
}

// Reader reads requests from a stream.
type Reader struct{ br *bufio.Reader }

// NewReader returns a Reader over r.
func NewReader(r io.Reader) *Reader { return &Reader{bufio.NewReader(r)} }

// Next returns the next request with its body; io.EOF marks only a clean end
// between requests.
func (r *Reader) Next() (*Request, error) {
	line, err := r.br.ReadBytes('\n')
	if err != nil {
		if err == io.EOF && len(line) == 0 {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("gocacheprog: cut request: %w", cut(err))
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return nil, fmt.Errorf("gocacheprog: malformed request: %w", err)
	}
	switch {
	case req.BodySize < 0:
		return nil, fmt.Errorf("gocacheprog: request %d: negative body size %d", req.ID, req.BodySize)
	case req.BodySize > MaxBodySize:
		return nil, fmt.Errorf("gocacheprog: request %d: body size %d over limit %d", req.ID, req.BodySize, MaxBodySize)
	case req.BodySize == 0:
		return &req, nil
	}
	line, err = r.br.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("gocacheprog: request %d: body: %w", req.ID, cut(err))
	}
	var s string
	if err := json.Unmarshal(line, &s); err != nil {
		return nil, fmt.Errorf("gocacheprog: request %d: body is not a JSON string: %w", req.ID, err)
	}
	body, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("gocacheprog: request %d: body is not base64: %w", req.ID, err)
	}
	if int64(len(body)) != req.BodySize {
		return nil, fmt.Errorf("gocacheprog: request %d: body is %d bytes, want %d", req.ID, len(body), req.BodySize)
	}
	req.Body = body
	return &req, nil
}

// cut maps a clean end of stream to io.ErrUnexpectedEOF, so a truncated body
// never reads as io.EOF.
func cut(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
