package townstate

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stateInfo is a fake fs.FileInfo the seam returns, so a test can describe an
// entry without one existing on the host.
type stateInfo struct {
	name string
	mode fs.FileMode
	size int64
	mod  time.Time
}

func (s stateInfo) Name() string       { return s.name }
func (s stateInfo) Size() int64        { return s.size }
func (s stateInfo) Mode() fs.FileMode  { return s.mode }
func (s stateInfo) ModTime() time.Time { return s.mod }
func (s stateInfo) IsDir() bool        { return s.mode.IsDir() }
func (s stateInfo) Sys() any           { return nil }

// errWriter refuses every write, so the first archive write fails.
type errWriter struct{ err error }

func (w errWriter) Write([]byte) (int, error) { return 0, w.err }

// failAfterWriter accepts n bytes and fails the next write, so a test can
// choose which archive header fails.
type failAfterWriter struct {
	n       int
	written int
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.written+len(p) > w.n {
		return 0, errors.New("write failed")
	}
	w.written += len(p)
	return len(p), nil
}

// zeroBlockWriter fails on the tar trailer, which is two zero blocks, so the
// archive's content is written and only the close fails.
type zeroBlockWriter struct {
	io.Writer
}

func (w zeroBlockWriter) Write(p []byte) (int, error) {
	if len(p) == 512 && bytes.Equal(p, make([]byte, 512)) {
		return 0, errors.New("close failed")
	}
	return w.Writer.Write(p)
}

// closeFailer reads a full file and fails on close.
type closeFailer struct{ io.Reader }

func (closeFailer) Close() error { return errors.New("close failed") }

// closeFailWriter accepts every write and fails on close.
type closeFailWriter struct{}

func (closeFailWriter) Write(p []byte) (int, error) { return len(p), nil }
func (closeFailWriter) Close() error                { return errors.New("close failed") }

// opsWith starts from the host seam and lets a test replace operations.
func opsWith(over func(*townOps)) townOps {
	ops := osOps()
	over(&ops)
	return ops
}

// lstatFailingOn fails for a path with the given suffix and defers to the host
// for every other path.
func lstatFailingOn(t *testing.T, suffix string) func(string) (fs.FileInfo, error) {
	t.Helper()
	return func(p string) (fs.FileInfo, error) {
		if strings.HasSuffix(filepath.ToSlash(p), suffix) {
			return nil, errors.New("lstat failed")
		}
		return os.Lstat(p)
	}
}

// lstatOverriding answers with info for a path with the given suffix and
// defers to the host for every other path, which is the shape of a target root
// that already holds one file.
func lstatOverriding(suffix string, info fs.FileInfo) func(string) (fs.FileInfo, error) {
	return func(p string) (fs.FileInfo, error) {
		if strings.HasSuffix(filepath.ToSlash(p), suffix) {
			return info, nil
		}
		return os.Lstat(p)
	}
}

// TestExportRefusesBadInputs drives every refusal an export makes before it
// walks anything: no manifest, an invalid one, no root, a root that is a file,
// and a rig registry that cannot be read.
func TestExportRefusesBadInputs(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cases := []struct {
		name string
		root string
		m    *Manifest
		want string
	}{
		{name: "no manifest", root: town, m: nil, want: "nil manifest"},
		{
			name: "an invalid manifest",
			root: town,
			m:    &Manifest{Entries: []Entry{{Path: "logs", Owner: OwnerSession, Why: "a session tree", Rollback: true}}},
			want: "rollback: true needs a daemon owner",
		},
		{name: "an empty root", root: "", m: Default(), want: "town root is empty"},
		{name: "a missing root", root: filepath.Join(town, "absent"), m: Default(), want: "reading the town root"},
	}
	file := writeStateFile(t, town, "a-file", "x", 0o644)
	cases = append(cases, struct {
		name string
		root string
		m    *Manifest
		want string
	}{name: "a root that is a file", root: file, m: Default(), want: "is not a directory"})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Export(io.Discard, tc.root, tc.m, ExportOptions{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Export = %v, want an error holding %q", err, tc.want)
			}
		})
	}

	t.Run("a registry that cannot be read", func(t *testing.T) {
		t.Parallel()
		bad := t.TempDir()
		writeStateFile(t, bad, "mayor/town.json", "{not json", 0o644)
		if _, err := Export(io.Discard, bad, Default(), ExportOptions{}); err == nil || !strings.Contains(err.Error(), "reading the town's rigs") {
			t.Fatalf("Export over an unreadable registry = %v, want a rig-reading refusal", err)
		}
	})
}

