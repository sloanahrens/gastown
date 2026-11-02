package storagesrc

import (
	"errors"
	"io/fs"
	"reflect"
	"testing"
	"testing/fstest"
	"time"

	"github.com/steveyegge/gastown/internal/metering/export"
)

// nov1 is the UTC day every case reports, at midnight.
var nov1 = time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)

const (
	trackerLine = `{"ts":"2026-11-01T02:00:00Z","workspace":"acme","prefix":"gt","rows":12,"bytes":1048576}`
	forgejoLine = `{"ts":"2026-11-01T04:00:00Z","workspace":"acme","repo":"acme/widgets","size_kb":2048}`
)

// dayFile is a fstest.MapFile holding lines as one JSONL day file.
func dayFile(lines ...string) *fstest.MapFile {
	var data []byte
	for _, line := range lines {
		data = append(append(data, line...), '\n')
	}
	return &fstest.MapFile{Data: data}
}

func TestSamplesReadsBothKinds(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"tracker-sizes/2026-11-01.jsonl": dayFile(trackerLine),
		"forgejo-sizes/2026-11-01.jsonl": dayFile(forgejoLine),
	}

	got, err := Samples(fsys)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	want := []export.StorageSample{
		{Workspace: "acme", Kind: "tracker:gt", Bytes: 1048576, Day: nov1},
		{Workspace: "acme", Kind: "repo:acme/widgets", Bytes: 2048 * 1024, Day: nov1},
	}
	if !reflect.DeepEqual(got.Samples, want) {
		t.Errorf("Samples = %+v, want %+v", got.Samples, want)
	}
	if len(got.Absent) != 0 {
		t.Errorf("Absent = %v, want none", got.Absent)
	}
	if len(got.Skipped) != 0 {
		t.Errorf("Skipped = %v, want none", got.Skipped)
	}
}

func TestSamplesSkipsAndCounts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
		line string
		want SkipReason
	}{
		{
			name: "null size_kb",
			path: "forgejo-sizes/2026-11-01.jsonl",
			line: `{"ts":"2026-11-01T04:00:00Z","workspace":"acme","repo":"acme/widgets","size_kb":null}`,
			want: SkipNullSize,
		},
		{
			name: "empty workspace",
			path: "tracker-sizes/2026-11-01.jsonl",
			line: `{"ts":"2026-11-01T02:00:00Z","workspace":"","prefix":"gt","rows":1,"bytes":10}`,
			want: SkipEmptyWorkspace,
		},
		{
			name: "invalid JSON",
			path: "forgejo-sizes/2026-11-01.jsonl",
			line: `{"ts":`,
			want: SkipInvalidJSON,
		},
		{
			name: "negative tracker bytes",
			path: "tracker-sizes/2026-11-01.jsonl",
			line: `{"ts":"2026-11-01T02:00:00Z","workspace":"acme","prefix":"gt","rows":1,"bytes":-1}`,
			want: SkipNegativeSize,
		},
		{
			name: "negative forgejo size",
			path: "forgejo-sizes/2026-11-01.jsonl",
			line: `{"ts":"2026-11-01T04:00:00Z","workspace":"acme","repo":"acme/widgets","size_kb":-5}`,
			want: SkipNegativeSize,
		},
		{
			name: "unparsable ts",
			path: "tracker-sizes/2026-11-01.jsonl",
			line: `{"ts":"yesterday","workspace":"acme","prefix":"gt","rows":1,"bytes":10}`,
			want: SkipUnparsableTS,
		},
		{
			name: "size overflows bytes",
			path: "forgejo-sizes/2026-11-01.jsonl",
			line: `{"ts":"2026-11-01T04:00:00Z","workspace":"acme","repo":"acme/widgets","size_kb":9223372036854775807}`,
			want: SkipSizeOverflow,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Samples(fstest.MapFS{tc.path: dayFile(tc.line)})
			if err != nil {
				t.Fatalf("Samples: %v", err)
			}
			if len(got.Samples) != 0 {
				t.Errorf("Samples = %+v, want none", got.Samples)
			}
			wantSkipped := map[SkipReason]int{tc.want: 1}
			if !reflect.DeepEqual(got.Skipped, wantSkipped) {
				t.Errorf("Skipped = %v, want %v", got.Skipped, wantSkipped)
			}
		})
	}
}

