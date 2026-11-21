//go:build unix

package deployercred_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/steveyegge/gastown/internal/deployercred"
)

// fakeGroup is the gid the in-memory fake is given, chosen so a test that reads
// a zero back from Info sees the fake's group was dropped.
const fakeGroup = 10001

// TestDeployerCredInfoGroup shows Info carries the owning group: Disk reads it
// from the host's stat, and the fake reports the group it was given. The host's
// gid is compared with the one os.Stat reports for the same path, never with a
// constant or the process's gid, because a new directory's group follows the
// host's setgid state.
func TestDeployerCredInfoGroup(t *testing.T) {
	t.Parallel()
	t.Run("disk", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// The CI runner's umask is 000, so TempDir does not leave the directory
		// 0700, the mode the deployer directory has.
		//testpolicy:allow no-exec-files — the rule flags any literal mode with an execute bit; this chmod sets a test directory to 0700, and no file becomes executable
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatalf("could not set the test directory to 0700: %v", err)
		}
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("could not stat %s: %v", dir, err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("the host reports no Unix stat for %s, so its group cannot be read", dir)
		}
		info, err := deployercred.Disk{}.Lstat(dir)
		if err != nil {
			t.Fatalf("Disk.Lstat of %s: %v", dir, err)
		}
		if want := int(st.Gid); info.Group != want {
			t.Fatalf("Disk.Lstat of %s reports group %d, want the OS's %d", dir, info.Group, want)
		}
	})
	t.Run("fake", func(t *testing.T) {
		t.Parallel()
		f := newFakeStoreFS(storeUID)
		f.group = fakeGroup
		f.putDir(storeDir, 0o700, storeUID)
		info, err := f.Lstat(storeDir)
		if err != nil {
			t.Fatalf("the fake could not read %s: %v", storeDir, err)
		}
		if info.Group != fakeGroup {
			t.Fatalf("the fake reports group %d for %s, want the %d it was given", info.Group, storeDir, fakeGroup)
		}
	})
}

// TestDeployerCredStoreDisk drives the real Disk wrapper through Store: the
// wrapper's own calls, not the fake's, must land files 0600 in a 0700
// directory. The temp root is resolved through EvalSymlinks first, because a
// symlinked ancestor (/var on macOS) is a path Store refuses.
func TestDeployerCredStoreDisk(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("could not resolve the test directory: %v", err)
	}
	// The CI runner's umask is 000, so TempDir does not leave the directory in
	// the one mode Store accepts.
	//testpolicy:allow no-exec-files — the rule flags any literal mode with an execute bit; this chmod sets a test directory to 0700, and no file becomes executable
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("could not set the test directory to 0700: %v", err)
	}
	files := map[string][]byte{
		"gt-deployer-apply.key":        []byte("the key bytes"),
		"gt-deployer-apply.kubeconfig": []byte("the kubeconfig bytes"),
	}
	uid := os.Getuid()
	disk := deployercred.Disk{}
	if err := deployercred.Store(disk, uid, dir, files); err != nil {
		t.Fatalf("Store refused a 0700 directory in a temp dir: %v", err)
	}
	for name, data := range files {
		target := filepath.Join(dir, name)
		info, err := disk.Lstat(target)
		if err != nil {
			t.Fatalf("could not read the credential %s: %v", target, err)
		}
		if info.Mode.Perm() != 0o600 {
			t.Fatalf("the credential %s is mode %04o, want 0600", target, info.Mode.Perm())
		}
		if info.Owner != uid {
			t.Fatalf("the credential %s is owned by uid %d, want %d", target, info.Owner, uid)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("could not read the credential %s: %v", target, err)
		}
		if string(got) != string(data) {
			t.Fatalf("the credential %s holds %q, want %q", target, got, data)
		}
	}
}

