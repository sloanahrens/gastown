package hostload

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the unit tier, so a goroutine outliving its test fails the run.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// zone is the reading's own zone, six hours behind UTC.
var zone = time.FixedZone("CST", -6*3600)

// TestHostLoadParse reads both host forms and refuses every other one, naming
// the text's start in the error.
func TestHostLoadParse(t *testing.T) {
	t.Parallel()
	good := [3]float64{21.53, 22.70, 21.14}
	for _, c := range []struct {
		text string
		want [3]float64
		bad  bool
	}{
		{"{ 21.53 22.70 21.14 }\n", good, false},
		{"21.53 22.70 21.14 3/900 12345\n", good, false},
		{"{ 21.53 22.70 21.14", [3]float64{}, true},
		{"{ 21.53 22.70 }", [3]float64{}, true},
		{"{ 21.53 22.70 busy }", [3]float64{}, true},
		{"21.53 22.70 21.14", [3]float64{}, true},
		{"21.53 22.70 21.14 900 12345", [3]float64{}, true},
		{"21.53 22.70 x 3/900 12345", [3]float64{}, true},
	} {
		got, err := Parse(c.text)
		if c.bad {
			if err == nil || !strings.Contains(err.Error(), c.text[:min(len(c.text), 40)]) {
				t.Errorf("Parse(%q) = %v, %v: want an error naming the start", c.text, got, err)
			}
		} else if err != nil || got != c.want {
			t.Errorf("Parse(%q) = %v, %v: want %v", c.text, got, err, c.want)
		}
	}
}

// TestHostLoadLine fixes a reading and its zone, so the rendered record can be
// compared whole.
func TestHostLoadLine(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 16, 24, 0, 0, zone)
	want := "2026-11-20T16:24:00-06:00 load1=21.53 load5=22.70 load15=21.14 ncpu=24"
	if got := Line(now, [3]float64{21.53, 22.70, 21.14}, 24); got != want {
		t.Errorf("Line = %q, want %q", got, want)
	}
}

// TestHostLoadAppend adds two lines to the 20th's file, opens a second file on
// the 21st, and leaves the 20th's exactly as it was.
func TestHostLoadAppend(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "hostload")
	for _, c := range []struct {
		at   time.Time
		line string
	}{
		{time.Date(2026, 11, 20, 16, 24, 0, 0, zone), "first"},
		{time.Date(2026, 11, 20, 23, 59, 0, 0, zone), "second"},
		{time.Date(2026, 11, 21, 0, 1, 0, 0, zone), "third"},
	} {
		if err := Append(dir, c.at, c.line); err != nil {
			t.Fatalf("Append(%s, %q): %v", c.at, c.line, err)
		}
	}
	for name, want := range map[string]string{
		"2026-11-20.log": "first\nsecond\n",
		"2026-11-21.log": "third\n",
	} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != want {
			t.Errorf("%s = %q, %v: want %q", name, data, err, want)
		}
	}
}

// failingFile fails every write, as a full disk does.
type failingFile struct{}

func (failingFile) Write([]byte) (int, error) { return 0, errors.New("disk full") }
func (failingFile) Close() error              { return nil }

// TestHostLoadAppendRefuses holds each way a write cannot happen: a directory
// that cannot be made, a day file that cannot be opened, and a write that
// fails after the open.
func TestHostLoadAppendRefuses(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 16, 24, 0, 0, zone)
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "logs", "2026-11-20.log"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{Append(filepath.Join(tmp, "file"), now, "x"), "creating host load directory"},
		{Append(filepath.Join(tmp, "logs"), now, "x"), "opening host load file"},
		{appendLine(tmp, now, "x", seams{os.MkdirAll, func(string, int, os.FileMode) (io.WriteCloser, error) {
			return failingFile{}, nil
		}}), "appending to host load file"},
	} {
		if c.err == nil || !strings.Contains(c.err.Error(), c.want) {
			t.Errorf("Append = %v: want %q", c.err, c.want)
		}
	}
}
