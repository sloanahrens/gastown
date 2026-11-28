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

// failWriter accepts n writes and fails every write after them, signalling the
// first failure on failed and keeping what it accepted.
type failWriter struct {
	n      int
	err    error
	failed chan struct{} // when set, gets a value for a refused write if it has room
	buf    strings.Builder
}

func (w *failWriter) Write(p []byte) (int, error) {
	if w.n <= 0 {
		if w.failed != nil {
			select {
			case w.failed <- struct{}{}:
			default:
			}
		}
		return 0, w.err
	}
	w.n--
	return w.buf.Write(p)
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

// traceWriter keeps what Serve writes and notes each write in trace. It takes
// no lock on purpose: Serve alone must serialise its writes and call Close after
// the last answer. An unlocked writer lets the -race run catch a Serve that does
// not; a lock here would hide that.
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
	// The session does not read the put back, since answers may run at once, so
	// check the entry the store holds under the put's action ID ("aw==" is "k").
	wantEntry := gocacheprog.Entry{OutputID: []byte("new"), Size: 5, Time: putTime, DiskPath: "/store/new"}
	if got := store.entries["k"]; !reflect.DeepEqual(got, wantEntry) {
		t.Errorf("stored entry = %+v, want %+v", got, wantEntry)
	}
}

// parallelWait bounds every wait on a signal another goroutine must send.
const parallelWait = 10 * time.Second

// settle returns the first value on ch, failing the test when none arrives
// within parallelWait.
func settle[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(parallelWait): //testpolicy:allow no-sleep — a hang guard: another goroutine sends on ch, so this fires only when it never arrives
		t.Fatalf("waiting for %s: nothing within %s", what, parallelWait)
	}
	var zero T
	return zero
}

// hit is the entry heldStore answers with.
var hit = gocacheprog.Entry{OutputID: []byte("x"), Size: 3, DiskPath: "/store/x"}

// heldStore holds every get whose action ID is in hold until release, answers the
// rest at once, counts the gets it received, and counts the held gets it finished.
// When trace is set, Close notes itself there.
type heldStore struct {
	hold    []string
	started chan struct{}
	release chan struct{}
	trace   *[]string

	mu   sync.Mutex
	gets int
	done int
}

func (s *heldStore) Get(actionID []byte) (gocacheprog.Entry, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	if !slices.Contains(s.hold, string(actionID)) {
		return hit, nil
	}
	s.started <- struct{}{}
	select {
	case <-s.release:
	case <-time.After(parallelWait): //testpolicy:allow no-sleep — a hang guard: the test releases the get, so this fires only when it never does
		return gocacheprog.Entry{}, errors.New("heldStore: the held get was never released")
	}
	s.mu.Lock()
	s.done++
	s.mu.Unlock()
	return hit, nil
}

func (s *heldStore) Put(_, outputID, _ []byte) (string, error) {
	return "/store/" + string(outputID), nil
}

func (s *heldStore) Close() error {
	if s.trace != nil {
		*s.trace = append(*s.trace, "close")
	}
	return nil
}

func (s *heldStore) counts() (gets, done int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.done
}

// servedRun is what serve reports once Serve returns: its error, and how many
// gets the held store had finished at that moment.
type servedRun struct {
	err      error
	finished int
}

// serve starts Serve in the background and returns the channel its result
// arrives on. stop unblocks every channel the test holds Serve at, and the
// cleanup waits for Serve to return before the test ends.
func serve(t *testing.T, in io.Reader, out io.Writer, store *heldStore, stop func()) <-chan servedRun {
	t.Helper()
	done := make(chan servedRun, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_, err := gocacheprog.Serve(context.Background(), in, out, store)
		_, finished := store.counts()
		done <- servedRun{err: err, finished: finished}
	}()
	t.Cleanup(func() {
		stop()
		select {
		case <-exited:
		case <-time.After(parallelWait): //testpolicy:allow no-sleep — a hang guard: stop unblocks Serve, so this fires only when Serve never returns
			t.Errorf("Serve did not return after its stops were released")
		}
	})
	return done
}