// sameInfo fails when an open handle reports other attributes than the OS does
// for the same path. The OS's report is the reference, never a constant: a
// created directory's group follows the host's setgid state.
func sameInfo(t *testing.T, what, path string, got deployercred.Info) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("could not stat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("the host reports no Unix stat for %s", path)
	}
	want := deployercred.Info{Mode: fi.Mode(), Owner: int(st.Uid), Group: int(st.Gid)}
	if got != want {
		t.Fatalf("%s of %s reports %+v, want the OS's %+v", what, path, got, want)
	}
}

// openDir opens dir as a handle, failing the test on an error.
func openDir(t *testing.T, dir string) deployercred.Dir {
	t.Helper()
	handle, err := (deployercred.Disk{}).OpenDir(dir)
	if err != nil {
		t.Fatalf("Disk.OpenDir of %s: %v", dir, err)
	}
	return handle
}

// closeDir closes a handle at the test's end, failing it when the close does.
func closeDir(t *testing.T, handle deployercred.Dir) {
	t.Helper()
	if err := handle.Close(); err != nil {
		t.Errorf("could not close the directory handle: %v", err)
	}
}

// TestDeployerCredReadFS: a directory handle and a file opened through it report
// what the OS reports for the same paths, and a read through the handle returns
// the file's bytes.
func TestDeployerCredReadFS(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// The CI runner's umask is 000, so TempDir does not leave the directory in
	// the 0700 a credential directory has.
	//testpolicy:allow no-exec-files — the rule flags any literal mode with an execute bit; this chmod sets a test directory to 0700, and no file becomes executable
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("could not set the test directory to 0700: %v", err)
	}
	const name = "approval.json"
	want := []byte(`{"plan":"example"}`)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatalf("could not write %s: %v", path, err)
	}
	handle := openDir(t, dir)
	defer closeDir(t, handle)
	dirInfo, err := handle.Stat()
	if err != nil {
		t.Fatalf("Dir.Stat of %s: %v", dir, err)
	}
	sameInfo(t, "Dir.Stat", dir, dirInfo)
	file, err := handle.Open(name)
	if err != nil {
		t.Fatalf("Dir.Open of %s: %v", name, err)
	}
	fileInfo, err := file.Stat()
	if err != nil {
		t.Fatalf("ReadFile.Stat of %s: %v", path, err)
	}
	sameInfo(t, "ReadFile.Stat", path, fileInfo)
	got, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("reading %s through the handle: %v", path, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("could not close %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the read through the handle returned %q, want %q", got, want)
	}
}

// TestDeployerCredReadFSErrors: each failure path of the read side returns its
// error rather than an empty report.
func TestDeployerCredReadFSErrors(t *testing.T) {
	t.Parallel()
	t.Run("open a directory that does not exist", func(t *testing.T) {
		t.Parallel()
		if _, err := (deployercred.Disk{}).OpenDir(filepath.Join(t.TempDir(), "absent")); err == nil {
			t.Fatal("Disk.OpenDir opened a directory that does not exist")
		}
	})
	t.Run("stat a closed directory handle", func(t *testing.T) {
		t.Parallel()
		handle := openDir(t, t.TempDir())
		if err := handle.Close(); err != nil {
			t.Fatalf("could not close the directory handle: %v", err)
		}
		if _, err := handle.Stat(); err == nil {
			t.Fatal("Dir.Stat of a closed handle reported attributes")
		}
	})
	t.Run("open a name that does not exist", func(t *testing.T) {
		t.Parallel()
		handle := openDir(t, t.TempDir())
		defer closeDir(t, handle)
		if _, err := handle.Open("absent"); err == nil {
			t.Fatal("Dir.Open opened a name that does not exist")
		}
	})
	t.Run("stat a closed file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		const name = "approval.json"
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatalf("could not write %s: %v", name, err)
		}
		handle := openDir(t, dir)
		defer closeDir(t, handle)
		file, err := handle.Open(name)
		if err != nil {
			t.Fatalf("Dir.Open of %s: %v", name, err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("could not close %s: %v", name, err)
		}
		if _, err := file.Stat(); err == nil {
			t.Fatal("ReadFile.Stat of a closed file reported attributes")
		}
	})
}