// TestExportDrivesWalkAndFileFailures drives every walk and file failure the
// export can meet under a seam: an entry that cannot be read, a directory that
// cannot be listed, a child that cannot be read, a failed open, a short file,
// a failing close, a symlink that cannot be read, and each header write.
func TestExportDrivesWalkAndFileFailures(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeStateFile(t, town, "daemon/state.json", "state\n", 0o644)
	writeStateFile(t, town, "daemon/sub/inner.txt", "inner\n", 0o644)
	if err := os.Symlink("state.json", filepath.Join(town, "daemon", "link")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		over func(*townOps)
		w    io.Writer
		want string
	}{
		{
			name: "an entry that cannot be read",
			over: func(o *townOps) { o.lstat = lstatFailingOn(t, "/daemon") },
			w:    io.Discard,
			want: "reading daemon",
		},
		{
			name: "a directory that cannot be listed",
			over: func(o *townOps) {
				o.readDir = func(string) ([]string, error) { return nil, errors.New("readdir failed") }
			},
			w:    io.Discard,
			want: "reading daemon",
		},
		{
			name: "a child that cannot be read",
			over: func(o *townOps) { o.lstat = lstatFailingOn(t, "/daemon/state.json") },
			w:    io.Discard,
			want: "reading daemon/state.json",
		},
		{
			name: "a child whose own walk fails",
			over: func(o *townOps) {
				real := osOps().readDir
				o.readDir = func(p string) ([]string, error) {
					if strings.HasSuffix(filepath.ToSlash(p), "/daemon/sub") {
						return nil, errors.New("readdir failed")
					}
					return real(p)
				}
			},
			w:    io.Discard,
			want: "reading daemon/sub",
		},
		{
			name: "the directory header",
			over: func(*townOps) {},
			w:    errWriter{err: errors.New("write failed")},
			want: "writing the archive header for daemon",
		},
		{
			// The directory's header fills the first 512 bytes; the next header
			// is the symlink's, which is the write that fails.
			name: "a symlink's header",
			over: func(*townOps) {},
			w:    &failAfterWriter{n: 512},
			want: "writing the archive header for daemon/link",
		},
		{
			// The directory and the link headers fit in 1024 bytes; the regular
			// file's header is the write that fails.
			name: "a regular file's header",
			over: func(*townOps) {},
			w:    &failAfterWriter{n: 1024},
			want: "writing the archive header for daemon/state.json",
		},
		{
			name: "opening a file",
			over: func(o *townOps) {
				o.open = func(string) (io.ReadCloser, error) { return nil, errors.New("open failed") }
			},
			w:    io.Discard,
			want: "opening daemon/state.json",
		},
		{
			name: "a file shorter than its header says",
			over: func(o *townOps) {
				o.open = func(string) (io.ReadCloser, error) {
					return io.NopCloser(strings.NewReader("short")), nil
				}
			},
			w:    io.Discard,
			want: "copying daemon/state.json into the archive",
		},
		{
			name: "closing a file after the copy",
			over: func(o *townOps) {
				o.open = func(string) (io.ReadCloser, error) {
					return closeFailer{strings.NewReader("state\n")}, nil
				}
			},
			w:    io.Discard,
			want: "closing daemon/state.json after the copy",
		},
		{
			name: "a symlink that cannot be read",
			over: func(o *townOps) {
				o.readlink = func(string) (string, error) { return "", errors.New("readlink failed") }
			},
			w:    io.Discard,
			want: "reading the symlink daemon/link",
		},
		{
			name: "the tar trailer",
			over: func(*townOps) {},
			w:    zeroBlockWriter{io.Discard},
			want: "closing the tar stream",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Export(tc.w, town, Default(), ExportOptions{ops: opsWith(tc.over)})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Export = %v, want an error holding %q", err, tc.want)
			}
		})
	}

}

