package gocacheprog

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// fakeStore answers Get from entries and fails every Get with getErr when that
// is set, so a row can tell a store read from a store read that faulted. Put
// returns putPath, or putErr when that is set.
type fakeStore struct {
	entries map[string]Entry
	getErr  error
	putPath string
	putErr  error
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
	if f.putErr != nil {
		return "", f.putErr
	}
	return f.putPath, nil
}

// closeCounter is a store with a Close method. It counts its Close calls and
// fails them with closeErr when that is set.
type closeCounter struct {
	fakeStore
	closeErr error
	closes   int
}

func (c *closeCounter) Close() error {
	c.closes++
	return c.closeErr
}

func TestAnswer(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	hit := Entry{OutputID: []byte("out"), Size: 12, Time: now, DiskPath: "/store/out"}
	tests := []struct {
		name       string
		req        Request
		store      Store
		want       Response
		wantStop   bool
		wantStats  Stats
		wantCloses int
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
			name:      "put returns the disk path and counts its bytes",
			req:       Request{ID: 4, Command: CmdPut, ActionID: []byte("key"), OutputID: []byte("out"), Body: []byte("hello")},
			store:     fakeStore{putPath: "/store/key"},
			want:      Response{ID: 4, DiskPath: "/store/key"},
			wantStats: Stats{Puts: 1, PutBytes: 5},
		},
		{
			name:      "failing put is answered with err and adds no put bytes",
			req:       Request{ID: 5, Command: CmdPut, ActionID: []byte("key"), OutputID: []byte("out"), Body: []byte("hello")},
			store:     fakeStore{putErr: errors.New("disk full")},
			want:      Response{ID: 5, Err: "disk full"},
			wantStats: Stats{Puts: 1, Errors: 1},
		},
		{
			name:      "close on a store without Close stops serving",
			req:       Request{ID: 6, Command: CmdClose},
			store:     fakeStore{},
			want:      Response{ID: 6},
			wantStop:  true,
			wantStats: Stats{},
		},
		{
			name:       "close calls Close once and stops serving",
			req:        Request{ID: 7, Command: CmdClose},
			store:      &closeCounter{},
			want:       Response{ID: 7},
			wantStop:   true,
			wantStats:  Stats{},
			wantCloses: 1,
		},
		{
			name:       "failing close counts an error and changes nothing else",
			req:        Request{ID: 8, Command: CmdClose},
			store:      &closeCounter{closeErr: errors.New("flush failed")},
			want:       Response{ID: 8},
			wantStop:   true,
			wantStats:  Stats{Errors: 1},
			wantCloses: 1,
		},
		{
			name:      "unknown command is answered with err and counted",
			req:       Request{ID: 9, Command: "get2"},
			store:     fakeStore{getErr: errors.New("store must not be read")},
			want:      Response{ID: 9, Err: `gocacheprog: unknown command "get2"`},
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
			closes := 0
			if c, ok := tt.store.(*closeCounter); ok {
				closes = c.closes
			}
			if closes != tt.wantCloses {
				t.Errorf("closes = %d, want %d", closes, tt.wantCloses)
			}
		})
	}
}
