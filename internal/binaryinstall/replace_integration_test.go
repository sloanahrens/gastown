//go:build integration

package binaryinstall

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/installlock"
)

// immutableSeams returns the immutable-flag seams a replace runs with on this
// host, and whether they are the real syscalls. The real flag needs
// CAP_LINUX_IMMUTABLE on Linux (or chflags on macOS), which an unprivileged
// host does not grant; there the pair is a stateful fake and the rename refuses
// an immutable destination itself, so the case the fakes used to hide is still
// exercised. The real pair is only handed out after a probe file took it.
func immutableSeams(t *testing.T) (clear, set func(string) error, rename func(string, string) error, supported bool) {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	if err := setImmutable(probe); err != nil {
		var mu sync.Mutex
		flags := map[string]bool{}
		clear = func(path string) error {
			mu.Lock()
			defer mu.Unlock()
			flags[path] = false
			return nil
		}
		set = func(path string) error {
			mu.Lock()
			defer mu.Unlock()
			flags[path] = true
			return nil
		}
		rename = func(oldpath, newpath string) error {
			mu.Lock()
			immutable := flags[newpath]
			mu.Unlock()
			if immutable {
				return fmt.Errorf("rename %s: destination is immutable", newpath)
			}
			if err := os.Rename(oldpath, newpath); err != nil {
				return err
			}
			mu.Lock()
			flags[newpath] = flags[oldpath]
			delete(flags, oldpath)
			mu.Unlock()
			return nil
		}
		return clear, set, rename, false
	}
	t.Cleanup(func() { _ = clearImmutable(probe) })
	return clearImmutable, setImmutable, os.Rename, true
}

// TestIntegrationConcurrentReadersNeverSeeAPartialBinary installs over a live
// target on a real directory while a reader reads it in a loop, and then has
// two installers replace the same target at once. It is the property the unit
// tests cannot show: rename(2) inside a directory publishes a whole file, so a
// reader sees an old complete binary or a new complete one, never a partial
// copy (gt-0het). The per-target lock means the loser of a concurrent pair is
// refused with ErrReplaceInProgress rather than sharing the critical section
// (gt-tpg0p.66).
func TestIntegrationConcurrentReadersNeverSeeAPartialBinary(t *testing.T) {
	dir := t.TempDir()
	destDir := filepath.Join(dir, "bin")
	dest := filepath.Join(destDir, "gt")

	old := bytes.Repeat([]byte("o"), 64*1024)
	first := bytes.Repeat([]byte("a"), 96*1024)
	second := bytes.Repeat([]byte("b"), 128*1024)
	srcOld := writeSource(t, dir, "gt-old", old)
	srcFirst := writeSource(t, dir, "gt-first", first)
	srcSecond := writeSource(t, dir, "gt-second", second)

	clear, set, rename, _ := immutableSeams(t)
	installer := Installer{Files: Files{
		ClearImmutable: clear,
		SetImmutable:   set,
		Rename:         rename,
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

	// Hold the target's real lock as a peer install would: the replace must be
	// refused at once, not wait for it and not interlace with it.
	lockPath := filepath.Join(destDir, ".gt.replace.lock")
	peer, err := installlock.Acquire(lockPath, 0)
	if err != nil {
		t.Fatalf("take the peer lock %s: %v", lockPath, err)
	}
	if err := installer.Replace(srcFirst, destDir, "gt"); !errors.Is(err, ErrReplaceInProgress) {
		peer()
		t.Fatalf("Replace while a peer holds the lock: %v, want ErrReplaceInProgress", err)
	}
	peer()

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
		if err != nil && !errors.Is(err, ErrReplaceInProgress) {
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
	names, err := entryNames(destDir)
	if err != nil {
		t.Fatalf("read %s: %v", destDir, err)
	}
	if strings.Join(names, ",") != ".gt.replace.lock,gt" {
		t.Fatalf("%s holds %v, want only the installed binary and its lock file", destDir, names)
	}
}

// TestIntegrationReplaceWithTheRealImmutableFlag replaces an immutable target
// with the real flag syscalls, which is the platform behaviour the no-op fakes
// hid. A host the kernel does not grant the flag to skips with the reason.
func TestIntegrationReplaceWithTheRealImmutableFlag(t *testing.T) {
	_, _, _, supported := immutableSeams(t)
	if !supported {
		t.Skip("the kernel refuses the immutable flag (no CAP_LINUX_IMMUTABLE, or no chflags/chattr), so the real flag cannot be exercised here")
	}
	dir := t.TempDir()
	destDir := filepath.Join(dir, "bin")
	dest := filepath.Join(destDir, "gt")
	t.Cleanup(func() { _ = clearImmutable(dest) })

	first := writeSource(t, dir, "gt-first", []byte("first binary"))
	second := writeSource(t, dir, "gt-second", []byte("second binary"))
	installer := Installer{}
	if err := installer.Replace(first, destDir, "gt"); err != nil {
		t.Fatalf("install the first binary: %v", err)
	}
	if err := installer.Replace(second, destDir, "gt"); err != nil {
		t.Fatalf("replace an immutable target: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read the installed binary: %v", err)
	}
	if !bytes.Equal(got, []byte("second binary")) {
		t.Fatalf("installed %q, want the second binary", got)
	}
	// The flag is back on: an immutable file cannot be removed.
	if err := os.Remove(dest); err == nil {
		t.Fatal("the installed binary could be removed, so the immutable flag is not back on it")
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