// TestImportRefusesBadInputs drives every refusal an import makes before or
// while it reads the stream: no manifest, an invalid one, no root, a root that
// is a file, an unreadable registry, a stream that is not a tar, and each
// illegal name.
func TestImportRefusesBadInputs(t *testing.T) {
	t.Parallel()
	dest := t.TempDir()
	cases := []struct {
		name    string
		root    string
		m       *Manifest
		archive []byte
		want    string
	}{
		{name: "no manifest", root: dest, m: nil, want: "nil manifest"},
		{
			name: "an invalid manifest",
			root: dest,
			m:    &Manifest{Entries: []Entry{{Path: "logs", Owner: OwnerShared, Why: "shared", Private: true}}},
			want: "private: true needs a daemon owner",
		},
		{name: "an empty root", root: "", m: Default(), want: "town root is empty"},
		{name: "a missing root", root: filepath.Join(dest, "absent"), m: Default(), want: "reading the town root"},
	}
	file := writeStateFile(t, dest, "a-file", "x", 0o644)
	cases = append(cases, struct {
		name    string
		root    string
		m       *Manifest
		archive []byte
		want    string
	}{name: "a root that is a file", root: file, m: Default(), want: "is not a directory"})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Import(bytes.NewReader(writeArchive(t, regularEntry("daemon/x", "x"))), tc.root, tc.m, ImportOptions{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Import = %v, want an error holding %q", err, tc.want)
			}
		})
	}

	t.Run("a registry that cannot be read", func(t *testing.T) {
		t.Parallel()
		bad := t.TempDir()
		writeStateFile(t, bad, "mayor/town.json", "{not json", 0o644)
		if _, err := Import(bytes.NewReader(writeArchive(t, regularEntry("daemon/x", "x"))), bad, Default(), ImportOptions{MergeNewer: true}); err == nil || !strings.Contains(err.Error(), "reading the destination's rigs") {
			t.Fatalf("Import over an unreadable registry = %v, want a rig-reading refusal", err)
		}
	})

	t.Run("a stream that is not a tar", func(t *testing.T) {
		t.Parallel()
		if _, err := Import(bytes.NewReader([]byte("not a tar stream")), dest, Default(), ImportOptions{MergeNewer: true}); err == nil || !strings.Contains(err.Error(), "reading the archive") {
			t.Fatalf("Import over a non-tar stream = %v, want a read refusal", err)
		}
	})

	names := []struct{ name, want string }{
		{name: "daemon//x", want: "empty or"},
		{name: "daemon/./x", want: "empty or"},
	}
	for _, tc := range names {
		t.Run("a name "+tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Import(bytes.NewReader(writeArchive(t, regularEntry(tc.name, "x"))), dest, Default(), ImportOptions{MergeNewer: true}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Import of %q = %v, want an error holding %q", tc.name, err, tc.want)
			}
		})
	}
}

