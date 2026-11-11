package forgejocreate

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/steveyegge/gastown/internal/forgejoprov"
)

// faultFS is a seed filesystem that fails one name on Open and one name on Read. A real disk
// rarely produces those failures on demand, so the refusal branches are driven through it.
type faultFS struct {
	base      fs.FS
	openFails string
	readFails string
}

// Open fails openFails outright, and hands readFails back as a file whose Read fails.
func (f faultFS) Open(name string) (fs.File, error) {
	if name == f.openFails {
		return nil, errors.New("read denied")
	}
	file, err := f.base.Open(name)
	if err != nil {
		return nil, err
	}
	if name == f.readFails {
		return failingRead{file}, nil
	}
	return file, nil
}

// failingRead is a file whose Read always fails part way through.
type failingRead struct{ fs.File }

// Read fails every call, as a disk that dies mid-file does.
func (failingRead) Read([]byte) (int, error) { return 0, errors.New("read failed mid-file") }

// TestCollectSeedRefusals gives each refusal of the walk a row. Each row names the cause the
// refusal text must carry and the path it must name.
func TestCollectSeedRefusals(t *testing.T) {
	t.Parallel()
	hello := &fstest.MapFile{Data: []byte("hello")}
	cases := []struct {
		name     string
		fsys     fs.FS
		wantText []string
	}{
		{
			name:     "subdirectory that cannot be read",
			fsys:     faultFS{base: fstest.MapFS{"sub/a.txt": hello}, openFails: "sub"},
			wantText: []string{"seed path sub", "read denied"},
		},
		{
			name:     "file that cannot be opened",
			fsys:     faultFS{base: fstest.MapFS{"a.txt": hello}, openFails: "a.txt"},
			wantText: []string{"seed file a.txt", "read denied"},
		},
		{
			name:     "file that fails mid-read",
			fsys:     faultFS{base: fstest.MapFS{"a.txt": hello}, readFails: "a.txt"},
			wantText: []string{"seed file a.txt", "read failed mid-file"},
		},
		{
			name: "symlink entry is refused by name",
			fsys: fstest.MapFS{
				"a.txt": hello,
				"link":  &fstest.MapFile{Data: []byte("a.txt"), Mode: fs.ModeSymlink},
			},
			wantText: []string{"seed file link", "not a regular file"},
		},
		{
			name:     "file over the limit is refused by name",
			fsys:     fstest.MapFS{"big.bin": {Data: make([]byte, MaxFileBytes+1)}},
			wantText: []string{"seed file big.bin", "is over the limit"},
		},
		{
			name:     "no files",
			fsys:     fstest.MapFS{},
			wantText: []string{"holds no files"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files, err := collectSeed("seed", tc.fsys)
			if err == nil {
				t.Fatalf("collectSeed succeeded with %d files; want a refusal", len(files))
			}
			for _, want := range tc.wantText {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("collectSeed error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// TestUnreachableForgejoRefusesTheRun: a request that fails at the transport refuses the run at
// the step it fails, and the error says so.
func TestUnreachableForgejoRefusesTheRun(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		configure func(*fakeForgejo)
		run       func(*forgejoprov.Client) error
	}{
		{
			name:      "the read before the create fails",
			configure: func(f *fakeForgejo) { f.down = true },
			run:       func(c *forgejoprov.Client) error { return CreateRepo(c, testOwner, testName) },
		},
		{
			name:      "the create fails",
			configure: func(f *fakeForgejo) { f.writesDown = true },
			run:       func(c *forgejoprov.Client) error { return CreateRepo(c, testOwner, testName) },
		},
		{
			name:      "the seed commit fails",
			configure: func(f *fakeForgejo) { f.writesDown = true },
			run: func(c *forgejoprov.Client) error {
				_, err := Seed(c, testOwner, testName, testAuthor, []File{{Path: "a", Data: []byte("a")}})
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t, tc.configure)
			err := tc.run(f.client(t))
			if err == nil || !strings.Contains(err.Error(), "request failed") {
				t.Fatalf("error = %v, want the transport failure named", err)
			}
		})
	}
}
