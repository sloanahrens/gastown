// Package binaryinstall replaces an installed binary atomically: the new bytes
// land in a uniquely named temp file beside the target and are published with
// rename(2), so a concurrent reader sees either the complete old binary or the
// complete new one, never a partial copy (gt-0het).
//
// Replaces of one target are serialized on a lock file beside it, so two
// installs cannot interleave their flag clears, copies and renames.
//
// internal/installer/binary.go reads the commit an installed binary was built
// from and offers nothing a replace needs, and the copy and immutable-flag
// helpers beside it in that package's Files seam are unexported, so this
// package carries its own with the same shape. The per-target lock reuses
// internal/installlock, the flock the Go installer already holds (gt-tpg0p.66).
package binaryinstall

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/installlock"
)

// ErrSourceNotFound is a replace whose source binary is not a regular file,
// the shell's `[ ! -f "$src" ]` refusal (gt-tpg0p.66).
var ErrSourceNotFound = errors.New("source binary not found")

// ErrReplaceInProgress reports that another replace holds the target's lock.
// Replace does not wait for a peer; the refused install is the caller's to
// retry (gt-tpg0p.66).
var ErrReplaceInProgress = errors.New("another install is in progress")

// Files is the filesystem a replace touches. Every field is a seam with the
// real operation behind a nil one, so a test scripts the failing rename and the
// refused flag restore the real filesystem will not produce on demand.
type Files struct {
	// Mode reports the permissions of path: the mode the installed copy takes.
	Mode func(path string) (os.FileMode, error)
	// IsFile reports whether path is a regular file.
	IsFile func(path string) bool
	// MkdirAll creates dir and its parents, mkdir -p.
	MkdirAll func(path string) error
	// TempFile creates an empty file in dir and returns its path.
	TempFile func(dir, pattern string) (string, error)
	// Copy writes src's bytes to dst, leaving dst's mode alone.
	Copy func(dst, src string) error
	// Chmod sets path's permissions.
	Chmod func(path string, perm os.FileMode) error
	// Rename replaces newpath with oldpath atomically, mv -f.
	Rename func(oldpath, newpath string) error
	// Remove deletes path.
	Remove func(path string) error
	// ClearImmutable strips the OS-immutable flag from path. An error means
	// the flag could not be touched at all, which is the answer on a platform
	// that never grants it (chattr -i without CAP_LINUX_IMMUTABLE).
	ClearImmutable func(path string) error
	// SetImmutable puts the flag back on path.
	SetImmutable func(path string) error
	// Lock takes path's exclusive lock without waiting and returns its release.
	// It returns installlock.ErrBusy when another holder has the lock, and any
	// other error when the lock could not be taken at all.
	Lock func(path string) (release func(), err error)
}

// Installer replaces an installed binary through its filesystem seam.
type Installer struct {
	Files Files
}

// Replace installs src as destDir/name, replacing a binary already there
// atomically; an empty name takes src's base name.
//
// One replace of a target holds its lock from before the copy until the
// immutable flag is back, and a second replace that finds it held returns
// ErrReplaceInProgress rather than interlacing with the first (gt-tpg0p.66).
func (i Installer) Replace(src, destDir, name string) (err error) {
	f := i.Files
	if name == "" {
		name = filepath.Base(src)
	}
	if !bareName(name) {
		return fmt.Errorf("binaryinstall: destination name %q is not a bare file name", name)
	}
	if !f.isFile(src) {
		return fmt.Errorf("binaryinstall: %w: %s", ErrSourceNotFound, src)
	}
	// The mode follows the built artifact rather than a fixed 0755, so an
	// artifact built otherwise installs as it is; go build's own 0755 is the
	// fallback when the mode cannot be read, as the script's failed
	// `chmod --reference` falls back (gt-tpg0p.66).
	perm := os.FileMode(0o755)
	if mode, modeErr := f.mode(src); modeErr == nil {
		perm = mode
	}
	if mkErr := f.mkdirAll(destDir); mkErr != nil {
		return fmt.Errorf("binaryinstall: create %s: %w", destDir, mkErr)
	}
	dest := filepath.Join(destDir, name)
	// The lock covers the whole mutation below: the flag clear, the copy, the
	// rename and the flag restore. Two replaces that interleaved there could
	// have one restore the flag while the other is about to rename onto an
	// immutable target, which the platform refuses. The lock is taken without
	// waiting: a peer's install is under way, and this one is refused rather
	// than queued behind it (gt-tpg0p.66).
	lockPath := filepath.Join(destDir, "."+name+".replace.lock")
	release, lockErr := f.lock(lockPath)
	if errors.Is(lockErr, installlock.ErrBusy) {
		return fmt.Errorf("binaryinstall: %s: %w", name, ErrReplaceInProgress)
	}
	if lockErr != nil {
		return fmt.Errorf("binaryinstall: lock %s: %w", lockPath, lockErr)
	}
	defer release()
	// The temp file sits in destDir, never in the system temp directory: a
	// rename across filesystems is a copy, and a copy is not atomic. Its name
	// is unique so two installs cannot rename each other's half-written copy
	// into place even when they name different targets in one directory.
	tmp, err := f.tempFile(destDir, "."+name+".tmp.*")
	if err != nil {
		return fmt.Errorf("binaryinstall: create temp file in %s: %w", destDir, err)
	}
	if copyErr := f.copy(tmp, src); copyErr != nil {
		_ = f.remove(tmp)
		return fmt.Errorf("binaryinstall: copy %s to %s: %w", src, tmp, copyErr)
	}
	if chmodErr := f.chmod(tmp, perm); chmodErr != nil {
		_ = f.remove(tmp)
		return fmt.Errorf("binaryinstall: set mode %#o on %s: %w", perm, tmp, chmodErr)
	}
	// The flag comes off here, at the rename it would block, and not a moment
	// sooner, so the live binary is writable for no longer than the replace
	// (gt-tqxdd).
	clearErr := f.clearImmutable(dest)
	// ... and it goes back on whichever way the rename ends. Restoring only
	// after a successful rename left a failed replace with a writable binary,
	// the window gt-vya0s exists to close. A restore that fails after a clear
	// that failed too is the platform refusing the flag outright (chattr
	// without CAP_LINUX_IMMUTABLE): nothing came off, so nothing is lost, and
	// the install must not fail for a protection the platform will not grant.
	// Every other failed restore is reported, because then the flag was there
	// and is now gone.
	defer func() {
		if setErr := f.setImmutable(dest); setErr != nil && clearErr == nil {
			err = errors.Join(err, fmt.Errorf("binaryinstall: restore immutable flag on %s: %w", dest, setErr))
		}
	}()
	if renameErr := f.rename(tmp, dest); renameErr != nil {
		_ = f.remove(tmp)
		return fmt.Errorf("binaryinstall: rename %s over %s: %w", tmp, dest, renameErr)
	}
	return nil
}

