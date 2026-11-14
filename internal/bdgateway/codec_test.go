package bdgateway

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// head is the head bytes a caller writes: the request as JSON, then its newline.
func head(t *testing.T, req Request) []byte {
	t.Helper()
	line, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal head: %v", err)
	}
	return append(line, '\n')
}

// TestReadRequest: the head line the design shows parses into its fields, and the
// stdin bytes after the newline come back byte for byte, including a newline and
// a byte that is not UTF-8.
func TestReadRequest(t *testing.T) {
	t.Parallel()
	req := Request{
		Argv:      []string{"show", "gt-abc", "--json"},
		Env:       map[string]string{"BD_ACTOR": "town/crew/dipper", "BD_JSON_ENVELOPE": "1"},
		Rig:       "gastown",
		Cwd:       "gastown/crew/dipper",
		TimeoutMS: 30000,
	}
	stdin := []byte("bead body\nline two\xff\x00end")

	got, rest, err := ReadRequest(bytes.NewReader(append(head(t, req), stdin...)))
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if !slices.Equal(got.Argv, req.Argv) || got.Rig != req.Rig || got.Cwd != req.Cwd || got.TimeoutMS != req.TimeoutMS {
		t.Errorf("head = %+v, want %+v", got, req)
	}
	if len(got.Env) != len(req.Env) || got.Env["BD_ACTOR"] != req.Env["BD_ACTOR"] || got.Env["BD_JSON_ENVELOPE"] != req.Env["BD_JSON_ENVELOPE"] {
		t.Errorf("env = %v, want %v", got.Env, req.Env)
	}
	gotStdin, err := io.ReadAll(rest)
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	if !bytes.Equal(gotStdin, stdin) {
		t.Errorf("stdin = %q, want %q", gotStdin, stdin)
	}
}

// TestReadRequestHeadSize: a head of exactly MaxHeadBytes with its newline is
// read, and one byte more is refused. The boundary is the codec's own choice (the
// design gives the head no cap), so the test pins it.
func TestReadRequestHeadSize(t *testing.T) {
	t.Parallel()
	const prefix = `{"argv":["show"],"rig":"`
	const suffix = `"}`
	exact := prefix + strings.Repeat("a", MaxHeadBytes-len(prefix)-len(suffix)) + suffix
	if len(exact) != MaxHeadBytes {
		t.Fatalf("built head is %d bytes, want %d", len(exact), MaxHeadBytes)
	}
	if _, _, err := ReadRequest(strings.NewReader(exact + "\n")); err != nil {
		t.Errorf("head at the cap: %v, want nil", err)
	}
	if _, _, err := ReadRequest(strings.NewReader(exact + "a\n")); !errors.Is(err, ErrHeadTooLarge) {
		t.Errorf("head one byte over the cap = %v, want %v", err, ErrHeadTooLarge)
	}
}

