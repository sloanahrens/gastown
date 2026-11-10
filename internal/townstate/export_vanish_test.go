package townstate

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestExportSkipsAFileThatVanishesDuringTheWalk holds the live-town rule: a
// rehearsal export runs beside the daemon, which writes files atomically through
// a temporary name, so an entry the directory listing named can be gone by the
// stat or by the open. Each window skips the entry under SkipVanished and the
// archive stays whole; a vanish at the open leaves no header without a body,
// because the file is opened before its header is written (gt-55evhv). Every
// other lstat or open error still fails the walk: TestExportDrivesWalkAndFileFailures.
func TestExportSkipsAFileThatVanishesDuringTheWalk(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeStateFile(t, town, "daemon/state.json", "state\n", 0o644)
	writeStateFile(t, town, "daemon/sub/inner.txt", "inner\n", 0o644)

	cases := []struct {
		name     string
		over     func(*townOps)
		vanished string // the entry that must be missing from the archive
		kept     string // an entry that must still be in it
	}{
		{
			name: "gone between the listing and the stat",
			over: func(o *townOps) {
				o.lstat = func(p string) (fs.FileInfo, error) {
					if strings.HasSuffix(filepath.ToSlash(p), "/daemon/sub/inner.txt") {
						return nil, &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
					}
					return os.Lstat(p)
				}
			},
			vanished: "daemon/sub/inner.txt",
			kept:     "daemon/state.json",
		},
		{
			name: "gone between the stat and the open",
			over: func(o *townOps) {
				real := osOps().open
				o.open = func(p string) (io.ReadCloser, error) {
					if strings.HasSuffix(filepath.ToSlash(p), "/daemon/state.json") {
						return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
					}
					return real(p)
				}
			},
			vanished: "daemon/state.json",
			kept:     "daemon/sub/inner.txt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stream bytes.Buffer
			sum, err := Export(&stream, town, Default(), ExportOptions{AllowNoRigs: true, ops: opsWith(tc.over)})
			if err != nil {
				t.Fatalf("Export: %v, want the vanished entry skipped", err)
			}
			if got := sum.Skipped[SkipVanished]; got != 1 {
				t.Errorf("Skipped[SkipVanished] = %d, want 1", got)
			}
			// readArchive fails on a header whose body is missing, so this read
			// is also the check that the stream stayed whole.
			names := archiveNames(t, stream.Bytes())
			if slices.Contains(names, tc.vanished) {
				t.Errorf("the archive carries %s, which vanished during the walk: %q", tc.vanished, names)
			}
			if !slices.Contains(names, tc.kept) {
				t.Errorf("the archive lacks %s, which did not vanish: %q", tc.kept, names)
			}
			for _, e := range readArchive(t, stream.Bytes()) {
				if e.hdr.Name == tc.kept && int64(len(e.data)) != e.hdr.Size {
					t.Errorf("%s: body is %d bytes, header says %d", tc.kept, len(e.data), e.hdr.Size)
				}
			}
		})
	}
}
