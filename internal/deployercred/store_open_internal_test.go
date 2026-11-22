package deployercred

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeFileInfo is a FileInfo whose mode the test chooses.
type fakeFileInfo struct{ mode fs.FileMode }

func (f fakeFileInfo) Name() string       { return "approval.json" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return nil }

// TestDeployerCredConfirmOpened: the file Dir.Open opened must be the file it lstat'd, and a
// regular file. A file swapped in between the lstat and the open (a different file at the
// opened handle) or a handle that is not a regular file (a FIFO, a device) is refused, whatever
// the lstat said (gt-371a2d). The comparison is a function so the swap needs no race to test.
func TestDeployerCredConfirmOpened(t *testing.T) {
	t.Parallel()
	regular := fakeFileInfo{mode: 0o600}
	for _, tc := range []struct {
		name    string
		opened  fs.FileInfo
		same    bool
		wantErr string
	}{
		{"the same regular file", regular, true, ""},
		{"another file than the one lstat'd", regular, false, "is not the file that was lstat'd"},
		{"the same handle but not a regular file", fakeFileInfo{mode: fs.ModeNamedPipe | 0o600}, true, "is not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			same := func(a, b fs.FileInfo) bool { return tc.same }

			err := confirmOpened("approval.json", regular, tc.opened, same)

			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("confirmOpened = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("confirmOpened = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestDeployerCredOpenRefusesAReplacedFile: Dir.Open on a real file in a real directory is
// refused when the sameness test says the opened handle is another file, and is accepted with
// the real os.SameFile (gt-371a2d).
func TestDeployerCredOpenRefusesAReplacedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "approval.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	if f, err := (diskDir{root: root}).Open("approval.json"); err != nil {
		t.Fatalf("Open with the real SameFile = %v, want the file", err)
	} else if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	notSame := diskDir{root: root, same: func(a, b fs.FileInfo) bool { return false }}
	f, err := notSame.Open("approval.json")
	if err == nil || !strings.Contains(err.Error(), "replaced while it was opened") || f != nil {
		t.Errorf("Open = %v, %v; want a refusal naming the replacement and no file", f, err)
	}
}