// TestImportFailureCases drives every filesystem failure an import can meet
// under a seam: a path that cannot be read, an occupied destination that cannot
// be read, a directory that cannot be created, moded or stamped, a file that
// cannot be created, moded or stamped, and a symlink that cannot be created.
func TestImportFailureCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		archive []archiveEntry
		ops     func(t *testing.T) townOps
		opts    ImportOptions
		want    string
	}{
		{
			name:    "a file that cannot be read",
			archive: []archiveEntry{regularEntry("daemon/x", "x")},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) { o.lstat = lstatFailingOn(t, "/daemon/x") })
			},
			opts: ImportOptions{MergeNewer: true},
			want: "reading \"daemon/x\" in the destination",
		},
		{
			// The parent is absent, so checkComponents stops before the
			// directory and importDir's look is the one that fails. Merge mode
			// skips the occupied-root check, which looks at the same path.
			name: "a directory that cannot be read when it is created",
			archive: []archiveEntry{{hdr: &tar.Header{
				Name: ".runtime/jev/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Unix(1, 0),
			}}},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) { o.lstat = lstatFailingOn(t, "/.runtime/jev") })
			},
			opts: ImportOptions{MergeNewer: true},
			want: "reading \".runtime/jev\" in the destination",
		},
		{
			name:    "a path component that cannot be read",
			archive: []archiveEntry{regularEntry("daemon/x", "x")},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) { o.lstat = lstatFailingOn(t, "/daemon") })
			},
			opts: ImportOptions{MergeNewer: true},
			want: "reading daemon in the destination",
		},
		{
			name:    "an occupied destination that cannot be read",
			archive: []archiveEntry{regularEntry("daemon/x", "x")},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) { o.lstat = lstatFailingOn(t, "/daemon") })
			},
			want: "reading \"daemon\" in the destination",
		},
		{
			// The parent is made first, so its failure is the one named.
			name: "a directory whose parent cannot be created",
			archive: []archiveEntry{{hdr: &tar.Header{
				Name: ".runtime/jev/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Unix(0, 0),
			}}},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) {
					o.mkdirAll = func(string, fs.FileMode) error { return errors.New("mkdir failed") }
				})
			},
			want: "creating the parent of \".runtime/jev\"",
		},
		{
			name: "a directory that cannot be created",
			archive: []archiveEntry{{hdr: &tar.Header{
				Name: ".runtime/jev/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Unix(0, 0),
			}}},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) {
					o.mkdirAll = func(p string, _ fs.FileMode) error {
						if filepath.Base(p) == "jev" {
							return errors.New("mkdir failed")
						}
						return nil
					}
				})
			},
			want: "creating \".runtime/jev\"",
		},
		{
			name: "a directory mode that cannot be set",
			archive: []archiveEntry{{hdr: &tar.Header{
				Name: ".runtime/jev/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Unix(0, 0),
			}}},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) {
					o.chmod = func(string, fs.FileMode) error { return errors.New("chmod failed") }
				})
			},
			want: "setting the mode of \".runtime/jev\"",
		},
		{
			name: "a directory mtime that cannot be set",
			archive: []archiveEntry{{hdr: &tar.Header{
				Name: ".runtime/jev/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Unix(1, 0),
			}}},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) {
					o.chtimes = func(string, time.Time, time.Time) error { return errors.New("chtimes failed") }
				})
			},
			want: "setting the mtime of \".runtime/jev\"",
		},
		{
			name:    "a file already in a root that must be empty",
			archive: []archiveEntry{regularEntry("daemon/x", "new")},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) {
					o.lstat = lstatOverriding("/daemon/x", stateInfo{name: "x", mode: 0o644, size: 3})
				})
			},
			want: "already exists in the destination",
		},
		{
			name:    "a file whose directory cannot be created",
			archive: []archiveEntry{regularEntry("daemon/x", "new")},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) {
					o.mkdirAll = func(string, fs.FileMode) error { return errors.New("mkdir failed") }
				})
			},
			want: "creating the directory for \"daemon/x\"",
		},
		{
			name:    "a file that cannot be created",
			archive: []archiveEntry{regularEntry("daemon/x", "new")},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) {
					o.openFile = func(string, int, fs.FileMode) (io.WriteCloser, error) {
						return nil, errors.New("open failed")
					}
				})
			},
			want: "creating \"daemon/x\"",
		},
		{
			name:    "a file whose mode cannot be set",
			archive: []archiveEntry{regularEntry("daemon/x", "new")},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) {
					o.chmod = func(string, fs.FileMode) error { return errors.New("chmod failed") }
				})
			},
			want: "setting the mode of \"daemon/x\"",
		},
		{
			name: "a file whose mtime cannot be set",
			archive: []archiveEntry{{hdr: &tar.Header{
				Name: "daemon/x", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1, ModTime: time.Unix(1, 0),
			}, data: []byte("x")}},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) {
					o.chtimes = func(string, time.Time, time.Time) error { return errors.New("chtimes failed") }
				})
			},
			want: "setting the mtime of \"daemon/x\"",
		},
		{
			name: "a symlink whose mode cannot be read",
			archive: []archiveEntry{{hdr: &tar.Header{
				Name: ".runtime/jev/link", Typeflag: tar.TypeSymlink, Linkname: "target", Mode: 0o777, ModTime: time.Unix(0, 0),
			}}},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) { o.lstat = lstatFailingOn(t, "/.runtime/jev/link") })
			},
			opts: ImportOptions{MergeNewer: true},
			want: "reading \".runtime/jev/link\" in the destination",
		},
		{
			name: "a symlink whose directory cannot be created",
			archive: []archiveEntry{{hdr: &tar.Header{
				Name: ".runtime/jev/link", Typeflag: tar.TypeSymlink, Linkname: "target", Mode: 0o777, ModTime: time.Unix(0, 0),
			}}},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) {
					o.mkdirAll = func(string, fs.FileMode) error { return errors.New("mkdir failed") }
				})
			},
			want: "creating the directory for \".runtime/jev/link\"",
		},
		{
			name: "a symlink that cannot be created",
			archive: []archiveEntry{{hdr: &tar.Header{
				Name: ".runtime/jev/link", Typeflag: tar.TypeSymlink, Linkname: "target", Mode: 0o777, ModTime: time.Unix(0, 0),
			}}},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) {
					o.symlink = func(string, string) error { return errors.New("symlink failed") }
				})
			},
			want: "creating the symlink \".runtime/jev/link\"",
		},
		{
			name: "a link target whose path cannot be read",
			archive: []archiveEntry{{hdr: &tar.Header{
				Name: "daemon/b", Typeflag: tar.TypeSymlink, Linkname: "sub/x", Mode: 0o777, ModTime: time.Unix(0, 0),
			}}},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) { o.lstat = lstatFailingOn(t, "/daemon/sub") })
			},
			want: "reading daemon/sub in the destination",
		},
		{
			name: "a link target that goes through a link that cannot be read",
			archive: []archiveEntry{{hdr: &tar.Header{
				Name: "daemon/b", Typeflag: tar.TypeSymlink, Linkname: "a", Mode: 0o777, ModTime: time.Unix(0, 0),
			}}},
			ops: func(t *testing.T) townOps {
				return opsWith(func(o *townOps) {
					o.lstat = lstatOverriding("/daemon/a", stateInfo{name: "a", mode: fs.ModeSymlink | 0o777})
					o.readlink = func(string) (string, error) { return "", errors.New("readlink failed") }
				})
			},
			want: "reading the symlink daemon/a in the destination",
		},
		{
			name: "a symlink with no target",
			archive: []archiveEntry{{hdr: &tar.Header{
				Name: ".runtime/jev/link", Typeflag: tar.TypeSymlink, Linkname: "", Mode: 0o777, ModTime: time.Unix(0, 0),
			}}},
			ops:  func(t *testing.T) townOps { return osOps() },
			want: "has an empty target",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dest := t.TempDir()
			opts := tc.opts
			opts.ops = tc.ops(t)
			_, err := Import(bytes.NewReader(writeArchive(t, tc.archive...)), dest, Default(), opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Import = %v, want an error holding %q", err, tc.want)
			}
		})
	}
}

