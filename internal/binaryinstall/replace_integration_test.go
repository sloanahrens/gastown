//go:build integration

package binaryinstall

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestIntegrationReplaceNeverShowsAPartialBinary installs over a live target on
// a real directory while a reader reads it in a loop, and then has two
// installers replace the same target at once. It is the one property the unit
// tests cannot show: rename(2) inside a directory publishes a whole file, so a
// reader sees an old complete binary or a new complete one, never a partial
// copy (gt-0het).
func TestIntegrationReplaceNeverShowsAPartialBinary(t *testing.T) {
	dir := t.TempDir()
	destDir := filepath.Join(dir, "bin")
	dest := filepath.Join(destDir, "gt")

	old := bytes.Repeat([]byte("o"), 64*1024)
	first := bytes.Repeat([]byte("a"), 96*1024)
	second := bytes.Repeat([]byte("b"), 128*1024)
	srcOld := writeSource(t, dir, "gt-old", old)
	srcFirst := writeSource(t, dir, "gt-first", first)
	srcSecond := writeSource(t, dir, "gt-second", second)

	// The immutable flag is the one thing a test may not touch: setting it
	// needs a kernel capability the host may not grant, and it would block the
	// temp directory's own cleanup. Signing is a fake seam for the same reason
	// (gt-tpg0p.67).
	installer := Installer{Files: Files{
		ClearImmutable: func(string) error { return nil },
		SetImmutable:   func(string) error { return nil },
	}}
	if err := installer.Replace(srcOld, destDir, "gt"); err != nil {
		t.Fatalf("install the first binary: %v", err)
	}

	done := make(chan struct{})
	var readers sync.WaitGroup
	var readErr error
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			got, err := os.ReadFile(dest)
			if err != nil {
				readErr = fmt.Errorf("read %s: %w", dest, err)
				return
			}
			if !bytes.Equal(got, old) && !bytes.Equal(got, first) && !bytes.Equal(got, second) {
				readErr = fmt.Errorf("read %d bytes, which is no complete binary (old %d, first %d, second %d)",
					len(got), len(old), len(first), len(second))
				return
			}
		}
	}()

	replaces := 20
	for i := 0; i < replaces; i++ {
		src := srcFirst
		if i%2 == 1 {
			src = srcSecond
		}
		if err := installer.Replace(src, destDir, "gt"); err != nil {
			close(done)
			readers.Wait()
			t.Fatalf("replace %d: %v", i, err)
		}
	}
	close(done)
	readers.Wait()
	if readErr != nil {
		t.Fatal(readErr)
	}

	var installers sync.WaitGroup
	errs := make([]error, 2)
	for i, src := range []string{srcFirst, srcSecond} {
		installers.Add(1)
		go func(i int, src string) {
			defer installers.Done()
			errs[i] = installer.Replace(src, destDir, "gt")
		}(i, src)
	}
	installers.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent replace %d: %v", i, err)
		}
	}

	final, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read the installed binary: %v", err)
	}
	if !bytes.Equal(final, first) && !bytes.Equal(final, second) {
		t.Fatalf("the target holds %d bytes, which is neither installer's complete binary", len(final))
	}
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("read %s: %v", destDir, err)
	}
	if len(entries) != 1 || entries[0].Name() != "gt" {
		names := make([]string, len(entries))
		for i, entry := range entries {
			names[i] = entry.Name()
		}
		t.Fatalf("%s holds %v, want only the installed binary", destDir, names)
	}
}

// writeSource is a built binary: a file of data, executable, outside the
// install directory.
func writeSource(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
