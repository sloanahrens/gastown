package dashboard

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// holdNow is the instant every case below reads at, so an age is the mtime the
// case wrote away from it and nothing else.
var holdNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// writeHold puts a hold file in a temp town and sets its mtime, so the age the
// reader reports is the case's and not the run's (gt-zzkhh).
func writeHold(t *testing.T, town, body string, at time.Time) string {
	t.Helper()

	path := filepath.Join(town, "seat-refill.hold")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing the hold file: %v", err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("setting the hold file's mtime: %v", err)
	}
	return path
}

// The chip's reading, case by case: a town with no hold file is not held, a
// file gives its first line and its age, and a path that cannot be read is
// neither present nor absent, so the page can say it cannot tell
// (gt-zzkhh).
func TestHoldReaderReadsTheTownRootHoldFile(t *testing.T) {
	t.Parallel()

	longNote := strings.Repeat("a", 200) + "\nsecond line the page must not print"
	cases := []struct {
		name    string
		setUp   func(t *testing.T, town string)
		present bool
		note    string
		age     int64
		wantErr string
	}{
		{
			name:  "no hold file: the town is dispatching",
			setUp: func(*testing.T, string) {},
		},
		{
			name: "a hold file: its first line and the age from its mtime",
			setUp: func(t *testing.T, town string) {
				writeHold(t, town, "operator: paused for the night\n", holdNow.Add(-2*time.Hour))
			},
			present: true,
			note:    "operator: paused for the night",
			age:     2 * 60 * 60,
		},
		{
			name: "an empty hold file: the hold stands with no note",
			setUp: func(t *testing.T, town string) {
				writeHold(t, town, "", holdNow)
			},
			present: true,
		},
		{
			name: "a long note: cut to the cap, the second line never printed",
			setUp: func(t *testing.T, town string) {
				writeHold(t, town, longNote, holdNow)
			},
			present: true,
			note:    strings.Repeat("a", holdNoteMax-1) + "…",
		},
		{
			name: "a path that is not a regular file: neither present nor absent",
			setUp: func(t *testing.T, town string) {
				if err := os.Mkdir(filepath.Join(town, "seat-refill.hold"), 0o755); err != nil {
					t.Fatalf("putting a directory at the hold path: %v", err)
				}
			},
			// The exact refusal, not merely some error: a directory read without
			// the regular-file guard fails as "is a directory", which is the
			// branch this case has to prove is not the one taken.
			wantErr: errHoldNotRegular.Error(),
		},
		{
			name: "an mtime after the read: the age is not negative",
			setUp: func(t *testing.T, town string) {
				writeHold(t, town, "held\n", holdNow.Add(time.Hour))
			},
			present: true,
			note:    "held",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			town := t.TempDir()
			tc.setUp(t, town)
			h := NewHoldReader(town).Read(holdNow)
			if h == nil {
				t.Fatal("a reader wired to a town reported nothing")
			}
			if tc.wantErr != "" {
				if h.Error != tc.wantErr {
					t.Errorf("an unreadable hold path reports %q, want %q", h.Error, tc.wantErr)
				}
				if h.Present != nil {
					t.Errorf("an unreadable hold path says present %v, want neither present nor absent", *h.Present)
				}
				return
			}
			if h.Present == nil {
				t.Fatalf("a readable town answers neither present nor absent: error %q", h.Error)
			}
			if *h.Present != tc.present {
				t.Errorf("present = %v, want %v", *h.Present, tc.present)
			}
			if h.Note != tc.note {
				t.Errorf("note = %q, want %q", h.Note, tc.note)
			}
			if h.AgeSeconds != tc.age {
				t.Errorf("age = %ds, want %ds", h.AgeSeconds, tc.age)
			}
			if h.Error != "" {
				t.Errorf("a readable town reports error %q", h.Error)
			}
			// The note is drawn in a title, so the cap is the contract: a note
			// the page prints whole must be no longer than the cap in runes.
			if n := utf8.RuneCountInString(h.Note); n > holdNoteMax {
				t.Errorf("the note runs %d runes, over the %d cap", n, holdNoteMax)
			}
		})
	}
}

// holdBadReader is a hold file that opens and then fails to be read: the disk
// error a test cannot make a real file produce.
type holdBadReader struct{}

func (holdBadReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (holdBadReader) Close() error             { return nil }

// A read that fails is an error state and never a clear town: a hold that could
// not be read is one nothing has ruled out, and the chip says so with "hold?"
// (gt-ifijm). The open and read failures are injected, since no permission bit
// makes a file fail on demand here.
func TestHoldReaderReadFailureIsNeverAClearTown(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		open func(string) (io.ReadCloser, error)
	}{
		{"the file will not open", func(string) (io.ReadCloser, error) { return nil, errors.New("permission denied") }},
		{"the file will not read", func(string) (io.ReadCloser, error) { return holdBadReader{}, nil }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			town := t.TempDir()
			writeHold(t, town, "held\n", holdNow)
			r := NewHoldReader(town)
			r.open = tc.open

			h := r.Read(holdNow)
			if h == nil {
				t.Fatal("a failed read reported nothing")
			}
			if h.Error == "" {
				t.Error("a failed read reports no error")
			}
			if h.Present != nil {
				t.Errorf("a failed read says present %v, want neither present nor absent", *h.Present)
			}
		})
	}
}

// A reader with no town told to it reports nothing rather than reading a hold
// file from the process's working directory.
func TestHoldReaderWithNoTownReportsNothing(t *testing.T) {
	t.Parallel()

	if h := NewHoldReader("").Read(holdNow); h != nil {
		t.Errorf("a reader with no town reported %+v", h)
	}
}

// The reading reaches /api/state, and a nil read leaves the last one standing:
// a chip drawn from a state the poller never filled would be a hold the page
// invented (gt-zzkhh).
func TestHoldReachesTheAPI(t *testing.T) {
	t.Parallel()

	present := true
	read := &Hold{Present: &present, Note: "operator: paused", AgeSeconds: 7200}
	h := NewHub(Config{Hold: func() *Hold { return read }})
	h.pollHold()

	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/state = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"hold"`, `"present":true`, `"note":"operator: paused"`, `"age_seconds":7200`} {
		if !strings.Contains(body, want) {
			t.Errorf("/api/state does not carry %s: %s", want, body)
		}
	}

	if h.State().Hold != read {
		t.Errorf("the hub holds %+v, want the reader's %+v", h.State().Hold, read)
	}
	read = nil
	h.pollHold()
	if got := h.State().Hold; got == nil || !*got.Present {
		t.Errorf("a nil read replaced the last state with %+v", got)
	}
}

// The hold file is written by hand, so the poll keeps the minute clock the
// other slow readers use.
func TestHoldEveryDefaultsToAMinute(t *testing.T) {
	t.Parallel()

	var c Config
	c.defaults()
	if c.HoldEvery != 60*time.Second {
		t.Errorf("HoldEvery defaults to %v, want 1m", c.HoldEvery)
	}
}
