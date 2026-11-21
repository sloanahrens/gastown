//go:build unix

package deployercred

import (
	"io/fs"
	"testing"
	"time"
)

// noStatInfo is an fs.FileInfo the platform does not report as a Unix stat, so
// infoOf has no uid or gid to read from it.
type noStatInfo struct{ name string }

func (i noStatInfo) Name() string       { return i.name }
func (i noStatInfo) Size() int64        { return 0 }
func (i noStatInfo) Mode() fs.FileMode  { return 0o600 }
func (i noStatInfo) ModTime() time.Time { return time.Time{} }
func (i noStatInfo) IsDir() bool        { return false }
func (i noStatInfo) Sys() any           { return nil }

// TestDeployerCredInfoNoStat shows infoOf reports -1 for both the owner and the
// group when the file info carries no Unix stat, so neither a missing owner nor
// a missing group is read as 0.
func TestDeployerCredInfoNoStat(t *testing.T) {
	t.Parallel()
	info := infoOf(noStatInfo{name: "the-credential"})
	if info.Owner != -1 || info.Group != -1 {
		t.Fatalf("infoOf of a file info with no Unix stat reports owner %d and group %d, want -1 for both", info.Owner, info.Group)
	}
}
