package taskfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A seat's mode is the MODE marker in its task directory: ModeFiles reads as
// file mode, and every other state as dolt mode, which is what a sandbox with
// no route to Dolt fails loudly on (gt-3qkaws).
func TestNoBeadAccessFileMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// setup writes the task directory's contents; nil leaves it empty.
		setup func(t *testing.T, root string)
		want  bool
	}{
		{
			name:  "the marker the archive stages",
			setup: writeMode(ModeFiles),
			want:  true,
		},
		{
			name:  "a hand-edited marker with a newline",
			setup: writeMode(ModeFiles + "\n"),
			want:  true,
		},
		{
			name:  "a marker padded with spaces",
			setup: writeMode("  " + ModeFiles + "\t"),
			want:  true,
		},
		{
			name:  "a dolt marker",
			setup: writeMode("dolt"),
			want:  false,
		},
		{
			name:  "an empty marker",
			setup: writeMode(""),
			want:  false,
		},
		{
			name:  "a marker with another case",
			setup: writeMode("Files"),
			want:  false,
		},
		{
			name: "no marker",
			want: false,
		},
		{
			name:  "a marker that cannot be read",
			setup: func(t *testing.T, root string) { t.Helper(); mkdirMode(t, root) },
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tc.setup != nil {
				tc.setup(t, root)
			}
			if got := FileMode(root); got != tc.want {
				t.Errorf("FileMode(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// A task directory that is not there at all is dolt mode too, not an error and
// not file mode: a seat with no archive stages no marker.
func TestNoBeadAccessFileModeWithoutATaskDirectory(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "absent")
	if FileMode(root) {
		t.Errorf("FileMode(%q) = true, want false for a missing task directory", root)
	}
}

// ReadFileMode answers dolt mode for a marker that is not there, and refuses
// to guess when one is there but cannot be read: a caller that would open the
// bead store on a false answer must not read an unreadable marker as dolt mode
// (gt-w1mzol).
func TestNoBeadAccessReadFileModeRefusesAnUnreadableMarker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// root derives the task directory from a temporary one; nil uses it as
		// it is.
		root  func(tmp string) string
		setup func(t *testing.T, root string)
		want  bool
		// wantErr is whether the answer must be an error, not a mode.
		wantErr bool
	}{
		{name: "no marker", want: false},
		{name: "absent task directory", root: func(tmp string) string { return filepath.Join(tmp, "absent") }, want: false},
		{name: "a files marker", setup: writeMode(ModeFiles), want: true},
		{name: "a dolt marker", setup: writeMode("dolt"), want: false},
		{name: "an unreadable marker", setup: mkdirMode, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tc.root != nil {
				root = tc.root(root)
			}
			if tc.setup != nil {
				tc.setup(t, root)
			}
			got, err := ReadFileMode(root)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ReadFileMode(%q) = %v, nil; want an error", tc.name, got)
				}
				if want := filepath.Join(root, ModeFile); !strings.Contains(err.Error(), want) {
					t.Errorf("ReadFileMode error = %q, want it to name the marker %s", err, want)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("ReadFileMode(%q) = %v, %v; want %v, nil", tc.name, got, err, tc.want)
			}
		})
	}
}

func writeMode(body string) func(*testing.T, string) {
	return func(t *testing.T, root string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, ModeFile), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func mkdirMode(t *testing.T, root string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(root, ModeFile), 0o755); err != nil {
		t.Fatal(err)
	}
}
