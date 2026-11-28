package gocacheprog

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// fakeStore answers Get from entries and fails every Get with getErr when that
// is set, so a row can tell a store read from a store read that faulted.
type fakeStore struct {
	entries map[string]Entry
	getErr  error
}

func (f fakeStore) Get(actionID []byte) (Entry, error) {
	if f.getErr != nil {
		return Entry{}, f.getErr
	}
	e, ok := f.entries[string(actionID)]
	if !ok {
		return Entry{}, ErrMiss
	}
	return e, nil
}

func (f fakeStore) Put(actionID, outputID, body []byte) (string, error) {
	return "", errors.New("answer does not put")
}

func TestAnswer(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	hit := Entry{OutputID: []byte("out"), Size: 12, Time: now, DiskPath: "/store/out"}
	tests := []struct {
		name      string
		req       Request
		store     fakeStore
		want      Response
		wantStop  bool
		wantStats Stats
	}{
		{
			name:      "hit returns the entry and counts it",
			req:       Request{ID: 1, Command: CmdGet, ActionID: []byte("key")},
			store:     fakeStore{entries: map[string]Entry{"key": hit}},
			want:      Response{ID: 1, OutputID: []byte("out"), Size: 12, Time: &now, DiskPath: "/store/out"},
			wantStats: Stats{Gets: 1, Hits: 1, HitBytes: 12},
		},
		{
			name:      "miss is answered and counted as a miss",
			req:       Request{ID: 2, Command: CmdGet, ActionID: []byte("absent")},
			store:     fakeStore{entries: map[string]Entry{}},
			want:      Response{ID: 2, Miss: true},
			wantStats: Stats{Gets: 1, Misses: 1},
		},
		{
			name:      "failing get is a miss and an error, never a failure",
			req:       Request{ID: 3, Command: CmdGet, ActionID: []byte("key")},
			store:     fakeStore{getErr: errors.New("disk gone")},
			want:      Response{ID: 3, Miss: true},
			wantStats: Stats{Gets: 1, Misses: 1, Errors: 1},
		},
		{
			name:      "unknown command is answered with err and counted",
			req:       Request{ID: 4, Command: "get2"},
			store:     fakeStore{getErr: errors.New("store must not be read")},
			want:      Response{ID: 4, Err: `gocacheprog: unknown command "get2"`},
			wantStats: Stats{Errors: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stats Stats
			got, stop := answer(&tt.req, tt.store, &stats)
			if !reflect.DeepEqual(*got, tt.want) {
				t.Errorf("response = %+v, want %+v", *got, tt.want)
			}
			if stop != tt.wantStop {
				t.Errorf("stop = %v, want %v", stop, tt.wantStop)
			}
			if stats != tt.wantStats {
				t.Errorf("stats = %+v, want %+v", stats, tt.wantStats)
			}
		})
	}
}
