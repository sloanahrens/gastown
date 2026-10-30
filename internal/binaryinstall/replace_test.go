package binaryinstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/installlock"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

const (
	testSrc     = "built/gt"
	testDestDir = "install-dir"
	testDest    = "install-dir/gt"
	testLock    = "install-dir/.gt.replace.lock"
)

var (
	errMkdir  = errors.New("mkdir refused")
	errTemp   = errors.New("create temp refused")
	errCopy   = errors.New("copy refused")
	errChmod  = errors.New("chmod refused")
	errRename = errors.New("rename refused (operation not permitted)")
	errSet    = errors.New("chattr +i refused (operation not permitted)")
	errClear  = errors.New("chattr -i refused (operation not permitted)")
	errLock   = errors.New("lock refused")
)

// seam is one replace's recorded seam: it logs each call where it happens and
// answers success, so a test overrides only the failure it drives. flags is
// the immutable flag of each path as the platform holds it, and the fake
// rename refuses a destination whose flag is set, the way the kernel does; a
// faked immutable flag that the rename ignored would let the concurrency bug
// through unseen (gt-tpg0p.66).
type seam struct {
	calls []string
	files Files

	flags map[string]bool

	missing   bool
	mkdirErr  error
	tempErr   error
	copyErr   error
	chmodErr  error
	renameErr error
	clearErr  error
	setErr    error
	lockErr   error
	lockBusy  bool

	mode    os.FileMode
	modeErr error

	perm     os.FileMode
	tempN    int
	temps    []string
	renamed  string
	removed  []string
	locks    int
	releases int
}

func newSeam() *seam {
	s := &seam{mode: 0o755, flags: map[string]bool{}}
	s.files = Files{
		Mode: func(path string) (os.FileMode, error) {
			s.log("mode", path)
			return s.mode, s.modeErr
		},
		IsFile: func(path string) bool {
			s.log("is-file", path)
			return !s.missing
		},
		MkdirAll: func(path string) error {
			s.log("mkdir-all", path)
			return s.mkdirErr
		},
		TempFile: func(dir, pattern string) (string, error) {
			s.log("temp-file", filepath.Join(dir, pattern))
			if s.tempErr != nil {
				return "", s.tempErr
			}
			s.tempN++
			temp := filepath.Join(dir, strings.Replace(pattern, "*", strconv.Itoa(s.tempN), 1))
			s.temps = append(s.temps, temp)
			return temp, nil
		},
		Copy: func(dst, src string) error {
			s.log("copy", dst+" <- "+src)
			return s.copyErr
		},
		Chmod: func(path string, perm os.FileMode) error {
			s.log("chmod", path)
			s.perm = perm
			return s.chmodErr
		},
		Rename: func(oldpath, newpath string) error {
			s.log("rename", oldpath+" -> "+newpath)
			if s.renameErr != nil {
				return s.renameErr
			}
			if s.flags[newpath] {
				return fmt.Errorf("rename %s: destination is immutable", newpath)
			}
			s.renamed = newpath
			s.flags[newpath] = s.flags[oldpath]
			delete(s.flags, oldpath)
			return nil
		},
		Remove: func(path string) error {
			s.log("remove", path)
			s.removed = append(s.removed, path)
			return nil
		},
		ClearImmutable: func(path string) error {
			s.log("clear-immutable", path)
			if s.clearErr != nil {
				return s.clearErr
			}
			s.flags[path] = false
			return nil
		},
		SetImmutable: func(path string) error {
			s.log("set-immutable", path)
			if s.setErr != nil {
				return s.setErr
			}
			s.flags[path] = true
			return nil
		},
		Lock: func(path string) (func(), error) {
			s.log("lock", path)
			s.locks++
			if s.lockBusy {
				return nil, installlock.ErrBusy
			}
			if s.lockErr != nil {
				return nil, s.lockErr
			}
			return func() {
				s.log("release", path)
				s.releases++
			}, nil
		},
	}
	return s
}

func (s *seam) log(call, arg string) { s.calls = append(s.calls, call+" "+arg) }

func (s *seam) replace(name string) error {
	return Installer{Files: s.files}.Replace(testSrc, testDestDir, name)
}