func TestSamplesMissingDirIsReported(t *testing.T) {
	t.Parallel()
	t.Run("missing directory is reported", func(t *testing.T) {
		fsys := fstest.MapFS{
			"tracker-sizes/2026-11-01.jsonl": dayFile(trackerLine),
		}

		got, err := Samples(fsys)
		if err != nil {
			t.Fatalf("Samples: %v", err)
		}
		want := []export.StorageSample{
			{Workspace: "acme", Kind: "tracker:gt", Bytes: 1048576, Day: nov1},
		}
		if !reflect.DeepEqual(got.Samples, want) {
			t.Errorf("Samples = %+v, want %+v", got.Samples, want)
		}
		if wantAbsent := []string{"forgejo-sizes"}; !reflect.DeepEqual(got.Absent, wantAbsent) {
			t.Errorf("Absent = %v, want %v", got.Absent, wantAbsent)
		}
	})

	// A directory that exists but cannot be read must fail the run rather than
	// be reported as absent and bill a month with no Forgejo storage.
	t.Run("unreadable directory is an error", func(t *testing.T) {
		fsys := denyFS{
			inner: fstest.MapFS{
				"tracker-sizes/2026-11-01.jsonl": dayFile(trackerLine),
				"forgejo-sizes/2026-11-01.jsonl": dayFile(forgejoLine),
			},
			deny: "forgejo-sizes",
		}

		if _, err := Samples(fsys); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("Samples error = %v, want fs.ErrPermission", err)
		}
	})
}

// TestSamplesUnreadableDayFileIsAnError drives a day file that exists but
// cannot be read: a directory with an unreadable file must fail the run, not
// silently drop that file's samples.
func TestSamplesUnreadableDayFileIsAnError(t *testing.T) {
	t.Parallel()
	fsys := denyFS{
		inner: fstest.MapFS{
			"tracker-sizes/2026-11-01.jsonl": dayFile(trackerLine),
			"forgejo-sizes/2026-11-01.jsonl": dayFile(forgejoLine),
		},
		deny: "tracker-sizes/2026-11-01.jsonl",
	}

	if _, err := Samples(fsys); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Samples error = %v, want fs.ErrPermission", err)
	}
}

// TestSamplesReadsDayFilesInNameOrder pins the determinism the day files'
// dates depend on, and that a .partial file a run is still writing is not read.
func TestSamplesReadsDayFilesInNameOrder(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"tracker-sizes/2026-11-02.jsonl":         dayFile(`{"ts":"2026-11-02T02:00:00Z","workspace":"acme","prefix":"gt","rows":1,"bytes":2}`),
		"tracker-sizes/2026-11-01.jsonl":         dayFile(`{"ts":"2026-11-01T02:00:00Z","workspace":"acme","prefix":"gt","rows":1,"bytes":1}`),
		"tracker-sizes/2026-11-03.jsonl.partial": dayFile(`{"ts":"2026-11-03T02:00:00Z","workspace":"acme","prefix":"gt","rows":1,"bytes":99}`),
	}

	got, err := Samples(fsys)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(got.Samples) != 2 {
		t.Fatalf("Samples = %+v, want 2", got.Samples)
	}
	if got.Samples[0].Bytes != 1 || got.Samples[1].Bytes != 2 {
		t.Errorf("Samples bytes = %d, %d, want 1, 2", got.Samples[0].Bytes, got.Samples[1].Bytes)
	}
}

// denyFS is a file system whose path deny cannot be opened. It implements only
// fs.FS, so fs.ReadDir and fs.ReadFile fall back to Open and see the error.
type denyFS struct {
	inner fstest.MapFS
	deny  string
}

func (f denyFS) Open(name string) (fs.File, error) {
	if name == f.deny {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	return f.inner.Open(name)
}
