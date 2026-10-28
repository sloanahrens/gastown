package metrics

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// showTestFound is the one issue the fake store holds, as bd prints it in a
// show's issue array.
const showTestFound = `[{"id":"gt-found","title":"found"}]`

// showTestPartial is bd's machine-mode answer to a show of one id it holds and
// one it does not: the held issue in data, the missing id under the envelope's
// error (kind partial, exit 22, the failed id not_found). The next bead's
// loaders read the missing id as a bead that is gone rather than as a read
// that broke, and this is the bd answer that makes that true.
const showTestPartial = `{
  "schema_version": 1,
  "contract_version": 1,
  "data": [{"id":"gt-found","title":"found"}],
  "pagination": null,
  "error": {
    "kind": "partial",
    "message": "1 of 2 ids failed",
    "ids": [
      {"id": "gt-missing", "kind": "not_found", "message": "no issue found matching \"gt-missing\""}
    ]
  }
}`

// showTestNoneFor is bd's machine-mode answer to a show that resolved none of
// its ids: data null, exit 20, the ids under the envelope's error (kind
// not_found). It is the shape internal/beads' notFoundEnvelope captures from
// bd, and it is what a bead-at-a-time read meets once its one bead is gone.
func showTestNoneFor(ids []string) string {
	entries := make([]string, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, fmt.Sprintf(`{"id":%q,"kind":"not_found","message":"no issue found matching %q"}`, id, id))
	}
	return fmt.Sprintf(`{
  "schema_version": 1,
  "contract_version": 1,
  "data": null,
  "pagination": null,
  "error": {
    "kind": "not_found",
    "message": "%d of %d ids failed",
    "ids": [%s]
  }
}`, len(ids), len(ids), strings.Join(entries, ","))
}

// showTestExit is a bd failure carrying bd's typed exit status, as
// *exec.ExitError does.
type showTestExit int

func (e showTestExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e showTestExit) ExitCode() int { return int(e) }

// showArgsIDs returns the bead ids a recorded show argv names: bd's verb, its
// flags (the seam's --allow-stale among them) and the probe's version word are
// not ids.
func showArgsIDs(args []string) []string {
	var ids []string
	for _, arg := range args {
		if arg == "show" || arg == "version" || strings.HasPrefix(arg, "-") {
			continue
		}
		ids = append(ids, arg)
	}
	return ids
}

// showTestStore is the real store over a fake bd, so the answer showBeads reads
// went through production's argv, routing, parser and failure classification.
func showTestStore(answer func(ctx context.Context, args []string) (stdout, stderr []byte, err error)) BeadStores {
	return func(_ context.Context, storeDir string) BeadStore {
		return beads.New(storeDir, beads.WithRunner(answer))
	}
}

// TestShowBeadsLeavesAMissingBeadOut: a bead the store does not hold is left
// out of the answer and is not an error. The store is a real *beads.Beads over
// a fake bd that answers the partial show the way bd does (machine envelope,
// exit 22, the failed id not_found), so the not-found contract is the seam's
// reading of bd's answer rather than something this package asserts.
func TestShowBeadsLeavesAMissingBeadOut(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	answer := func(_ context.Context, args []string) ([]byte, []byte, error) {
		if len(args) > 0 && args[len(args)-1] == "version" {
			return []byte("bd version 1.2.2\n"), nil, nil
		}
		for _, id := range showArgsIDs(args) {
			if id == "gt-missing" {
				return []byte(showTestPartial), []byte("Error: 1 of 2 ids failed\n"), showTestExit(22)
			}
		}
		return []byte(showTestFound), nil, nil
	}

	got, err := showBeads(context.Background(), Env{Beads: showTestStore(answer)}, dir,
		[]string{"gt-found", "gt-missing"})
	if err != nil {
		t.Fatalf("showBeads = %v, want no error: a bead the store does not hold is its answer", err)
	}
	if got["gt-found"] == nil {
		t.Errorf("showBeads = %v, want the found bead present", got)
	}
	if issue, ok := got["gt-missing"]; ok {
		t.Errorf("showBeads = %v, want the missing bead left out", issue)
	}
}

// TestShowBeadsAWhollyMissingIdIsNone: a read whose every id the store does not
// hold is none found, not a broken read. bd answers such a show with its
// not-found envelope and exit 20, and the seam turns that into
// beads.ErrNotFound; a reader that asks about one bead at a time must read it as
// a bead that is gone, as every reader did before the seam (gt-55ypc).
func TestShowBeadsAWhollyMissingIdIsNone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	answer := func(_ context.Context, args []string) ([]byte, []byte, error) {
		if len(args) > 0 && args[len(args)-1] == "version" {
			return []byte("bd version 1.2.2\n"), nil, nil
		}
		ids := showArgsIDs(args)
		return []byte(showTestNoneFor(ids)), nil, showTestExit(20)
	}

	got, err := showBeads(context.Background(), Env{Beads: showTestStore(answer)}, dir,
		[]string{"gt-missing"})
	if err != nil {
		t.Fatalf("showBeads = %v, want no error: an id the store does not hold is none found", err)
	}
	if len(got) != 0 {
		t.Errorf("showBeads = %v, want no bead for a wholly missing id", got)
	}
}