// bareName reports whether name names a file inside destDir and nothing else.
// A name with a separator in it would install the binary somewhere the caller
// did not name.
func bareName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		filepath.Base(name) == name && !strings.ContainsRune(name, os.PathSeparator)
}

func (f Files) mode(path string) (os.FileMode, error) {
	if f.Mode != nil {
		return f.Mode(path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Mode().Perm(), nil
}

func (f Files) isFile(path string) bool {
	if f.IsFile != nil {
		return f.IsFile(path)
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func (f Files) mkdirAll(path string) error {
	if f.MkdirAll != nil {
		return f.MkdirAll(path)
	}
	return os.MkdirAll(path, 0o755)
}

func (f Files) tempFile(dir, pattern string) (string, error) {
	if f.TempFile != nil {
		return f.TempFile(dir, pattern)
	}
	return createTempFile(dir, pattern)
}

func (f Files) copy(dst, src string) error {
	if f.Copy != nil {
		return f.Copy(dst, src)
	}
	return copyFile(dst, src)
}

func (f Files) chmod(path string, perm os.FileMode) error {
	if f.Chmod != nil {
		return f.Chmod(path, perm)
	}
	return os.Chmod(path, perm)
}

func (f Files) rename(oldpath, newpath string) error {
	if f.Rename != nil {
		return f.Rename(oldpath, newpath)
	}
	return os.Rename(oldpath, newpath)
}

func (f Files) remove(path string) error {
	if f.Remove != nil {
		return f.Remove(path)
	}
	return os.Remove(path)
}

func (f Files) clearImmutable(path string) error {
	if f.ClearImmutable != nil {
		return f.ClearImmutable(path)
	}
	return clearImmutable(path)
}

func (f Files) setImmutable(path string) error {
	if f.SetImmutable != nil {
		return f.SetImmutable(path)
	}
	return setImmutable(path)
}

// lock takes the per-target lock. A zero wait makes the real take
// non-blocking, so a peer that holds it answers ErrBusy at once instead of
// making this install wait.
func (f Files) lock(path string) (func(), error) {
	if f.Lock != nil {
		return f.Lock(path)
	}
	return installlock.Acquire(path, 0)
}

// createTempFile is mktemp: the pattern decides the name, the directory decides
// the filesystem the rename stays on. A close that fails is not the step's
// failure: the copy that follows reopens the file and reports its own error.
func createTempFile(dir, pattern string) (string, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	_ = file.Close()
	return file.Name(), nil
}

// copyFile streams src into dst and leaves dst's mode to the caller, as `cp`
// into an existing file does. It streams rather than reading the whole
// artifact, which for gt is over a hundred megabytes.
func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// clearImmutable strips the flag on macOS (chflags) and Linux (chattr). A
// platform that grants neither reports the failure, and the caller treats it as
// "there was nothing to take off" rather than as a failed install, because the
// rename refuses an immutable destination with an error of its own
// (gt-vya0s).
func clearImmutable(path string) error {
	if err := exec.Command("chflags", "nouchg", path).Run(); err == nil {
		return nil
	}
	return exec.Command("chattr", "-i", path).Run()
}

// setImmutable restores the flag, macOS first, as the script's set_immutable
// does.
func setImmutable(path string) error {
	if err := exec.Command("chflags", "uchg", path).Run(); err == nil {
		return nil
	}
	return exec.Command("chattr", "+i", path).Run()
}
