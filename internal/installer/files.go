package installer

import (
	"bytes"
	"os"
	"os/exec"
)

// Files is the filesystem one run touches: the reads of the daemon's commit,
// the copy and rename that move the installed binary aside, and the copy and
// rename that put it back. Every field is a seam with the real operation behind
// a nil one, so a test sets only the calls it watches and the install's own
// move can be exercised on a temporary directory, never the real install one
// (gt-tpg0p.10).
type Files struct {
	// Copy writes src's bytes and mode to dst.
	Copy func(dst, src string) error
	// Rename replaces newpath with oldpath atomically, the script's mv -f.
	Rename    func(oldpath, newpath string) error
	Remove    func(path string) error
	ReadFile  func(path string) ([]byte, error)
	WriteFile func(path string, data []byte, perm os.FileMode) error
	// MkdirAll creates path and its parents, the script's mkdir -p.
	MkdirAll func(path string) error
	IsFile   func(path string) bool
	Exists   func(path string) bool
	// IsExecutable reports whether path is a file someone can run, the
	// script's -x test on the binary the container-gate slot is taken with.
	IsExecutable func(path string) bool
	// TempFile creates an empty file in dir and returns its path, the script's
	// mktemp: signing runs codesign against a copy beside the binary, never
	// against the live path.
	TempFile func(dir, pattern string) (string, error)
	// ClearImmutable and SetImmutable bracket a rename over the installed
	// binary, which an install can leave OS-immutable; both are best effort
	// because a platform without the flag has nothing to do (gt-vya0s).
	ClearImmutable func(path string)
	SetImmutable   func(path string)
}

func (f Files) copy(dst, src string) error {
	if f.Copy != nil {
		return f.Copy(dst, src)
	}
	return copyFile(dst, src)
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

func (f Files) readFile(path string) ([]byte, error) {
	if f.ReadFile != nil {
		return f.ReadFile(path)
	}
	return os.ReadFile(path)
}

func (f Files) writeFile(path string, data []byte, perm os.FileMode) error {
	if f.WriteFile != nil {
		return f.WriteFile(path, data, perm)
	}
	return os.WriteFile(path, data, perm)
}

func (f Files) mkdirAll(path string) error {
	if f.MkdirAll != nil {
		return f.MkdirAll(path)
	}
	return os.MkdirAll(path, 0o755)
}

func (f Files) isFile(path string) bool {
	if f.IsFile != nil {
		return f.IsFile(path)
	}
	return isRegularFile(path)
}

func (f Files) exists(path string) bool {
	if f.Exists != nil {
		return f.Exists(path)
	}
	return pathExists(path)
}

func (f Files) isExecutable(path string) bool {
	if f.IsExecutable != nil {
		return f.IsExecutable(path)
	}
	return isExecutable(path)
}

func (f Files) tempFile(dir, pattern string) (string, error) {
	if f.TempFile != nil {
		return f.TempFile(dir, pattern)
	}
	return createTempFile(dir, pattern)
}

func (f Files) clearImmutable(path string) {
	if f.ClearImmutable != nil {
		f.ClearImmutable(path)
		return
	}
	clearImmutable(path)
}

func (f Files) setImmutable(path string) {
	if f.SetImmutable != nil {
		f.SetImmutable(path)
		return
	}
	setImmutable(path)
}

// sameContent is cmp -s: a file either cannot be read or differs, and both are
// read as a difference, so a broken previous binary still gets restored.
func (f Files) sameContent(a, b string) bool {
	ab, err := f.readFile(a)
	if err != nil {
		return false
	}
	bb, err := f.readFile(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// copyFile copies contents and mode, which is all a rollback needs. Unlike cp
// -p it copies no platform flag, so a copy of an immutable binary is writable
// before the flag is re-applied to the installed path (gt-vya0s).
func copyFile(dst, src string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, info.Mode().Perm())
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// createTempFile is mktemp: an empty file in dir whose name the pattern
// decides, closed before it is handed back.
func createTempFile(dir, pattern string) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// clearImmutable strips the OS-immutable flag on macos (chflags) and Linux
// (chattr). A clear that cannot happen leaves the file as protected as it was.
func clearImmutable(path string) {
	_ = exec.Command("chflags", "nouchg", path).Run()
	_ = exec.Command("chattr", "-i", path).Run()
}

// setImmutable restores the flag clearImmutable took off, macos first, as the
// script's set_immutable does.
func setImmutable(path string) {
	if exec.Command("chflags", "uchg", path).Run() == nil {
		return
	}
	_ = exec.Command("chattr", "+i", path).Run()
}
