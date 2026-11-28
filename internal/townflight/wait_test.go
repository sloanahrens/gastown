package townflight

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

// TestWaitAll checks Wait closes done only after every held command has ended,
// with none in flight already done, and never on a failed wait or read error.
func TestWaitAll(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	held := func(t *testing.T, label string) *entry {
		e, err := createEntry(inflightDir(townRoot), label)
		if err != nil {
			t.Fatalf("createEntry: %v", err)
		}
		if err := os.Rename(e.path, e.path+".cmd"); err != nil {
			t.Fatalf("listing entry: %v", err)
		}
		e.path += ".cmd"
		t.Cleanup(func() { _ = e.drop() })
		return e
	}

	// None in flight: done is closed before Wait returns.
	labels, done, err := Wait(t.TempDir())
	if err != nil || len(labels) != 0 {
		t.Fatalf("Wait with none in flight = %v, %v; want no labels and no error", labels, err)
	}
	select {
	case <-done:
	default:
		t.Fatal("done is open with no command in flight")
	}

	// Two held: done stays open after the first ends and closes after both.
	first, second := held(t, "gt status"), held(t, "gt sling")
	ended := make(chan struct{}, 2)
	labels, done, err = wait(townRoot, func(path string) error {
		err := waitEnded(path)
		ended <- struct{}{}
		return err
	})
	if err != nil || !slices.Contains(labels, "gt status") || !slices.Contains(labels, "gt sling") {
		t.Fatalf("wait = %v, %v; want both labels and no error", labels, err)
	}
	if err := first.drop(); err != nil {
		t.Fatalf("drop: %v", err)
	}
	guard := time.After(10 * time.Second) //testpolicy:allow no-sleep — a bound on a hang, not a settle
	select {
	case <-ended:
	case <-guard:
		t.Fatal("the wait for the released entry did not return")
	}
	select {
	case <-done:
		t.Fatal("done closed while the second entry is still held")
	default:
	}
	if err := second.drop(); err != nil {
		t.Fatalf("drop: %v", err)
	}
	select {
	case <-done:
	case <-guard:
		t.Fatal("done did not close after both entries ended")
	}

	// A failed wait never confirms its command ended, so done stays open.
	synctest.Test(t, func(t *testing.T) {
		held(t, "gt status")
		_, done, err := wait(townRoot, func(string) error { return errors.New("boom") })
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("a failed wait was reported as ended")
		default:
		}
	})

	// A listing or probe error is returned with a nil channel.
	blocked := t.TempDir()
	if err := os.WriteFile(filepath.Join(blocked, "daemon"), nil, 0o600); err != nil {
		t.Fatalf("writing blocker: %v", err)
	}
	dir := inflightDir(townRoot)
	if err := os.Symlink(filepath.Join(dir, "missing", "entry"), filepath.Join(dir, "broken.cmd")); err != nil {
		t.Fatalf("making unopenable entry: %v", err)
	}
	for name, root := range map[string]string{"listing": blocked, "probe": townRoot} {
		if labels, done, err := Wait(root); err == nil || done != nil || labels != nil {
			t.Errorf("%s error: Wait = %v, %v, %v; want nil, nil and an error", name, labels, done, err)
		}
	}
}
