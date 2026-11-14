package bdgateway

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// ReadRequest reads one request head and returns the reader over the caller's
// stdin bytes that follow it. The returned reader is the same stream, so a head
// is never read past what it must: bytes buffered beside the newline are still in
// the reader the caller gets. Only the head's shape is checked here; whether argv
// is a verb the class may run, and whether rig and cwd name a directory the
// gateway has, is the policy's and the executor's call.
func ReadRequest(r io.Reader) (Request, io.Reader, error) {
	// The buffer holds one byte more than the cap so a head of exactly
	// MaxHeadBytes with its newline is read rather than refused as full.
	br := bufio.NewReaderSize(r, MaxHeadBytes+1)
	line, err := br.ReadSlice('\n')
	switch {
	case errors.Is(err, bufio.ErrBufferFull):
		return Request{}, nil, fmt.Errorf("%w: over %d bytes", ErrHeadTooLarge, MaxHeadBytes)
	case errors.Is(err, io.EOF):
		if len(line) == 0 {
			return Request{}, nil, ErrEmptyHead
		}
		return Request{}, nil, fmt.Errorf("%w: %d bytes", ErrTruncatedHead, len(line))
	case err != nil:
		return Request{}, nil, fmt.Errorf("bdgateway: read request head: %w", err)
	}
	var req Request
	if err := json.Unmarshal(line[:len(line)-1], &req); err != nil {
		return Request{}, nil, fmt.Errorf("%w: %v", ErrBadHead, err)
	}
	if len(req.Argv) == 0 {
		return Request{}, nil, fmt.Errorf("%w: no argv", ErrBadHead)
	}
	return req, br, nil
}

// ReadFrame reads one response frame. At a clean end of stream it returns io.EOF;
// any other failure is one of the named refusals, never a partial frame.
func ReadFrame(r io.Reader) (Frame, error) {
	var head [1 + 4]byte
	if _, err := io.ReadFull(r, head[:1]); err != nil {
		if errors.Is(err, io.EOF) {
			return Frame{}, io.EOF
		}
		return Frame{}, fmt.Errorf("bdgateway: read frame kind: %w", err)
	}
	if err := readFull(r, head[1:]); err != nil {
		return Frame{}, err
	}
	length := binary.BigEndian.Uint32(head[1:])
	// The cap is checked before the allocation, so a forged length costs nothing.
	if length > MaxFramePayload {
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, length)
	}
	payload := make([]byte, length)
	if err := readFull(r, payload); err != nil {
		return Frame{}, err
	}
	f := Frame{Kind: FrameKind(head[0]), Payload: payload}
	if err := validate(f); err != nil {
		return Frame{}, err
	}
	return f, nil
}

// readFull fills buf from r. A stream that ends first is a frame cut short; any
// other failure is returned as itself, so a caller can tell a truncated frame from
// a broken connection.
func readFull(r io.Reader, buf []byte) error {
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%w: %v", ErrShortFrame, err)
		}
		return fmt.Errorf("bdgateway: read frame: %w", err)
	}
	return nil
}

// WriteFrame writes one frame, refusing a frame its own reader would reject so a
// malformed frame never reaches the stream. The header and payload go out in one
// Write call. That is not a guarantee against concurrent writers: io.Writer makes
// no atomicity promise, so a caller sharing w must serialize its WriteFrame calls.
func WriteFrame(w io.Writer, f Frame) error {
	if err := validate(f); err != nil {
		return err
	}
	buf := make([]byte, 1+4+len(f.Payload))
	buf[0] = byte(f.Kind)
	binary.BigEndian.PutUint32(buf[1:], uint32(len(f.Payload)))
	copy(buf[1+4:], f.Payload)
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("bdgateway: write frame: %w", err)
	}
	return nil
}

// ExitFrame returns the frame that ends a stream with status, whose payload is
// the status in ASCII digits. A status outside 0 to 255 is not one a process
// exits with, so WriteFrame refuses it.
func ExitFrame(status int) Frame {
	return Frame{Kind: FrameExit, Payload: []byte(strconv.Itoa(status))}
}

// ExitStatus returns the status a FrameExit carries, refusing any other kind so a
// caller cannot read a stdout frame as an exit.
func (f Frame) ExitStatus() (int, error) {
	if f.Kind != FrameExit {
		return 0, fmt.Errorf("%w: frame kind %d", ErrBadExitStatus, f.Kind)
	}
	return exitStatus(f.Payload)
}

// GatewayError returns the refusal a FrameError carries, refusing any other kind.
func (f Frame) GatewayError() (ErrorPayload, error) {
	if f.Kind != FrameError {
		return ErrorPayload{}, fmt.Errorf("%w: frame kind %d", ErrBadErrorPayload, f.Kind)
	}
	return decodeErrorPayload(f.Payload)
}

// validate says whether one frame is well formed: a known kind, a payload within
// the cap, and for the two kinds whose payload has a structure, that structure.
// The reader and the writer share it so a frame one accepts is a frame the other
// accepts.
func validate(f Frame) error {
	if len(f.Payload) > MaxFramePayload {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(f.Payload))
	}
	switch f.Kind {
	case FrameStdout, FrameStderr:
		// Any bytes, including none: output is opaque.
	case FrameExit:
		if _, err := exitStatus(f.Payload); err != nil {
			return err
		}
	case FrameError:
		if _, err := decodeErrorPayload(f.Payload); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: %d", ErrUnknownFrameKind, byte(f.Kind))
	}
	return nil
}

// exitStatus parses an exit frame's payload: ASCII digits naming a status a
// process can exit with. Leading zeros are accepted and add nothing to the value:
// "007" is 7. The 255 limit applies to the value, not the digit count, so "0255"
// is 255 and "256" is refused. The running total is bounded as it grows, so a
// long payload cannot overflow it.
func exitStatus(payload []byte) (int, error) {
	if len(payload) == 0 {
		return 0, fmt.Errorf("%w: empty payload", ErrBadExitStatus)
	}
	status := 0
	for _, c := range payload {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: not ASCII digits", ErrBadExitStatus)
		}
		status = status*10 + int(c-'0')
		if status > 255 {
			return 0, fmt.Errorf("%w: over 255", ErrBadExitStatus)
		}
	}
	return status, nil
}

// decodeErrorPayload parses an error frame's payload. A code is required because
// a refusal with no code gives the client nothing to act on.
func decodeErrorPayload(payload []byte) (ErrorPayload, error) {
	var p ErrorPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return ErrorPayload{}, fmt.Errorf("%w: %v", ErrBadErrorPayload, err)
	}
	if p.Code == "" {
		return ErrorPayload{}, fmt.Errorf("%w: empty code", ErrBadErrorPayload)
	}
	return p, nil
}
