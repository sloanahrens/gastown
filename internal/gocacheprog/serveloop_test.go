package gocacheprog_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
	got := decodeResponses(t, buf.String())
	slices.SortFunc(got, func(a, b gocacheprog.Response) int { return cmp.Compare(a.ID, b.ID) })
	if !reflect.DeepEqual(got, want) {
		t.Errorf("responses = %+v, want %+v", got, want)
	}
	if wantStats := (gocacheprog.Stats{Gets: 1, Misses: 1, Puts: 1, PutBytes: 2}); stats != wantStats {
		t.Errorf("stats = %+v, want %+v", stats, wantStats)
	}
	if store.closes != 0 {
		t.Errorf("store closed %d times, want 0", store.closes)
	}
}

// traceStore holds entries in memory, stamps each Put with now, and notes its
// Close in trace so the test sees where Close falls among the writes. It locks
// the entries so a Serve answering requests at once can share them.
type traceStore struct {
	mu      sync.Mutex
	entries map[string]gocacheprog.Entry
	now     time.Time
	trace   *[]string
	closes  int
}

func (s *traceStore) Get(actionID []byte) (gocacheprog.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[string(actionID)]
	if !ok {
		return gocacheprog.Entry{}, gocacheprog.ErrMiss
	}
	return e, nil
}

func (s *traceStore) Put(actionID, outputID, body []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := "/store/" + string(outputID)
	s.entries[string(actionID)] = gocacheprog.Entry{OutputID: outputID, Size: int64(len(body)), Time: s.now, DiskPath: path}
	return path, nil
}

func (s *traceStore) Close() error {
	*s.trace = append(*s.trace, "close")
	s.closes++
	return nil
}

// traceWriter keeps what Serve writes and notes each write in trace.
type traceWriter struct {
	buf   strings.Builder
	trace *[]string
}

func (w *traceWriter) Write(p []byte) (int, error) {
	*w.trace = append(*w.trace, "response")
	return w.buf.Write(p)
}

func TestServeSession(t *testing.T) {
	t.Parallel()
	var trace []string
	putTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	hitTime := putTime.Add(-time.Hour)
	store := &traceStore{
		entries: map[string]gocacheprog.Entry{"hit": {OutputID: []byte("old"), Size: 10, Time: hitTime, DiskPath: "/store/old"}},
		now:     putTime,
		trace:   &trace,
	}
	// No request may depend on an earlier one's effect in the store: a Serve
	// may answer them at once.
	in := strings.Join([]string{
		`{"ID":1,"Command":"get","ActionID":"aGl0"}`,
		`{"ID":2,"Command":"get","ActionID":"bWlzcw=="}`,
		`{"ID":3,"Command":"put","ActionID":"aw==","OutputID":"bmV3","BodySize":5}`,
		`"aGVsbG8="`,
		`{"ID":4,"Command":"get","ActionID":"aGl0"}`,
		`{"ID":5,"Command":"close"}`,
	}, "\n") + "\n"
	out := &traceWriter{trace: &trace}
	stats, err := gocacheprog.Serve(context.Background(), strings.NewReader(in), out, store)
	if err != nil {
		t.Fatalf("Serve = %v, want nil", err)
	}
	want := []gocacheprog.Response{
		handshake,
		{ID: 1, OutputID: []byte("old"), Size: 10, Time: &hitTime, DiskPath: "/store/old"},
		{ID: 2, Miss: true},
		{ID: 3, DiskPath: "/store/new"},
		{ID: 4, OutputID: []byte("old"), Size: 10, Time: &hitTime, DiskPath: "/store/old"},
		{ID: 5},
	}
	got := decodeResponses(t, out.buf.String())
	if len(got) != len(want) || !reflect.DeepEqual(got[0], handshake) {
		t.Fatalf("responses = %+v, want the handshake and %+v", got, want[1:])
	}
	// Answers for requests served at once come in either order, so only the
	// close is pinned to a position: it follows every earlier answer.
	if last := got[len(got)-1]; last.ID != 5 || last.Err != "" {
		t.Errorf("last response = %+v, want the close", last)
	}
	slices.SortFunc(got, func(a, b gocacheprog.Response) int { return cmp.Compare(a.ID, b.ID) })
	if !reflect.DeepEqual(got, want) {
		t.Errorf("responses = %+v, want %+v", got, want)
	}
	if wantStats := (gocacheprog.Stats{Gets: 3, Hits: 2, Misses: 1, Puts: 1, PutBytes: 5, HitBytes: 20}); stats != wantStats {
		t.Errorf("stats = %+v, want %+v", stats, wantStats)
	}
	// Five writes (the handshake and IDs 1 to 4) come before close; the close
	// response is written after the store's Close.
	if wantTrace := []string{"response", "response", "response", "response", "response", "close", "response"}; !reflect.DeepEqual(trace, wantTrace) {
		t.Errorf("trace = %v, want %v", trace, wantTrace)
	}
	if store.closes != 1 {
		t.Errorf("store closed %d times, want 1", store.closes)
	}
}