func TestReplaceInstallsTheSourceUnderTheTargetName(t *testing.T) {
	t.Parallel()
	s := newSeam()
	if err := s.replace("gt"); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	temp := filepath.Join(testDestDir, ".gt.tmp.1")
	want := []string{
		"is-file " + testSrc,
		"mode " + testSrc,
		"mkdir-all " + testDestDir,
		"lock " + testLock,
		"temp-file " + filepath.Join(testDestDir, ".gt.tmp.*"),
		"copy " + temp + " <- " + testSrc,
		"chmod " + temp,
		"clear-immutable " + testDest,
		"rename " + temp + " -> " + testDest,
		"set-immutable " + testDest,
		"release " + testLock,
	}
	if strings.Join(s.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("call order:\n got %v\nwant %v", s.calls, want)
	}
	if s.renamed != testDest {
		t.Fatalf("renamed onto %q, want %q", s.renamed, testDest)
	}
	if len(s.removed) != 0 {
		t.Fatalf("removed %v on a successful replace", s.removed)
	}
	if s.releases != 1 {
		t.Fatalf("released the lock %d times, want once", s.releases)
	}
}

func TestReplaceTakesTheSourceBaseNameWhenTheNameIsEmpty(t *testing.T) {
	t.Parallel()
	s := newSeam()
	if err := s.replace(""); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if want := filepath.Join(testDestDir, "gt"); s.renamed != want {
		t.Fatalf("renamed onto %q, want the source's base name %q", s.renamed, want)
	}
}

// TestReplaceKeepsSourceModeAndClearsImmutableFirst is the order that keeps the
// live binary writable for no longer than the rename: the flag comes off before
// the rename and goes back after it, and the installed mode is the source's.
func TestReplaceKeepsSourceModeAndClearsImmutableFirst(t *testing.T) {
	t.Parallel()
	s := newSeam()
	s.mode = 0o641
	if err := s.replace("gt"); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if s.perm != 0o641 {
		t.Fatalf("installed with mode %#o, want the source's %#o", s.perm, 0o641)
	}
	cleared := indexOf(s.calls, "clear-immutable "+testDest)
	renamed := indexOf(s.calls, "rename "+filepath.Join(testDestDir, ".gt.tmp.1")+" -> "+testDest)
	restored := indexOf(s.calls, "set-immutable "+testDest)
	if cleared < 0 || renamed < 0 || restored < 0 {
		t.Fatalf("calls %v do not hold the flag cleared, the rename and the flag back", s.calls)
	}
	if cleared >= renamed {
		t.Fatalf("calls %v cleared the flag only at or after the rename it exists to unblock", s.calls)
	}
	if renamed >= restored {
		t.Fatalf("calls %v restored the flag before the rename, so a failed rename leaves the binary writable", s.calls)
	}
}

func TestReplaceUsesGoBuildModeWhenTheSourceModeCannotBeRead(t *testing.T) {
	t.Parallel()
	s := newSeam()
	s.modeErr = errors.New("permission denied")
	if err := s.replace("gt"); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if s.perm != 0o755 {
		t.Fatalf("installed with mode %#o, want the 0755 fallback", s.perm)
	}
}

func TestReplaceRefusesAMissingSource(t *testing.T) {
	t.Parallel()
	s := newSeam()
	s.missing = true
	err := s.replace("gt")
	if !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("Replace: %v, want an ErrSourceNotFound", err)
	}
	if !strings.Contains(err.Error(), testSrc) {
		t.Fatalf("error %q does not name the source %q", err, testSrc)
	}
	if len(s.calls) != 1 {
		t.Fatalf("a refused source still touched the filesystem: %v", s.calls)
	}
}

func TestReplaceRefusesANameThatLeavesTheDestination(t *testing.T) {
	t.Parallel()
	for _, name := range []string{".", "..", "sub/gt", "../gt", "install-dir/gt"} {
		t.Run(name, func(t *testing.T) {
			s := newSeam()
			if err := s.replace(name); err == nil {
				t.Fatalf("Replace accepted the name %q", name)
			}
			if len(s.calls) != 0 {
				t.Fatalf("a refused name still touched the filesystem: %v", s.calls)
			}
		})
	}
}

