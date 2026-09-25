package krc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/events"
)

func eventLine(t *testing.T, ts time.Time, typ, actor string) string {
	t.Helper()
	data, err := json.Marshal(map[string]interface{}{
		"ts":    ts.UTC().Format(time.RFC3339),
		"type":  typ,
		"actor": actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeEventLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A prune that removes nothing must not replace the file. Every rename hands
// the path a new inode; before claude-9jq that blinded in-flight await-signal
// waits even when nothing had expired.
func TestPruneFile_NoopKeepsFileIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, events.EventsFile)
	now := time.Now()
	writeEventLines(t, path,
		eventLine(t, now.Add(-time.Hour), "mail", "a"),
		eventLine(t, now.Add(-time.Minute), "sling", "b"),
	)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(path)

	result, err := NewPruner(dir, DefaultConfig()).Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if result.EventsPruned != 0 {
		t.Fatalf("EventsPruned = %d, want 0", result.EventsPruned)
	}
	if result.BytesAfter != result.BytesBefore {
		t.Errorf("BytesAfter = %d, want unchanged %d", result.BytesAfter, result.BytesBefore)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("no-op prune replaced the events file (new inode)")
	}
	if got, _ := os.ReadFile(path); string(got) != string(content) {
		t.Errorf("no-op prune changed content:\n%s\nwant:\n%s", got, content)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("no-op prune left a temp file behind (stat err %v)", err)
	}
}

func TestPruneFile_ReplacesWhenSomethingExpires(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, events.EventsFile)
	now := time.Now()
	kept := eventLine(t, now.Add(-time.Minute), "sling", "kept")
	writeEventLines(t, path, eventLine(t, now.Add(-30*24*time.Hour), "sling", "expired"), kept)

	result, err := NewPruner(dir, DefaultConfig()).Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if result.EventsPruned != 1 {
		t.Fatalf("EventsPruned = %d, want 1", result.EventsPruned)
	}
	got, _ := os.ReadFile(path)
	if string(got) != kept+"\n" {
		t.Errorf("pruned content = %q, want %q", got, kept+"\n")
	}
}

// The pruner shares the writers' flock (<file>.lock). While a writer holds it
// the pruner must not scan or rename, so an append in progress is never lost
// between the scan and the rename.
func TestPruneFile_WaitsForWriterLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, events.EventsFile)
	now := time.Now()
	writeEventLines(t, path,
		eventLine(t, now.Add(-30*24*time.Hour), "sling", "expired"),
		eventLine(t, now.Add(-time.Minute), "sling", "kept"),
	)

	writer := flock.New(path + ".lock")
	if err := writer.Lock(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := NewPruner(dir, DefaultConfig()).Prune()
		done <- err
	}()

	select {
	case err := <-done:
		_ = writer.Unlock()
		t.Fatalf("Prune finished while a writer held the events lock (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}

	late := eventLine(t, now, "nudge", "written-under-lock")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(late + "\n")
	_ = f.Close()
	if err := writer.Unlock(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Prune: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Prune did not finish after the lock was released")
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), late) {
		t.Fatalf("line appended under the writer lock was lost:\n%s", got)
	}
	if strings.Contains(string(got), `"expired"`) {
		t.Fatalf("expired line survived:\n%s", got)
	}
}

// No append through events.LogTo is lost while the pruner rotates the file
// repeatedly underneath the writers.
func TestPruneFile_ConcurrentAppendsSurviveRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, events.EventsFile)
	cfg := DefaultConfig()
	// Every "sling" is already past a negative TTL, so each seed below expires
	// and every pass really rotates; the "mail" lines the writers append keep
	// the default 30 days.
	cfg.TTLs["sling"] = -time.Nanosecond

	const writers, perWriter = 4, 50
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var pruneErr error
	var pruneWG sync.WaitGroup
	pruneWG.Add(1)
	go func() {
		defer pruneWG.Done()
		p := NewPruner(dir, cfg)
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Seed an expired line so every pass really rotates.
			if err := events.LogTo(dir, "sling", "seed", nil, events.VisibilityAudit); err != nil {
				pruneErr = err
				return
			}
			if _, err := p.Prune(); err != nil {
				pruneErr = err
				return
			}
		}
	}()
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				actor := fmt.Sprintf("w%d-%d", w, i)
				if err := events.LogTo(dir, "mail", actor, nil, events.VisibilityAudit); err != nil {
					t.Errorf("LogTo: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	pruneWG.Wait()
	if pruneErr != nil {
		t.Fatalf("pruner: %v", pruneErr)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			actor := fmt.Sprintf(`"actor":"w%d-%d"`, w, i)
			if !strings.Contains(string(data), actor) {
				t.Fatalf("append %s lost across rotation", actor)
			}
		}
	}
}