// TestImportDrivesCopyAndPartialCleanup drives the failures after a file was
// created: a stream that ends mid-entry, a close that fails, and a cleanup
// that cannot remove the partial file.
func TestImportDrivesCopyAndPartialCleanup(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, dest string, opts ImportOptions, r io.Reader, want ...string) {
		t.Helper()
		_, err := Import(r, dest, Default(), opts)
		if err == nil {
			t.Fatalf("Import = nil, want an error holding %v", want)
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("Import error = %q, want it to hold %q", err, w)
			}
		}
	}

	t.Run("a stream that ends mid-entry", func(t *testing.T) {
		t.Parallel()
		whole := writeArchive(t, regularEntry("daemon/big", strings.Repeat("x", 4096)))
		// The first 512 bytes are the header; 100 bytes of the body follow it,
		// so the stream ends inside the entry.
		write(t, t.TempDir(), ImportOptions{}, bytes.NewReader(whole[:512+100]), "writing \"daemon/big\"")
	})

	t.Run("a close that fails", func(t *testing.T) {
		t.Parallel()
		opts := ImportOptions{ops: opsWith(func(o *townOps) {
			o.openFile = func(string, int, fs.FileMode) (io.WriteCloser, error) {
				return closeFailWriter{}, nil
			}
		})}
		write(t, t.TempDir(), opts, bytes.NewReader(writeArchive(t, regularEntry("daemon/x", "x"))), "writing \"daemon/x\"", "close failed")
	})

	t.Run("a partial file that cannot be removed", func(t *testing.T) {
		t.Parallel()
		opts := ImportOptions{ops: opsWith(func(o *townOps) {
			o.openFile = func(string, int, fs.FileMode) (io.WriteCloser, error) {
				return closeFailWriter{}, nil
			}
			o.remove = func(string) error { return errors.New("remove failed") }
		})}
		write(t, t.TempDir(), opts, bytes.NewReader(writeArchive(t, regularEntry("daemon/x", "x"))), "removing the partial file")
	})
}
