package forgejoprobe

import (
	"path/filepath"
	"testing"
)

// TestSystemFSRefusesAMissingDirectory holds the operating-system adapter's failure
// path: a temporary file cannot be made in a directory that is not there, so the
// adapter returns the error and no file. The directory is never created.
func TestSystemFSRefusesAMissingDirectory(t *testing.T) {
	t.Parallel()
	f, err := osFS{}.CreateTemp(filepath.Join(t.TempDir(), "absent"), ".probe-rig.*")
	if err == nil {
		_ = f.Close()
		t.Fatalf("CreateTemp in a missing directory = nil error, want one")
	}
	if f != nil {
		t.Errorf("CreateTemp returned a file alongside its error")
	}
}
