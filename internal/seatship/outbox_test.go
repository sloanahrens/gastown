package seatship

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// escapeSymlink points result.json at a secret beside the outbox by a relative link leaving it.
func escapeSymlink(base, dir string) error {
	if err := os.WriteFile(filepath.Join(base, "secret"), []byte("{}"), 0o644); err != nil {
		return err
	}
	return os.Symlink(filepath.Join("..", "secret"), filepath.Join(dir, "result.json"))
}

// outboxBounded runs OutboxFrom and fails the row if it does not return in
// time, so a regression that blocks on a FIFO fails rather than hangs the package.
func outboxBounded(t *testing.T, dir string, cap int64) (map[string]string, error) {
	t.Helper()
	type result struct {
		files map[string]string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		files, err := OutboxFrom(dir, cap)
		done <- result{files, err}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case r := <-done:
		return r.files, r.err
	case <-ctx.Done():
		t.Fatal("OutboxFrom did not return within the test's bound")
		return nil, nil
	}
}

func TestSeatShipOutbox(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cap     int64
		absent  bool                         // the outbox directory does not exist at all
		dirFile bool                         // a regular file sits where the outbox directory goes
		files   map[string]string            // plain files written into the outbox
		special func(base, dir string) error // an entry a plain write cannot make
		want    map[string]string
		wantErr string // a fragment the error must carry
	}{
		{"both files, notes at the cap", 5, false, false, map[string]string{"result.json": "{}", "notes.md": "notes"}, nil, map[string]string{"result.json": "{}", "notes.md": "notes"}, ""},
		{"one absent", 1 << 10, false, false, map[string]string{"notes.md": "notes"}, nil, map[string]string{"notes.md": "notes"}, ""},
		{"over the cap", 5, false, false, map[string]string{"notes.md": "123456"}, nil, nil, "notes.md: is larger than the 5-byte cap"},
		{"no directory", 1 << 10, true, false, nil, nil, map[string]string{}, ""},
		{"an empty directory", 1 << 10, false, false, nil, nil, map[string]string{}, ""},
		{"negative cap", -1, false, false, map[string]string{"notes.md": "notes"}, nil, nil, "cap -1 is negative"},
		{"the largest cap", math.MaxInt64, false, false, map[string]string{"notes.md": "notes"}, nil, map[string]string{"notes.md": "notes"}, ""},
		{"a symlink leaving the outbox", 1 << 10, false, false, nil, escapeSymlink, nil, "outbox result.json"},
		{"a FIFO in place of a file", 1 << 10, false, false, nil, func(_, d string) error { return syscall.Mkfifo(filepath.Join(d, "result.json"), 0o600) }, nil, "result.json: is not a regular file"},
		{"a directory in place of a file", 1 << 10, false, false, nil, func(_, d string) error { return os.Mkdir(filepath.Join(d, "result.json"), 0o755) }, nil, "result.json: is not a regular file"},
		{"a file where the outbox directory goes", 1 << 10, false, true, nil, nil, nil, "open outbox"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			base := t.TempDir()
			dir := filepath.Join(base, "outbox")
			if tt.dirFile {
				must(os.WriteFile(dir, []byte("x"), 0o644))
			} else if !tt.absent {
				must(os.MkdirAll(dir, 0o755))
			}
			for name, body := range tt.files {
				must(os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
			}
			if tt.special != nil {
				must(tt.special(base, dir))
			}
			got, err := outboxBounded(t, dir, tt.cap)
			switch {
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("OutboxFrom = %v, %v; want an error naming %s", got, err, tt.wantErr)
			case tt.wantErr == "" && (err != nil || !reflect.DeepEqual(got, tt.want)):
				t.Fatalf("OutboxFrom = %v, %v; want %v, nil", got, err, tt.want)
			}
		})
	}
}
