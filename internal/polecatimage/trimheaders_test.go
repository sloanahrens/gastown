package polecatimage

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"

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
)

func TestTrimHeaderPool(t *testing.T) {
	t.Parallel()
	t.Run("outputs match a serial read", func(t *testing.T) {
		t.Parallel()
		open := func(p string) (io.ReadCloser, error) {
			if p != trimHeaderAction {
				return nil, errors.New("opened " + p + ", want only the build-cache action file")
			}
			return io.NopCloser(bytes.NewReader([]byte("v1 " + trimHeaderActionID + " " + trimHeaderOutputID + " "))), nil
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
