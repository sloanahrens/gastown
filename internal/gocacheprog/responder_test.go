package gocacheprog

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestResponderConcurrentWrites(t *testing.T) {
	t.Parallel()
	var out strings.Builder // takes no lock on purpose: the responder must serialize.
	r := &responder{out: &out}
	const n = 64
	sum := n * (n + 1) / 2
	want := Stats{Gets: sum, Hits: 2 * sum, Misses: 3 * sum, Puts: 5 * sum, PutBytes: int64(7 * sum), HitBytes: int64(11 * sum), Errors: 13 * sum}
	done := make(chan struct{}, n)
	for i := 1; i <= n; i++ {
		delta := Stats{Gets: i, Hits: 2 * i, Misses: 3 * i, Puts: 5 * i, PutBytes: int64(7 * i), HitBytes: int64(11 * i), Errors: 13 * i}
		go func(id int, delta Stats) {
			if err := r.write(&Response{ID: int64(id)}, delta); err != nil {
				t.Errorf("write(%d) = %v, want nil", id, err)
			}
			done <- struct{}{}
		}(i, delta)
	}
	//testpolicy:allow no-sleep — a hang guard: each write sends on done, so the deadline fires only when one never returns
	for i, deadline := 0, time.After(10*time.Second); i < n; i++ {
		select {
		case <-done:
		case <-deadline:
			t.Fatalf("%d of %d writes finished", i, n)
		}
	}
	dec := json.NewDecoder(strings.NewReader(out.String()))
	seen := make(map[int64]bool)
	for dec.More() {
		var resp Response
		if err := dec.Decode(&resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		seen[resp.ID] = true
	}
	if len(seen) != n {
		t.Errorf("wrote %d responses, want %d", len(seen), n)
	}
	if got := r.read(); got != want {
		t.Errorf("read() = %+v, want %+v", got, want)
	}
}

func TestResponderFailedWriteAddsCounts(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("pipe closed")
	r := &responder{out: failingWriter{err: writeErr}}
	delta := Stats{Gets: 1, Hits: 2, Misses: 3, Puts: 4, PutBytes: 5, HitBytes: 6, Errors: 7}
	err := r.write(&Response{ID: 1}, delta)
	if err == nil || !strings.Contains(err.Error(), writeErr.Error()) {
		t.Fatalf("write = %v, want %v", err, writeErr)
	}
	if got := r.read(); got != delta {
		t.Errorf("read() = %+v, want the failed write's counts %+v", got, delta)
	}
}