func TestReplaceReportsADestinationDirectoryItCannotCreate(t *testing.T) {
	t.Parallel()
	s := newSeam()
	s.mkdirErr = errMkdir
	err := s.replace("gt")
	if !errors.Is(err, errMkdir) {
		t.Fatalf("Replace: %v, want the mkdir error", err)
	}
	if !strings.Contains(err.Error(), testDestDir) {
		t.Fatalf("error %q does not name the directory %q", err, testDestDir)
	}
	if s.locks != 0 || len(s.temps) != 0 {
		t.Fatalf("a replace locked or wrote a temp file in a directory it could not create: %v", s.calls)
	}
}

func TestReplaceReportsATempFileItCannotCreate(t *testing.T) {
	t.Parallel()
	s := newSeam()
	s.tempErr = errTemp
	err := s.replace("gt")
	if !errors.Is(err, errTemp) {
		t.Fatalf("Replace: %v, want the temp-file error", err)
	}
	if s.renamed != "" {
		t.Fatal("a replace renamed a target it never copied")
	}
	if s.releases != 1 {
		t.Fatalf("released the lock %d times after a failed temp file, want once", s.releases)
	}
}

// TestReplaceTempIsBesideTheTargetWithAUniqueName is the one property a fake
// cannot show: the real os.CreateTemp makes a file in the target's own
// directory (so the rename stays on one filesystem) and gives each call a name
// of its own, so two installs never rename each other's half-written copy.
func TestReplaceTempIsBesideTheTargetWithAUniqueName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := writeSourceFile(t, dir)
	destDir := filepath.Join(dir, "bin")
	var temps []string
	files := Files{
		TempFile: func(dir, pattern string) (string, error) {
			temp, err := createTempFile(dir, pattern)
			temps = append(temps, temp)
			return temp, err
		},
		// The flag needs a kernel capability the test host may not grant, and
		// its real take would start an external tool the unit tier refuses.
		ClearImmutable: func(string) error { return nil },
		SetImmutable:   func(string) error { return nil },
	}
	for i := 0; i < 2; i++ {
		if err := (Installer{Files: files}).Replace(src, destDir, "gt"); err != nil {
			t.Fatalf("Replace %d: %v", i, err)
		}
	}
	dest := filepath.Join(destDir, "gt")
	if len(temps) != 2 {
		t.Fatalf("saw %d temp files, want one per replace", len(temps))
	}
	for _, temp := range temps {
		if filepath.Dir(temp) != destDir {
			t.Fatalf("temp file %q is not in the target's directory %q, so the rename could cross a filesystem", temp, destDir)
		}
		if temp == dest {
			t.Fatalf("the temp file is the target itself (%q)", temp)
		}
	}
	if temps[0] == temps[1] {
		t.Fatalf("both replaces used the temp name %q, so a concurrent install could publish the other's copy", temps[0])
	}
	// The real lock file stays beside the target: unlinking it would reopen the
	// race internal/lock documents. No temp file survives.
	names, err := entryNames(destDir)
	if err != nil {
		t.Fatalf("read destination directory: %v", err)
	}
	if strings.Join(names, ",") != ".gt.replace.lock,gt" {
		t.Fatalf("destination directory holds %v, want the installed binary and its lock file", names)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != "built binary" {
		t.Fatalf("installed %q, want the source bytes", got)
	}
}

