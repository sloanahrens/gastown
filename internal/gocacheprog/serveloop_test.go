package gocacheprog_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/gocacheprog"
)

// missStore misses every get, accepts every put and counts its Close calls.
type missStore struct{ closes int }

func (m *missStore) Get([]byte) (gocacheprog.Entry, error) {
	return gocacheprog.Entry{}, gocacheprog.ErrMiss
}

func (m *missStore) Put(_, outputID, _ []byte) (string, error) {
	return "/store/" + string(outputID), nil
}

func (m *missStore) Close() error {
	m.closes++
	return nil
}

// failWriter accepts n writes and fails every write after them.
type failWriter struct {
	n   int
	err error
}

func (w *failWriter) Write(p []byte) (int, error) {
	if w.n <= 0 {
		return 0, w.err
	}
	w.n--
	return len(p), nil
}

// decodeResponses returns every response value written to written.
func decodeResponses(t *testing.T, written string) []gocacheprog.Response {
	t.Helper()
	var got []gocacheprog.Response
	dec := json.NewDecoder(strings.NewReader(written))
	for dec.More() {
		var resp gocacheprog.Response
		if err := dec.Decode(&resp); err != nil {
			t.Fatalf("responses %q: %v", written, err)
		}
		got = append(got, resp)
	}
	return got
}

var (
	handshake = gocacheprog.Response{KnownCommands: []gocacheprog.Cmd{gocacheprog.CmdGet, gocacheprog.CmdPut, gocacheprog.CmdClose}}
	closeLine = `{"ID":1,"Command":"close"}` + "\n"
)

func TestServeErrors(t *testing.T) {
	t.Parallel()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name    string
		ctx     context.Context
		in      string
		writer  *failWriter
		wantErr string
	}{
		{name: "read error ends serve", in: `{"ID":1,"Command":"get","ActionID":"aw=="`, wantErr: "cut request"},
		{name: "handshake write error ends serve", in: closeLine, writer: &failWriter{err: errors.New("pipe closed")}, wantErr: "pipe closed"},
		{name: "response write error ends serve", in: closeLine, writer: &failWriter{n: 1, err: errors.New("pipe closed")}, wantErr: "pipe closed"},
		{name: "cancelled context ends serve", ctx: cancelled, in: closeLine, wantErr: "context canceled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tc.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			var buf strings.Builder
			var out io.Writer = &buf
			if tc.writer != nil {
				out = tc.writer
			}
			_, err := gocacheprog.Serve(ctx, strings.NewReader(tc.in), out, &missStore{})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Serve = %v, want an error carrying %q", err, tc.wantErr)
			}
			if tc.writer == nil {
				if got := decodeResponses(t, buf.String()); !reflect.DeepEqual(got, []gocacheprog.Response{handshake}) {
					t.Errorf("responses = %+v, want the handshake only", got)
				}
			}
		})
	}
}

func TestServeEndOfInput(t *testing.T) {
	t.Parallel()
	store := &missStore{}
	in := `{"ID":1,"Command":"get","ActionID":"aw=="}` + "\n" +
		`{"ID":2,"Command":"put","ActionID":"aw==","OutputID":"bmV3","BodySize":2}` + "\n" +
		`"aGk="` + "\n"
	var buf strings.Builder
	stats, err := gocacheprog.Serve(context.Background(), strings.NewReader(in), &buf, store)
	if err != nil {
		t.Fatalf("Serve = %v, want nil", err)
	}
	want := []gocacheprog.Response{handshake, {ID: 1, Miss: true}, {ID: 2, DiskPath: "/store/new"}}
	if got := decodeResponses(t, buf.String()); !reflect.DeepEqual(got, want) {
		t.Errorf("responses = %+v, want %+v", got, want)
	}
	if wantStats := (gocacheprog.Stats{Gets: 1, Misses: 1, Puts: 1, PutBytes: 2}); stats != wantStats {
		t.Errorf("stats = %+v, want %+v", stats, wantStats)
	}
	if store.closes != 0 {
		t.Errorf("store closed %d times, want 0", store.closes)
	}
}
