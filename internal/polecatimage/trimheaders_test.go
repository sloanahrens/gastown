package polecatimage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

const (
	trimHeaderActionID = "1111111111111111111111111111111111111111111111111111111111111111"
	trimHeaderOutputID = "2222222222222222222222222222222222222222222222222222222222222222"
	trimHeaderOtherID  = "3333333333333333333333333333333333333333333333333333333333333333"
	trimHeaderAction   = cachevol.SeedBuildCacheDir + "/11/" + trimHeaderActionID + "-a"
	trimHeaderOutput   = cachevol.SeedBuildCacheDir + "/22/" + trimHeaderOutputID + "-d"
	// trimHeaderModule takes a path the build cache would read, so only its Cache keeps it unopened.
	trimHeaderModule = cachevol.SeedBuildCacheDir + "/33/" + trimHeaderOtherID + "-a"
	trimHeaderBody   = "v1 " + trimHeaderActionID + " " + trimHeaderOutputID + " "
	// trimHeaderBound is a hang guard, not a delay: a pool that serialises reads never signals a second one.
	trimHeaderBound = 5 * time.Second
)

// trimHeaderActionEntries returns n build-cache action entries, the shape a claim's listing hands the pool.
func trimHeaderActionEntries(n int) []cachevol.Entry {
	entries := make([]cachevol.Entry, n)
	for i := range entries {
		entries[i] = cachevol.Entry{Path: trimHeaderAction, Cache: cachevol.BuildCache}
	}
	return entries
}

// trimHeaderCloser reports its own Close so a row can see the pool closed every reader the opener handed out.
type trimHeaderCloser struct {
	io.Reader
	onClose func()
}

func (r *trimHeaderCloser) Close() error { r.onClose(); return nil }

func TestTrimHeaderPool(t *testing.T) {
	t.Parallel()
	t.Run("outputs match a serial read", func(t *testing.T) {
		t.Parallel()
		open := func(p string) (io.ReadCloser, error) {
			if p != trimHeaderAction {
				return nil, errors.New("opened " + p + ", want only the build-cache action file")
			}
			return io.NopCloser(bytes.NewReader([]byte(trimHeaderBody))), nil
		}
		entries := []cachevol.Entry{{Path: trimHeaderOutput, Cache: cachevol.BuildCache}, {Path: trimHeaderModule, Cache: cachevol.ModuleCache}, {Path: trimHeaderAction, Cache: cachevol.BuildCache}}
		want := []cachevol.Entry{entries[0], entries[1], {Path: trimHeaderAction, Cache: cachevol.BuildCache, Output: trimHeaderOutput}}
		if err := readBuildOutputs(entries, open, 3); err != nil {
			t.Fatalf("readBuildOutputs error = %v, want nil", err)
		}
		if !reflect.DeepEqual(entries, want) {
			t.Errorf("readBuildOutputs entries = %#v, want the serial read's %#v", entries, want)
		}
	})
	t.Run("two reads are in flight at once, and every reader is closed at return", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), trimHeaderBound)
		defer cancel()
		var inFlight, opened, closed atomic.Int32
		var overlapped atomic.Bool
		two := make(chan struct{})
		open := func(string) (io.ReadCloser, error) {
			opened.Add(1)
			if inFlight.Add(1) == 2 {
				overlapped.Store(true)
				close(two)
			}
			select {
			case <-two:
			case <-ctx.Done():
			}
			inFlight.Add(-1)
			return &trimHeaderCloser{Reader: bytes.NewReader([]byte(trimHeaderBody)), onClose: func() { closed.Add(1) }}, nil
		}
		if err := readBuildOutputs(trimHeaderActionEntries(2), open, 2); err != nil {
			t.Fatalf("readBuildOutputs error = %v, want nil", err)
		}
		if !overlapped.Load() {
			t.Fatalf("readBuildOutputs never had two header reads in flight; a one-worker pool fails here")
		}
		if opened.Load() != 2 || closed.Load() != opened.Load() {
			t.Errorf("opened %d readers and closed %d, want both closed at return", opened.Load(), closed.Load())
		}
	})
	t.Run("no open starts after a read fails", func(t *testing.T) {
		t.Parallel()
		var opens int
		open := func(string) (io.ReadCloser, error) {
			opens++
			return nil, errors.New("the claim is unreadable")
		}
		if err := readBuildOutputs(trimHeaderActionEntries(3), open, 1); err == nil {
			t.Fatal("readBuildOutputs error = nil, want the failed read's error")
		}
		if opens != 1 {
			t.Errorf("opener called %d times, want 1: no read starts once a failure is seen", opens)
		}
	})
	t.Run("a vanished file leaves no Output", func(t *testing.T) {
		t.Parallel()
		open := func(string) (io.ReadCloser, error) { return nil, fs.ErrNotExist }
		entries := trimHeaderActionEntries(1)
		if err := readBuildOutputs(entries, open, 2); err != nil {
			t.Fatalf("readBuildOutputs error = %v, want nil for a file another job removed", err)
		}
		if entries[0].Output != "" {
			t.Errorf("Output = %q, want none: a vanished action file names no live output", entries[0].Output)
		}
	})
	t.Run("a failed read names its file", func(t *testing.T) {
		t.Parallel()
		open := func(string) (io.ReadCloser, error) { return nil, errors.New("the claim is unreadable") }
		entries := []cachevol.Entry{{Path: trimHeaderAction, Cache: cachevol.BuildCache}}
		want := "reading the build-cache header of " + trimHeaderAction + ": the claim is unreadable"
		if err := readBuildOutputs(entries, open, 2); err == nil || err.Error() != want {
			t.Fatalf("readBuildOutputs error = %v, want %q", err, want)
		}
	})
	t.Run("zero workers is an error", func(t *testing.T) {
		if err := readBuildOutputs(nil, nil, 0); err == nil {
			t.Fatal("readBuildOutputs error = nil, want an error for zero workers")
		}
	})
}