// TestCodecRoundTrip: every frame kind survives a write and a read with its
// payload intact, the wire bytes are the design's kind byte, big-endian length and
// payload, the exit status reads back as the number, the refusal reads back as its
// code and message, and a consumed stream ends with io.EOF.
func TestCodecRoundTrip(t *testing.T) {
	t.Parallel()
	errorPayload, err := json.Marshal(ErrorPayload{Code: "bad_head", Message: `the head said "x" and no argv`})
	if err != nil {
		t.Fatalf("marshal error payload: %v", err)
	}
	for _, tc := range []struct {
		name string
		f    Frame
	}{
		{"stdout empty", Frame{Kind: FrameStdout}},
		{"stdout binary", Frame{Kind: FrameStdout, Payload: []byte{'h', 'i', '\n', 0x00, 0xff}}},
		{"stdout large", Frame{Kind: FrameStdout, Payload: bytes.Repeat([]byte("x"), 64<<10)}},
		{"stderr", Frame{Kind: FrameStderr, Payload: []byte("bd: no such bead\n")}},
		{"exit 0", ExitFrame(0)},
		{"exit 124", ExitFrame(124)},
		{"exit 255", ExitFrame(255)},
		{"gateway error", Frame{Kind: FrameError, Payload: errorPayload}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := WriteFrame(&buf, tc.f); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
			got, err := ReadFrame(&buf)
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			if got.Kind != tc.f.Kind || !bytes.Equal(got.Payload, tc.f.Payload) {
				t.Errorf("frame = {%d %q}, want {%d %q}", got.Kind, got.Payload, tc.f.Kind, tc.f.Payload)
			}
			if buf.Len() != 0 {
				t.Errorf("%d bytes left after the frame", buf.Len())
			}
			if _, err := ReadFrame(&buf); !errors.Is(err, io.EOF) {
				t.Errorf("end of stream = %v, want io.EOF", err)
			}
		})
	}

	t.Run("exit status", func(t *testing.T) {
		t.Parallel()
		for _, status := range []int{0, 1, 124, 125, 255} {
			if got, err := ExitFrame(status).ExitStatus(); err != nil || got != status {
				t.Errorf("ExitStatus of %d = %d, %v; want %d, nil", status, got, err, status)
			}
		}
		for _, tc := range []struct {
			payload string
			want    int
		}{{"007", 7}, {"0255", 255}} {
			if got, err := (Frame{Kind: FrameExit, Payload: []byte(tc.payload)}).ExitStatus(); err != nil || got != tc.want {
				t.Errorf("ExitStatus of %q = %d, %v; want %d, nil", tc.payload, got, err, tc.want)
			}
		}
		if _, err := (Frame{Kind: FrameStdout, Payload: []byte("0")}).ExitStatus(); !errors.Is(err, ErrBadExitStatus) {
			t.Errorf("ExitStatus of a stdout frame = %v, want %v", err, ErrBadExitStatus)
		}
	})

	t.Run("gateway error", func(t *testing.T) {
		t.Parallel()
		got, err := (Frame{Kind: FrameError, Payload: errorPayload}).GatewayError()
		if err != nil {
			t.Fatalf("GatewayError: %v", err)
		}
		want := ErrorPayload{Code: "bad_head", Message: `the head said "x" and no argv`}
		if got != want {
			t.Errorf("GatewayError = %+v, want %+v", got, want)
		}
		if _, err := (Frame{Kind: FrameStdout}).GatewayError(); !errors.Is(err, ErrBadErrorPayload) {
			t.Errorf("GatewayError of a stdout frame = %v, want %v", err, ErrBadErrorPayload)
		}
	})

	t.Run("wire bytes", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if err := WriteFrame(&buf, Frame{Kind: FrameStdout, Payload: []byte("hi")}); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
		want := []byte{1, 0, 0, 0, 2, 'h', 'i'}
		if !bytes.Equal(buf.Bytes(), want) {
			t.Errorf("wire = %v, want %v", buf.Bytes(), want)
		}
	})
}

