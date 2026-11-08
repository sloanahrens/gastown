package cloudcheck

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestJSONWrittenAtomically(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "status.json")
	if err := Write(path, Report{At: 1, OK: 1, Items: []Item{}}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// first.json is a second name for the status file as it stands now.
	first := filepath.Join(filepath.Dir(path), "first.json")
	if err := os.Link(path, first); err != nil {
		t.Fatalf("linking the first status: %v", err)
	}

	// A status wide enough that a truncate-and-write in place leaves a window
	// a reader can land in.
	next := Report{At: 2, OK: 2, Items: []Item{}}
	for i := 0; i < 1024; i++ {
		next.Items = append(next.Items, Item{Level: LevelFail, Text: "a failure line wide enough to leave a partial read when a write truncates the file in place"})
	}

	done := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		for {
			select {
			case <-done:
				errc <- nil
				return
			default:
			}
			b, err := os.ReadFile(path)
			if err != nil {
				errc <- err
				return
			}
			var got Report
			if err := json.Unmarshal(b, &got); err != nil {
				errc <- fmt.Errorf("a reader saw a partial status: %w", err)
				return
			}
		}
	}()
	var writeErr error
	for i := 0; i < 20 && writeErr == nil; i++ {
		writeErr = Write(path, next)
	}
	close(done)
	if err := <-errc; err != nil {
		t.Error(err)
	}
	if writeErr != nil {
		t.Fatalf("Write: %v", writeErr)
	}

	// A second name for the first status still holds it: the write replaced
	// the file instead of rewriting it in place, where both names would
	// change.
	keptBytes, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	var kept Report
	if err := json.Unmarshal(keptBytes, &kept); err != nil {
		t.Fatalf("the linked first status does not parse: %v", err)
	}
	if kept.At != 1 {
		t.Errorf("the linked first status is At %d, want 1: the write rewrote the file in place", kept.At)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Report
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("the written status does not parse: %v", err)
	}
	if got.At != next.At || len(got.Items) != len(next.Items) {
		t.Errorf("written status = At %d with %d items, want At %d with %d", got.At, len(got.Items), next.At, len(next.Items))
	}
}