// TestReplaceRefusesASecondInstallWhileTheFirstHoldsTheLock models the race the
// lock exists to stop: A holds the target's lock with the flag already off
// while it copies, and B, started meanwhile, is refused before it can touch the
// flag or the target. Once A's replace is done, a sequential B succeeds.
func TestReplaceRefusesASecondInstallWhileTheFirstHoldsTheLock(t *testing.T) {
	t.Parallel()
	t.Run("held", func(t *testing.T) {
		t.Parallel()
		s := newSeam()
		s.lockBusy = true
		s.flags[testDest] = false // A cleared the flag inside its critical section
		err := s.replace("gt")
		if !errors.Is(err, ErrReplaceInProgress) {
			t.Fatalf("Replace: %v, want ErrReplaceInProgress", err)
		}
		if !strings.Contains(err.Error(), "gt") {
			t.Fatalf("error %q does not name the target", err)
		}
		want := []string{"is-file " + testSrc, "mode " + testSrc, "mkdir-all " + testDestDir, "lock " + testLock}
		if strings.Join(s.calls, "\n") != strings.Join(want, "\n") {
			t.Fatalf("a refused replace went on past the lock:\n got %v\nwant %v", s.calls, want)
		}
		if set, present := s.flags[testDest]; !present || set {
			t.Fatalf("the target's flag is %v (present %v), want exactly as the holder left it (off)", set, present)
		}
	})
	t.Run("released", func(t *testing.T) {
		t.Parallel()
		s := newSeam()
		s.flags[testDest] = true // the first install put the flag back before letting go
		if err := s.replace("gt"); err != nil {
			t.Fatalf("Replace: %v", err)
		}
		if s.renamed != testDest {
			t.Fatal("the binary was not installed")
		}
		if !s.flags[testDest] {
			t.Fatal("the flag is not back on the target after the replace")
		}
		if s.releases != 1 {
			t.Fatalf("released the lock %d times, want once", s.releases)
		}
	})
}

func TestReplaceReportsALockItCannotTake(t *testing.T) {
	t.Parallel()
	s := newSeam()
	s.lockErr = errLock
	err := s.replace("gt")
	if !errors.Is(err, errLock) {
		t.Fatalf("Replace: %v, want the lock error", err)
	}
	if !strings.Contains(err.Error(), testLock) {
		t.Fatalf("error %q does not name the lock file %q", err, testLock)
	}
	if len(s.temps) != 0 {
		t.Fatalf("a replace copied a temp file without the lock: %v", s.calls)
	}
}

func TestReplaceCleansTheTempOnEveryFailure(t *testing.T) {
	t.Parallel()
	temp := filepath.Join(testDestDir, ".gt.tmp.1")
	for _, tc := range []struct {
		name string
		set  func(*seam)
		want error
	}{
		{"copy error", func(s *seam) { s.copyErr = errCopy }, errCopy},
		{"chmod error", func(s *seam) { s.chmodErr = errChmod }, errChmod},
		{"rename error", func(s *seam) { s.renameErr = errRename }, errRename},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSeam()
			tc.set(s)
			err := s.replace("gt")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Replace: %v, want %v", err, tc.want)
			}
			if len(s.removed) != 1 || s.removed[0] != temp {
				t.Fatalf("removed %v, want the temp file %s", s.removed, temp)
			}
			if s.releases != 1 {
				t.Fatalf("released the lock %d times after %s, want once", s.releases, tc.name)
			}
		})
	}
}

func TestReplaceRestoresTheImmutableFlagAfterAFailedRename(t *testing.T) {
	t.Parallel()
	s := newSeam()
	s.renameErr = errRename
	_ = s.replace("gt")
	if !s.flags[testDest] {
		t.Fatalf("the flag is not back on the target after a failed rename: %v", s.calls)
	}
}

func TestReplaceReturnsTheRestoreFailureAfterAGoodRename(t *testing.T) {
	t.Parallel()
	s := newSeam()
	s.setErr = errSet
	err := s.replace("gt")
	if !errors.Is(err, errSet) {
		t.Fatalf("Replace: %v, want the restore error", err)
	}
	if !strings.Contains(err.Error(), "immutable") || !strings.Contains(err.Error(), testDest) {
		t.Fatalf("error %q does not name the flag and the path", err)
	}
	if s.renamed != testDest {
		t.Fatal("the new binary is not in place after a failed restore")
	}
}

func TestReplaceReportsBothWhenTheRenameAndTheRestoreFail(t *testing.T) {
	t.Parallel()
	s := newSeam()
	s.renameErr = errRename
	s.setErr = errSet
	err := s.replace("gt")
	if !errors.Is(err, errRename) || !errors.Is(err, errSet) {
		t.Fatalf("Replace: %v, want both the rename and the restore error", err)
	}
}