// TestShowBeadsAWhollyMissingChunkIsNone: a chunk the store holds none of does
// not end the read. 130 ids are three chunks of 64, 64 and 2; the first and the
// last are wholly missing and the middle one holds a bead, so the answer is that
// one bead and no error (gt-55ypc).
func TestShowBeadsAWhollyMissingChunkIsNone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const held = "gt-b070"
	reads := 0
	answer := func(_ context.Context, args []string) ([]byte, []byte, error) {
		if len(args) > 0 && args[len(args)-1] == "version" {
			return []byte("bd version 1.2.2\n"), nil, nil
		}
		reads++
		ids := showArgsIDs(args)
		for _, id := range ids {
			if id == held {
				return []byte(fmt.Sprintf(`[{"id":%q,"title":"held"}]`, held)), nil, nil
			}
		}
		return []byte(showTestNoneFor(ids)), nil, showTestExit(20)
	}
	ids := make([]string, 0, 130)
	for i := 0; i < 130; i++ {
		ids = append(ids, fmt.Sprintf("gt-b%03d", i))
	}

	got, err := showBeads(context.Background(), Env{Beads: showTestStore(answer)}, dir, ids)
	if err != nil {
		t.Fatalf("showBeads = %v, want no error: a chunk of ids the store does not hold is none found", err)
	}
	if got[held] == nil {
		t.Errorf("showBeads = %v, want the bead the middle chunk holds", got)
	}
	if len(got) != 1 {
		t.Errorf("showBeads answered %d bead(s), want only %q", len(got), held)
	}
	if reads != 3 {
		t.Errorf("the store read %d chunk(s), want 3: a not-found chunk must not end the read", reads)
	}
}

// TestShowBeadsFailedReadIsAnError: a read that fails for any reason other than
// an id the store does not hold is an error, so a caller never prints a broken
// store as beads that are gone.
func TestShowBeadsFailedReadIsAnError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	answer := func(_ context.Context, args []string) ([]byte, []byte, error) {
		if len(args) > 0 && args[len(args)-1] == "version" {
			return []byte("bd version 1.2.2\n"), nil, nil
		}
		return nil, []byte("Error: failed to open database: invalid connection\n"), showTestExit(1)
	}

	got, err := showBeads(context.Background(), Env{Beads: showTestStore(answer)}, dir,
		[]string{"gt-found"})
	if err == nil {
		t.Fatalf("showBeads = %v, want an error for a failed read", got)
	}
	if got != nil {
		t.Errorf("showBeads = %v with %v, want no answer from a failed read", got, err)
	}
}

// TestShowBeadsRefusesANilSeam: an Env with no store seam and a seam that names
// no store are both errors. A source that counted the beads of either would
// report an empty town rather than the wiring fault.
func TestShowBeadsRefusesANilSeam(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  Env
	}{
		{name: "no seam", env: Env{}},
		{name: "seam names no store", env: Env{Beads: func(context.Context, string) BeadStore { return nil }}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := showBeads(context.Background(), tc.env, "/town", []string{"gt-found"})
			if err == nil {
				t.Fatalf("showBeads = %v, want an error for %s", got, tc.name)
			}
		})
	}
}

// chunkStore answers every id it is given and records each read's ids, so a
// test sees exactly what reached the store.
type chunkStore struct {
	chunks [][]string
}

func (s *chunkStore) List(beads.ListOptions) ([]*beads.Issue, error) {
	return nil, errors.New("the show test read the board")
}

func (s *chunkStore) ReadyFiltered(beads.ReadyOptions) ([]*beads.Issue, error) {
	return nil, errors.New("the show test read the ready queue")
}

func (s *chunkStore) Blocked() ([]*beads.Issue, error) {
	return nil, errors.New("the show test read the blocked list")
}

func (s *chunkStore) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	s.chunks = append(s.chunks, append([]string(nil), ids...))
	out := make(map[string]*beads.Issue, len(ids))
	for _, id := range ids {
		out[id] = &beads.Issue{ID: id}
	}
	return out, nil
}

// TestShowBeadsSkipsDashIdsAndChunks pins the two shapes of a store read: an id
// bd would read as a flag (or an empty one) never reaches the store, and a long
// list is read in chunks of storeShowChunk, so 130 ids are three reads of 64,
// 64 and 2.
func TestShowBeadsSkipsDashIdsAndChunks(t *testing.T) {
	t.Parallel()
	ids := make([]string, 0, 132)
	for i := 0; i < 130; i++ {
		ids = append(ids, fmt.Sprintf("gt-b%03d", i))
	}
	ids = append(ids, "-dash", "")

	store := &chunkStore{}
	env := Env{Beads: func(context.Context, string) BeadStore { return store }}
	got, err := showBeads(context.Background(), env, "/town", ids)
	if err != nil {
		t.Fatalf("showBeads: %v", err)
	}

	wantChunks := []int{64, 64, 2}
	if len(store.chunks) != len(wantChunks) {
		t.Fatalf("the store read %d chunk(s) %v, want %d", len(store.chunks), store.chunks, len(wantChunks))
	}
	seen := 0
	for i, chunk := range store.chunks {
		if len(chunk) != wantChunks[i] {
			t.Errorf("chunk %d holds %d id(s), want %d", i, len(chunk), wantChunks[i])
		}
		for _, id := range chunk {
			if id == "" || strings.HasPrefix(id, "-") {
				t.Errorf("the store was asked for %q, which is not a bead id", id)
			}
			if got[id] == nil {
				t.Errorf("showBeads answered no bead for %q", id)
			}
			seen++
		}
	}
	if seen != 130 || len(got) != 130 {
		t.Errorf("the store read %d id(s) and showBeads answered %d, want 130", seen, len(got))
	}
}
