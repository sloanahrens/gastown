package seatship

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestSeatShipOutbox(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cap     int64
		absent  bool                   // the outbox directory does not exist at all
		files   map[string]string      // plain files written into the outbox
		special func(dir string) error // an entry a plain write cannot make
		want    map[string]string
		wantErr string // a fragment the error must carry
	}{
		{"both files, notes at the cap", 5, false,
			map[string]string{"result.json": "{}", "notes.md": "notes"}, nil,
			map[string]string{"result.json": "{}", "notes.md": "notes"}, ""},
		{"one absent", 1 << 10, false, map[string]string{"notes.md": "notes"}, nil,
			map[string]string{"notes.md": "notes"}, ""},
		{"over the cap", 5, false, map[string]string{"notes.md": "123456"}, nil,
			nil, "notes.md is larger than the 5-byte cap"},
		{"no directory", 1 << 10, true, nil, nil, map[string]string{}, ""},
		{"an empty directory", 1 << 10, false, nil, nil, map[string]string{}, ""},
		{"negative cap", -1, false, map[string]string{"notes.md": "notes"}, nil,
			nil, "cap -1 is negative"},
		{"a symlink in place of a file", 1 << 10, false, map[string]string{"notes.md": "notes"},
			func(dir string) error { return os.Symlink("notes.md", filepath.Join(dir, "result.json")) }, nil, "result.json is not a regular file"},
		{"a FIFO in place of a file", 1 << 10, false, nil,
			func(dir string) error { return syscall.Mkfifo(filepath.Join(dir, "result.json"), 0o600) }, nil, "result.json is not a regular file"},
		{"a directory in place of a file", 1 << 10, false, nil,
			func(dir string) error { return os.Mkdir(filepath.Join(dir, "result.json"), 0o755) }, nil, "result.json is not a regular file"},
		{"a file the process cannot read", 1 << 10, false, nil,
			func(dir string) error { return os.WriteFile(filepath.Join(dir, "result.json"), []byte("{}"), 0o000) }, nil, "result.json"},
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
			dir := filepath.Join(t.TempDir(), "outbox")
			if !tt.absent {
				must(os.MkdirAll(dir, 0o755))
			}
			for name, body := range tt.files {
				must(os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
			}
			if tt.special != nil {
				must(tt.special(dir))
			}
			got, err := OutboxFrom(dir, tt.cap)
			switch {
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("OutboxFrom = %v, %v; want an error naming %s", got, err, tt.wantErr)
			case tt.wantErr == "" && (err != nil || !reflect.DeepEqual(got, tt.want)):
				t.Fatalf("OutboxFrom = %v, %v; want %v, nil", got, err, tt.want)
			}
		})
	}
}
