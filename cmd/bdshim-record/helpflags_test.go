package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helpBD answers bd's help for the arguments a test names.
type helpBD map[string]string

func (f helpBD) run(dir string, args []string) ([]byte, int, error) {
	if out, ok := f[strings.Join(args, " ")]; ok {
		return []byte(out), 0, nil
	}
	return []byte("bd: no such command"), 1, nil
}

// oneCommand is the smallest bd whose help records: a version and a root
// command with one flag.
var oneCommand = helpBD{
	"version": "bd version 1.2.2 (dev) schema<=66\n",
	"--help": `Usage:
  bd [flags]

Flags:
      --json   Output in JSON format
`,
}

// TestRecordFlagsCheck: -check reports whether the recorded file still matches
// bd's help and writes nothing, so a bd upgrade has a command that names it.
func TestRecordFlagsCheck(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bd-flags.txt")
	var stderr bytes.Buffer
	if code := recordFlags(options{out: path}, oneCommand.run, &stderr); code != 0 {
		t.Fatalf("recordFlags = %d, want 0: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "wrote "+path) {
		t.Errorf("stderr = %q, want the file it wrote named", stderr.String())
	}

	stderr.Reset()
	if code := recordFlags(options{out: path, check: true}, oneCommand.run, &stderr); code != 0 {
		t.Fatalf("recordFlags -check on its own output = %d, want 0: %s", code, stderr.String())
	}

	stale := []byte("value\tcreate\t--title\n")
	if err := os.WriteFile(path, stale, 0o644); err != nil {
		t.Fatalf("writing a stale recording: %v", err)
	}
	stderr.Reset()
	if code := recordFlags(options{out: path, check: true}, oneCommand.run, &stderr); code != 1 {
		t.Fatalf("recordFlags -check on a stale file = %d, want 1: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "differs from bd's help") {
		t.Errorf("stderr = %q, want the drift named", stderr.String())
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, stale) {
		t.Error("-check rewrote the file, want it left untouched")
	}
}
