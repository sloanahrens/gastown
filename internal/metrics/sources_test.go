package metrics

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// The registry test lists names only: it never calls Collect, so it runs no
// command and reads no file whatever the sources do.
func TestSourcesRegistersEveryGroup(t *testing.T) {
	t.Parallel()
	want := []string{"attention", "config", "host", "host_cpu", "money", "placement", "queue", "resources", "review", "sweep"}
	var got []string
	for _, src := range Sources() {
		got = append(got, src.Group())
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registered groups = %v, want %v", got, want)
	}
}

// TestSourcesCollectHoldsTheContract runs Collect of every registered source
// over an empty town whose seams answer no file and no command, so a source
// added to the registry is exercised here without a host. The Source contract
// leaves the group null when Collect errors; a source must not lean on the
// snapshot to discard columns it returned beside one (gt-cjwx0).
func TestSourcesCollectHoldsTheContract(t *testing.T) {
	t.Parallel()
	env := emptyTownEnv(t)
	for _, src := range Sources() {
		t.Run(src.Group(), func(t *testing.T) {
			t.Parallel()
			cols, err := src.Collect(context.Background(), env)
			if err != nil && cols != nil {
				t.Fatalf("%s Collect = %v, %v, want the group null when it errors", src.Group(), cols, err)
			}
		})
	}
}

// emptyTownEnv is a town with no files and no commands: each read misses and
// each run fails, so a source's answer depends on neither the host it runs on
// nor the order the subtests run in.
func emptyTownEnv(t *testing.T) Env {
	t.Helper()
	miss := func(string) ([]byte, error) { return nil, fs.ErrNotExist }
	noCommand := errors.New("no command in the registry test")
	return Env{
		Town: t.TempDir(),
		Run:  func(context.Context, []string) ([]byte, error) { return nil, noCommand },
		Beads: func(context.Context, string) BeadStore {
			return failingStore{err: noCommand}
		},
		RunEnv:   func(context.Context, []string, []string) ([]byte, error) { return nil, noCommand },
		Now:      func() time.Time { return time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC) },
		ReadFile: miss,
		ModTime:  func(string) (time.Time, error) { return time.Time{}, fs.ErrNotExist },
		ReadDir:  func(string) ([]DirEntry, error) { return nil, fs.ErrNotExist },
		Tail:     func(string, int64) ([]byte, error) { return nil, fs.ErrNotExist },
	}
}

// failingStore is the empty town's bead store: every read fails the way a town
// whose database is not up does.
type failingStore struct{ err error }

func (s failingStore) List(beads.ListOptions) ([]*beads.Issue, error) { return nil, s.err }
func (s failingStore) ReadyFiltered(beads.ReadyOptions) ([]*beads.Issue, error) {
	return nil, s.err
}
func (s failingStore) Blocked() ([]*beads.Issue, error) { return nil, s.err }

func (s failingStore) ShowMultiple([]string) (map[string]*beads.Issue, error) {
	return nil, s.err
}
