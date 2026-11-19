//go:build unix

package deployercred_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/deployercred"
)

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
	//testpolicy:allow no-exec-files — the target is the test directory, not a script, and 0700 is the mode Store requires
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