// TestCodecMalformed: malformed input of every shape the design names is refused
// with its named error, from the request reader, the frame reader and the frame
// writer, and none of them panics.
func TestCodecMalformed(t *testing.T) {
	t.Parallel()

	t.Run("request", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			in   string
			want error
		}{
			{"empty input", "", ErrEmptyHead},
			{"head with no newline", `{"argv":["show"]}`, ErrTruncatedHead},
			{"oversize head", strings.Repeat("a", MaxHeadBytes+1), ErrHeadTooLarge},
			{"bad JSON", "not a head\n", ErrBadHead},
			{"JSON null", "null\n", ErrBadHead},
			{"JSON array", "[]\n", ErrBadHead},
			{"no argv", `{"rig":"gastown"}` + "\n", ErrBadHead},
			{"empty argv", `{"argv":[]}` + "\n", ErrBadHead},
			{"argv not strings", `{"argv":[1,2]}` + "\n", ErrBadHead},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if _, _, err := ReadRequest(strings.NewReader(tc.in)); !errors.Is(err, tc.want) {
					t.Errorf("ReadRequest(%q) = %v, want %v", tc.in, err, tc.want)
				}
			})
		}
	})

	t.Run("frame", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			in   []byte
			want error
		}{
			{"kind with no length", []byte{1, 0, 0}, ErrShortFrame},
			{"unknown kind", rawFrame(9, nil), ErrUnknownFrameKind},
			{"payload shorter than the length", []byte{1, 0, 0, 0, 5, 'a'}, ErrShortFrame},
			{"length over the cap", rawFrameHeader(1, MaxFramePayload+1), ErrFrameTooLarge},
			{"exit with no payload", rawFrame(3, nil), ErrBadExitStatus},
			{"exit with a letter", rawFrame(3, []byte("12a")), ErrBadExitStatus},
			{"exit with a sign", rawFrame(3, []byte("-1")), ErrBadExitStatus},
			{"exit over 255", rawFrame(3, []byte("256")), ErrBadExitStatus},
			{"exit over 255 with leading zeros", rawFrame(3, []byte("0256")), ErrBadExitStatus},
			{"error payload not JSON", rawFrame(4, []byte("nope")), ErrBadErrorPayload},
			{"error payload not an object", rawFrame(4, []byte("[]")), ErrBadErrorPayload},
			{"error payload with no code", rawFrame(4, []byte(`{"message":"m"}`)), ErrBadErrorPayload},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := ReadFrame(bytes.NewReader(tc.in)); !errors.Is(err, tc.want) {
					t.Errorf("ReadFrame(%v) = %v, want %v", tc.in, err, tc.want)
				}
			})
		}
	})

	t.Run("write", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			f    Frame
		}{
			{"kind zero", Frame{}},
			{"unknown kind", Frame{Kind: 9}},
			{"payload over the cap", Frame{Kind: FrameStdout, Payload: make([]byte, MaxFramePayload+1)}},
			{"exit with no payload", Frame{Kind: FrameExit}},
			{"exit over 255", Frame{Kind: FrameExit, Payload: []byte("256")}},
			{"error payload not JSON", Frame{Kind: FrameError, Payload: []byte("nope")}},
			{"error payload with no code", Frame{Kind: FrameError, Payload: []byte(`{"message":"m"}`)}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var buf bytes.Buffer
				if err := WriteFrame(&buf, tc.f); err == nil {
					t.Errorf("WriteFrame(%+v) = nil, want an error", tc.f)
				}
				if buf.Len() != 0 {
					t.Errorf("a refused frame wrote %d bytes", buf.Len())
				}
			})
		}
	})
}

// TestCodecReadError: a connection that fails mid-frame is reported as itself,
// not as a short frame, so a caller can retry a broken connection and not a
// protocol violation, and a failure at the first byte of a frame or a head
// reaches the caller instead of reading as an end of stream.
func TestCodecReadError(t *testing.T) {
	t.Parallel()
	reset := errors.New("connection reset")

	// The kind byte arrives, then the connection fails reading the length.
	r := io.MultiReader(bytes.NewReader([]byte{1}), errReader{reset})
	if _, err := ReadFrame(r); !errors.Is(err, reset) || errors.Is(err, ErrShortFrame) {
		t.Errorf("ReadFrame over a failing connection = %v, want it to wrap %v and not %v", err, reset, ErrShortFrame)
	}

	// The connection fails before the frame's first byte.
	if _, err := ReadFrame(errReader{reset}); !errors.Is(err, reset) || errors.Is(err, io.EOF) {
		t.Errorf("ReadFrame with no byte read = %v, want it to wrap %v and not %v", err, reset, io.EOF)
	}

	// The connection fails while the head is read.
	if _, _, err := ReadRequest(errReader{reset}); !errors.Is(err, reset) {
		t.Errorf("ReadRequest over a failing connection = %v, want it to wrap %v", err, reset)
	}
}