func TestServeParallelAnswersTogether(t *testing.T) {
	t.Parallel()
	var trace []string
	store := &heldStore{hold: []string{"a"}, started: make(chan struct{}, 2), release: make(chan struct{}), trace: &trace}
	// Both gets are held, so both must be in flight together before either is
	// answered. The close follows them on the same input.
	in := `{"ID":1,"Command":"get","ActionID":"YQ=="}` + "\n" +
		`{"ID":2,"Command":"get","ActionID":"YQ=="}` + "\n" +
		`{"ID":3,"Command":"close"}` + "\n"
	out := &traceWriter{trace: &trace}
	release := sync.OnceFunc(func() { close(store.release) })
	done := serve(t, strings.NewReader(in), out, store, release)
	settle(t, store.started, "the first get must start")
	settle(t, store.started, "a Serve that answers one request at a time never starts the second get")
	release()
	run := settle(t, done, "Serve must return")
	if run.err != nil {
		t.Fatalf("Serve = %v, want nil", run.err)
	}
	got := decodeResponses(t, out.buf.String())
	if len(got) != 4 || !reflect.DeepEqual(got[0], handshake) {
		t.Fatalf("responses = %+v, want the handshake, two hits and the close", got)
	}
	// The hits come in either order, so they are sorted by ID before the check.
	slices.SortFunc(got[1:3], func(a, b gocacheprog.Response) int { return cmp.Compare(a.ID, b.ID) })
	for i, resp := range got[1:3] {
		if resp.ID != int64(i+1) || resp.Miss || string(resp.OutputID) != "x" || resp.Size != 3 {
			t.Errorf("response = %+v, want the whole hit x for ID %d", resp, i+1)
		}
	}
	if got[3].ID != 3 || got[3].Err != "" {
		t.Errorf("last response = %+v, want the close for ID 3", got[3])
	}
	// The store's Close falls after both hits and before the close response.
	if wantTrace := []string{"response", "response", "response", "close", "response"}; !reflect.DeepEqual(trace, wantTrace) {
		t.Errorf("trace = %v, want %v", trace, wantTrace)
	}
}

func TestServeParallelHeldGetThenBadLine(t *testing.T) {
	t.Parallel()
	store := &heldStore{hold: []string{"a", "b"}, started: make(chan struct{}, 2), release: make(chan struct{})}
	// Both gets are held, and the input then holds a line that is not a request.
	in := `{"ID":1,"Command":"get","ActionID":"YQ=="}` + "\n" +
		`{"ID":2,"Command":"get","ActionID":"Yg=="}` + "\n" +
		"not a request\n"
	var buf strings.Builder
	release := sync.OnceFunc(func() { close(store.release) })
	done := serve(t, strings.NewReader(in), &buf, store, release)
	settle(t, store.started, "the first held get must start")
	settle(t, store.started, "the second held get must start")
	release()
	run := settle(t, done, "Serve must return")
	if run.err == nil || !strings.Contains(run.err.Error(), "malformed") {
		t.Fatalf("Serve = %v, want the malformed request error", run.err)
	}
	if run.finished != 2 {
		t.Errorf("store had finished %d gets when Serve returned, want both held gets finished (2)", run.finished)
	}
	got := decodeResponses(t, buf.String())
	if len(got) != 3 || !reflect.DeepEqual(got[0], handshake) {
		t.Fatalf("responses = %+v, want the handshake and the two hits", got)
	}
	// The hits come in either order, so they are sorted by ID before the check.
	slices.SortFunc(got, func(a, b gocacheprog.Response) int { return cmp.Compare(a.ID, b.ID) })
	for i, resp := range got[1:] {
		if resp.ID != int64(i+1) || resp.Miss || string(resp.OutputID) != "x" || resp.Size != 3 {
			t.Errorf("response = %+v, want the whole hit x for ID %d", resp, i+1)
		}
	}
}

