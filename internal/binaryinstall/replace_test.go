package binaryinstall

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

const (
	testSrc     = "built/gt"
	testDestDir = "install-dir"
	testDest    = "install-dir/gt"
)

var (
	errMkdir  = errors.New("mkdir refused")
	errTemp   = errors.New("create temp refused")
	errCopy   = errors.New("copy refused")
	errChmod  = errors.New("chmod refused")
	errRename = errors.New("rename refused (operation not permitted)")
	errSet    = errors.New("chattr +i refused (operation not permitted)")
	errClear  = errors.New("chattr -i refused (operation not permitted)")
)

// seam is one replace's recorded seam: it logs each call where it happens and
// answers success, so a test overrides only the failure it drives.
type seam struct {
	calls []string
	files Files

	missing   bool
	mkdirErr  error
	tempErr   error
	copyErr   error
	chmodErr  error
	renameErr error
	clearErr  error
	setErr    error

	mode    os.FileMode
	modeErr error

	perm    os.FileMode
	temp    string
	renamed string
	removed []string
}

func newSeam() *seam {
	s := &seam{mode: 0o755}
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
			return filepath.Join(dir, ".gt.tmp.1"), nil
		},
		Copy: func(dst, src string) error {
			s.log("copy", dst+" <- "+src)
			s.temp = dst
			return s.copyErr
		},
		Chmod: func(path string, perm os.FileMode) error {
			s.log("chmod", path)
			s.perm = perm
			return s.chmodErr
		},
		Rename: func(oldpath, newpath string) error {
			s.log("rename", oldpath+" -> "+newpath)
			s.renamed = newpath
			return s.renameErr
		},
		Remove: func(path string) error {
			s.log("remove", path)
			s.removed = append(s.removed, path)
			return nil
		},
		ClearImmutable: func(path string) error {
			s.log("clear-immutable", path)
			return s.clearErr
		},
		SetImmutable: func(path string) error {
			s.log("set-immutable", path)
			return s.setErr
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
	want := []string{
		"is-file " + testSrc,
		"mode " + testSrc,
		"mkdir-all " + testDestDir,
		"temp-file " + filepath.Join(testDestDir, ".gt.tmp.*"),
		"copy " + filepath.Join(testDestDir, ".gt.tmp.1") + " <- " + testSrc,
		"chmod " + filepath.Join(testDestDir, ".gt.tmp.1"),
		"clear-immutable " + testDest,
		"rename " + filepath.Join(testDestDir, ".gt.tmp.1") + " -> " + testDest,
		"set-immutable " + testDest,
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

func TestReplaceCopiesTheSourceMode(t *testing.T) {
	t.Parallel()
	s := newSeam()
	s.mode = 0o641
	if err := s.replace("gt"); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if s.perm != 0o641 {
		t.Fatalf("installed with mode %#o, want the source's %#o", s.perm, 0o641)
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
	if s.temp != "" {
		t.Fatal("a replace went on to write a temp file in a directory it could not create")
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
}

func TestReplaceRemovesTheTempFileWhenTheModeCannotBeSet(t *testing.T) {
	t.Parallel()
	s := newSeam()
	s.chmodErr = errChmod
	err := s.replace("gt")
	if !errors.Is(err, errChmod) {
		t.Fatalf("Replace: %v, want the chmod error", err)
	}
	if len(s.removed) != 1 || s.removed[0] != filepath.Join(testDestDir, ".gt.tmp.1") {
		t.Fatalf("removed %v, want the temp file", s.removed)
	}
	if s.renamed != "" {
		t.Fatal("a replace renamed a temp file whose mode it could not set")
	}
}

func TestReplaceRemovesTheTempFileWhenTheCopyFails(t *testing.T) {
	t.Parallel()
	s := newSeam()
	s.copyErr = errCopy
	err := s.replace("gt")
	if !errors.Is(err, errCopy) {
		t.Fatalf("Replace: %v, want the copy error", err)
	}
	if len(s.removed) != 1 || s.removed[0] != filepath.Join(testDestDir, ".gt.tmp.1") {
		t.Fatalf("removed %v, want the temp file", s.removed)
	}
	if s.renamed != "" {
		t.Fatal("a failed copy still renamed a temp file over the target")
	}
}

func TestReplaceRemovesTheTempFileWhenTheRenameFails(t *testing.T) {
	t.Parallel()
	s := newSeam()
	s.renameErr = errRename
	err := s.replace("gt")
	if !errors.Is(err, errRename) {
		t.Fatalf("Replace: %v, want the rename error", err)
	}
	if len(s.removed) != 1 || s.removed[0] != filepath.Join(testDestDir, ".gt.tmp.1") {
		t.Fatalf("removed %v, want the temp file", s.removed)
	}
}

func TestReplaceRestoresTheImmutableFlagAfterAFailedRename(t *testing.T) {
	t.Parallel()
	s := newSeam()
	s.renameErr = errRename
	_ = s.replace("gt")
	cleared := indexOf(s.calls, "clear-immutable "+testDest)
	renamed := indexOf(s.calls, "rename "+filepath.Join(testDestDir, ".gt.tmp.1")+" -> "+testDest)
	restored := indexOf(s.calls, "set-immutable "+testDest)
	if cleared < 0 || renamed < 0 || restored < 0 {
		t.Fatalf("calls %v do not hold the flag cleared, the rename and the flag back", s.calls)
	}
	if cleared >= renamed || renamed >= restored {
		t.Fatalf("calls %v put the flag back before the rename that failed, leaving the binary writable", s.calls)
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

// TestReplaceLeavesNoTempFileBesideTheTarget runs on a real directory, the one
// property a fake cannot show: the temp file the real os.CreateTemp makes sits
// in the target's own directory (so the rename stays on one filesystem) and is
// gone once the replace is done.
func TestReplaceLeavesNoTempFileBesideTheTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "gt-built")
	if err := os.WriteFile(src, []byte("new binary"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	destDir := filepath.Join(dir, "bin")
	var temp string
	files := Files{
		Copy: func(dst, src string) error {
			temp = dst
			return copyFile(dst, src)
		},
		// The mode is faked because a real 0755 in a unit test is an
		// executable file the test policy refuses; Chmod is not what this
		// test is about.
		Chmod: func(string, os.FileMode) error { return nil },
		// The flag needs a kernel capability the test host may not grant,
		// and setting it on a temp directory would block its cleanup.
		ClearImmutable: func(string) error { return nil },
		SetImmutable:   func(string) error { return nil },
	}
	if err := (Installer{Files: files}).Replace(src, destDir, "gt"); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	dest := filepath.Join(destDir, "gt")
	if filepath.Dir(temp) != destDir {
		t.Fatalf("temp file %q is not in the target's directory %q, so the rename could cross a filesystem", temp, destDir)
	}
	if temp == dest {
		t.Fatalf("the temp file is the target itself (%q)", temp)
	}
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("read destination directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "gt" {
		t.Fatalf("destination directory holds %v, want only the installed binary", entries)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != "new binary" {
		t.Fatalf("installed %q, want the source bytes", got)
	}
}

// The four tests below drive the real operations behind the seams, which the
// fakes cannot: they are the paths a replace takes when the disk itself says
// no.

func TestReplaceStopsAtASourceThatVanishesBeforeTheModeRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// The existence check saw the file; the read a moment later did not, which
	// is the race the 0755 fallback exists for.
	files := Files{IsFile: func(string) bool { return true }}
	destDir := filepath.Join(dir, "bin")
	err := (Installer{Files: files}).Replace(filepath.Join(dir, "gt-built"), destDir, "gt")
	if err == nil || !strings.Contains(err.Error(), "copy") {
		t.Fatalf("Replace: %v, want the copy to report the vanished source", err)
	}
	if entries, readErr := os.ReadDir(destDir); readErr != nil || len(entries) != 0 {
		t.Fatalf("%s holds %v (%v), want the temp file removed", destDir, entries, readErr)
	}
}

func TestReplaceReportsADestinationDirectoryTheDiskWouldNotCreate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := writeSourceFile(t, dir)
	// MkdirAll answers success without creating, which is the one way a real
	// os.CreateTemp meets a directory that is not there.
	files := Files{MkdirAll: func(string) error { return nil }}
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
	if entries, readErr := os.ReadDir(destDir); readErr != nil || len(entries) != 0 {
		t.Fatalf("%s holds %v (%v), want the temp file removed", destDir, entries, readErr)
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