// TestCodecWriteError: a failing sink is reported to the caller, not dropped.
func TestCodecWriteError(t *testing.T) {
	t.Parallel()
	down := errors.New("connection reset")
	if err := WriteFrame(errWriter{down}, Frame{Kind: FrameStdout, Payload: []byte("x")}); !errors.Is(err, down) {
		t.Errorf("WriteFrame to a failing writer = %v, want it to wrap %v", err, down)
	}
}

// FuzzReadFrame: no input panics the reader, and every frame it accepts is a
// frame the writer accepts and the reader reads back unchanged.
func FuzzReadFrame(f *testing.F) {
	f.Add(rawFrame(1, []byte("hi")))
	f.Add(rawFrame(3, []byte("124")))
	f.Add(rawFrame(4, []byte(`{"code":"c","message":"m"}`)))
	f.Add([]byte{1, 0, 0, 0, 9, 'x'})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		fr, err := ReadFrame(bytes.NewReader(data))
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			for _, want := range []error{ErrUnknownFrameKind, ErrFrameTooLarge, ErrShortFrame, ErrBadExitStatus, ErrBadErrorPayload} {
				if errors.Is(err, want) {
					return
				}
			}
			t.Fatalf("ReadFrame(%v) = %v, which is not io.EOF or a named refusal", data, err)
		}
		var buf bytes.Buffer
		if err := WriteFrame(&buf, fr); err != nil {
			t.Fatalf("ReadFrame(%v) accepted a frame WriteFrame refuses: %v", data, err)
		}
		back, err := ReadFrame(&buf)
		if err != nil || back.Kind != fr.Kind || !bytes.Equal(back.Payload, fr.Payload) {
			t.Fatalf("frame %+v did not survive a write and read: %+v, %v", fr, back, err)
		}
	})
}

// FuzzReadRequest: no input panics the reader, and a head it accepts is one with
// an argv whose stdin starts exactly after the head line.
func FuzzReadRequest(f *testing.F) {
	f.Add([]byte(`{"argv":["show","gt-abc"]}` + "\nbody\n"))
	f.Add([]byte(""))
	f.Add([]byte("null\n"))
	f.Add([]byte("{\"argv\":[]}\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		req, stdin, err := ReadRequest(bytes.NewReader(data))
		if err != nil {
			for _, want := range []error{ErrEmptyHead, ErrTruncatedHead, ErrHeadTooLarge, ErrBadHead} {
				if errors.Is(err, want) {
					return
				}
			}
			t.Fatalf("ReadRequest(%q) = %v, which is not a named refusal", data, err)
		}
		if len(req.Argv) == 0 {
			t.Fatalf("ReadRequest(%q) accepted a head with no argv", data)
		}
		got, err := io.ReadAll(stdin)
		if err != nil {
			t.Fatalf("read stdin: %v", err)
		}
		want := data[bytes.IndexByte(data, '\n')+1:]
		if !bytes.Equal(got, want) {
			t.Fatalf("stdin = %q, want %q", got, want)
		}
	})
}

// rawFrame is a frame as the wire bytes, built here because WriteFrame refuses
// the malformed ones these tests feed the reader.
func rawFrame(kind byte, payload []byte) []byte {
	return append(rawFrameHeader(kind, len(payload)), payload...)
}

// rawFrameHeader is a kind and length with no payload, for a length the payload
// does not follow.
func rawFrameHeader(kind byte, length int) []byte {
	out := make([]byte, 5)
	out[0] = kind
	binary.BigEndian.PutUint32(out[1:], uint32(length))
	return out
}

// errWriter is a sink that refuses every write.
type errWriter struct{ err error }

func (w errWriter) Write([]byte) (int, error) { return 0, w.err }

// errReader is a stream that fails every read.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