func TestServeParallelEndOfInputHeldWriteError(t *testing.T) {
	t.Parallel()
	store := &heldStore{hold: []string{"a"}, started: make(chan struct{}, 1), release: make(chan struct{})}
	// The get is held, and the input then ends. The handshake is the only write
	// the writer accepts, so the held get's response write fails after the end
	// of input, and Serve must return that write error, not nil.
	in := `{"ID":1,"Command":"get","ActionID":"YQ=="}` + "\n"
	out := &failWriter{n: 1, err: errors.New("pipe closed"), failed: make(chan struct{}, 1)}
	release := sync.OnceFunc(func() { close(store.release) })
	done := serve(t, strings.NewReader(in), out, store, release)
	settle(t, store.started, "the held get must start")
	release()
	run := settle(t, done, "Serve must return")
	if run.err == nil || !strings.Contains(run.err.Error(), "pipe closed") {
		t.Fatalf("Serve = %v, want the held response's write error", run.err)
	}
	if run.finished != 1 {
		t.Errorf("store had finished %d gets when Serve returned, want the held get finished (1)", run.finished)
	}
	settle(t, out.failed, "the held get's response write must be refused")
	if got := decodeResponses(t, out.buf.String()); !reflect.DeepEqual(got, []gocacheprog.Response{handshake}) {
		t.Errorf("responses = %+v, want the handshake only", got)
	}
}

// gateReader blocks until gate closes, then reads from inner. A gate that never
// opens is a read error, so Serve reports it instead of the test hanging.
type gateReader struct {
	gate  <-chan struct{}
	inner io.Reader
}

func (g *gateReader) Read(p []byte) (int, error) {
	select {
	case <-g.gate:
		return g.inner.Read(p)
	case <-time.After(parallelWait): //testpolicy:allow no-sleep — a hang guard: the test opens the gate, so this fires only when Serve never asks for the rest
		return 0, errors.New("gateReader: the gate never opened")
	}
}

func TestServeParallelWriteErrorStartsNoRequest(t *testing.T) {
	t.Parallel()
	store := &heldStore{hold: []string{"a"}, started: make(chan struct{}, 1), release: make(chan struct{})}
	// The first get is held in the store. The second is answered at once, and its
	// response write fails. The third waits behind gate, which the test opens only
	// after that write has failed: a Serve that reads on and starts it has no
	// write-error check after the read, and so gets three.
	gate := make(chan struct{})
	in := io.MultiReader(
		strings.NewReader(`{"ID":1,"Command":"get","ActionID":"YQ=="}`+"\n"+`{"ID":2,"Command":"get","ActionID":"Yg=="}`+"\n"),
		&gateReader{gate: gate, inner: strings.NewReader(`{"ID":3,"Command":"get","ActionID":"Yw=="}` + "\n")},
	)
	out := &failWriter{n: 1, err: errors.New("pipe closed"), failed: make(chan struct{}, 1)}
	openGate := sync.OnceFunc(func() { close(gate) })
	release := sync.OnceFunc(func() { close(store.release) })
	done := serve(t, in, out, store, func() { openGate(); release() })
	settle(t, store.started, "the held get must start")
	settle(t, out.failed, "the second get's response write must fail")
	openGate()
	// Serve returns only once the held get has finished, so the test releases it
	// here and reads the counts from that return.
	release()
	run := settle(t, done, "Serve must return")
	if run.err == nil || !strings.Contains(run.err.Error(), "pipe closed") {
		t.Fatalf("Serve = %v, want the write error", run.err)
	}
	if gets, _ := store.counts(); gets != 2 || run.finished != 1 {
		t.Errorf("store saw %d gets and finished %d when Serve returned, want the two read and the one in flight finished", gets, run.finished)
	}
	if got := decodeResponses(t, out.buf.String()); !reflect.DeepEqual(got, []gocacheprog.Response{handshake}) {
		t.Errorf("responses = %+v, want the handshake only", got)
	}
}
