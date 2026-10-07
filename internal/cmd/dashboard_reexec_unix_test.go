//go:build !windows

package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileInodeIsReadOnThisPlatform(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fileInode(fi) == 0 {
		t.Fatal("fileInode read 0: stampFile could not tell a replaced binary from the old one by inode")
	}
}

func TestDashboardListenerBindsWhenNothingIsInherited(t *testing.T) {
	t.Parallel()
	ln, inherited, err := dashboardListener("127.0.0.1:0", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if inherited {
		t.Fatal("reported an inherited socket with none handed over")
	}
}

func TestDashboardListenerRejectsABadFD(t *testing.T) {
	t.Parallel()
	if _, _, err := dashboardListener("127.0.0.1:0", "not-a-number"); err == nil {
		t.Fatal("accepted a non-numeric inherited fd")
	}
}