func TestReplaceToleratesRestoreFailureWhenTheFlagCouldNotComeOff(t *testing.T) {
	t.Parallel()
	// Without CAP_LINUX_IMMUTABLE both directions fail, and there was never a
	// flag to lose: the install must not fail for it.
	s := newSeam()
	s.clearErr = errClear
	s.setErr = errSet
	if err := s.replace("gt"); err != nil {
		t.Fatalf("Replace: %v, want a replace that tolerates a flag the system will not grant", err)
	}
	if s.renamed != testDest {
		t.Fatal("the binary was not installed")
	}
}

// The four tests below drive the real operations behind the seams, which the
// fakes cannot: they are the paths a replace takes when the disk itself says
// no. They run with the real lock, so their directories hold the lock file
// beside the target.

func TestReplaceReportsACopyThatCannotOpenTheMissingSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// The existence check saw the file; the copy a moment later did not, the
	// race the 0755 fallback in the mode read exists for.
	files := Files{IsFile: func(string) bool { return true }}
	destDir := filepath.Join(dir, "bin")
	err := (Installer{Files: files}).Replace(filepath.Join(dir, "gt-built"), destDir, "gt")
	if err == nil || !strings.Contains(err.Error(), "copy") {
		t.Fatalf("Replace: %v, want the copy to report the vanished source", err)
	}
	names, readErr := entryNames(destDir)
	if readErr != nil || strings.Join(names, ",") != ".gt.replace.lock" {
		t.Fatalf("%s holds %v (%v), want the lock file and no temp left behind", destDir, names, readErr)
	}
}

func TestReplaceReportsADestinationDirectoryTheDiskWouldNotCreate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := writeSourceFile(t, dir)
	// MkdirAll answers success without creating, which is the one way a real
	// os.CreateTemp meets a directory that is not there. The lock answers
	// without locking for the same reason: the real one would fail first, on
	// the lock file it cannot open in a directory that is absent.
	files := Files{
		MkdirAll: func(string) error { return nil },
		Lock:     func(string) (func(), error) { return func() {}, nil },
	}
	destDir := filepath.Join(dir, "absent")
	err := (Installer{Files: files}).Replace(src, destDir, "gt")
	if err == nil || !strings.Contains(err.Error(), "create temp file") || !strings.Contains(err.Error(), destDir) {
		t.Fatalf("Replace: %v, want the temp-file error to name %s", err, destDir)
	}
}

func TestReplaceReportsACopyThatCannotOpenTheTempFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := writeSourceFile(t, dir)
	absent := filepath.Join(dir, "absent")
	files := Files{TempFile: func(string, string) (string, error) {
		return filepath.Join(absent, ".gt.tmp.1"), nil
	}}
	err := (Installer{Files: files}).Replace(src, filepath.Join(dir, "bin"), "gt")
	if err == nil || !strings.Contains(err.Error(), "copy") {
		t.Fatalf("Replace: %v, want the copy to report the temp file it could not open", err)
	}
}

func TestReplaceReportsACopyThatCannotReadTheSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A directory opens and then refuses the read, which is how the streaming
	// copy meets an unreadable source.
	src := filepath.Join(dir, "gt-built")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", src, err)
	}
	files := Files{IsFile: func(string) bool { return true }}
	destDir := filepath.Join(dir, "bin")
	err := (Installer{Files: files}).Replace(src, destDir, "gt")
	if err == nil || !strings.Contains(err.Error(), "copy") {
		t.Fatalf("Replace: %v, want the copy to report the unreadable source", err)
	}
	names, readErr := entryNames(destDir)
	if readErr != nil || strings.Join(names, ",") != ".gt.replace.lock" {
		t.Fatalf("%s holds %v (%v), want the lock file and no temp left behind", destDir, names, readErr)
	}
}

// writeSourceFile is a built binary on a real filesystem.
func writeSourceFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "gt-built")
	if err := os.WriteFile(path, []byte("built binary"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// entryNames is the sorted names in dir, for asserting what a replace left.
func entryNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names, nil
}

// indexOf is the position of call in the log, or -1 when the code never made
// it.
func indexOf(calls []string, call string) int {
	for i, got := range calls {
		if got == call {
			return i
		}
	}
	return -1
}
