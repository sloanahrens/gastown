// Package bdgateway is the wire format between a caller and the bd gateway: the
// head that names one bd call and the frames that carry its output and exit
// status back. The design is in docs/design/bd-container.md ("The gateway
// protocol"); the HTTP transport, policy and executor are later tasks (G2-G6),
// and this package is only the types and the codec (gt-k3gyqp).
package bdgateway

import "errors"

// Request is the head of one gateway request: one JSON line, then the caller's
// stdin bytes. Field names are the protocol's, so a caller outside Go can write
// the head by hand.
type Request struct {
	// Argv is bd's arguments, never "bd" itself and never a shell string.
	Argv []string `json:"argv"`
	// Env is the caller's environment. The gateway refuses a key outside its
	// allowlist rather than dropping it.
	Env map[string]string `json:"env"`
	// Rig and Cwd are relative to the town root, never absolute paths.
	Rig string `json:"rig"`
	Cwd string `json:"cwd"`
	// TimeoutMS is the caller's requested limit; the gateway caps it by class.
	TimeoutMS int `json:"timeout_ms"`
}

// FrameKind is the kind byte that starts every response frame.
type FrameKind byte

const (
	// FrameStdout carries bytes bd wrote to stdout.
	FrameStdout FrameKind = 1
	// FrameStderr carries bytes bd wrote to stderr.
	FrameStderr FrameKind = 2
	// FrameExit carries the exit status as ASCII digits and ends the stream.
	FrameExit FrameKind = 3
	// FrameError carries a gateway refusal; an exit frame follows it.
	FrameError FrameKind = 4
)

// Frame is one response frame: a kind byte, a four-byte big-endian payload
// length, then the payload.
type Frame struct {
	Kind    FrameKind
	Payload []byte
}

// ErrorPayload is the payload of a FrameError: the gateway's refusal code and
// the message shown to the caller.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

const (
	// MaxHeadBytes bounds the head line. The design caps stdin and gives the head
	// no cap; the codec sets one so a caller cannot make the gateway buffer
	// without bound. A head is an argv plus a few short fields, far under this.
	MaxHeadBytes = 64 << 10
	// MaxFramePayload bounds one frame's payload. Output streams in chunks, so a
	// frame at this size is a client with a large buffer, not a protocol need.
	MaxFramePayload = 1 << 20
)

// The codec's named refusals. ReadRequest, ReadFrame and WriteFrame wrap one of
// these with the size or offset they refused at, so a caller matches on the
// error rather than on message text.
var (
	// ErrEmptyHead is input that ended before any head byte.
	ErrEmptyHead = errors.New("bdgateway: empty request head")
	// ErrTruncatedHead is a head line with no newline before the end of input.
	ErrTruncatedHead = errors.New("bdgateway: request head has no newline")
	// ErrHeadTooLarge is a head over MaxHeadBytes.
	ErrHeadTooLarge = errors.New("bdgateway: request head over the size cap")
	// ErrBadHead is a head that is not a JSON object or names no argv.
	ErrBadHead = errors.New("bdgateway: malformed request head")
	// ErrUnknownFrameKind is a kind byte outside stdout, stderr, exit and error.
	ErrUnknownFrameKind = errors.New("bdgateway: unknown frame kind")
	// ErrFrameTooLarge is a frame payload over MaxFramePayload.
	ErrFrameTooLarge = errors.New("bdgateway: frame payload over the size cap")
	// ErrShortFrame is a frame whose length claims more bytes than follow.
	ErrShortFrame = errors.New("bdgateway: frame cut short")
	// ErrBadExitStatus is an exit frame whose payload is not ASCII digits.
	ErrBadExitStatus = errors.New("bdgateway: exit frame payload is not a status")
	// ErrBadErrorPayload is an error frame whose payload is not JSON with a code
	// and a message.
	ErrBadErrorPayload = errors.New("bdgateway: error frame payload is not JSON with a code and a message")
)
