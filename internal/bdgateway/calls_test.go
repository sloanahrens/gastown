package bdgateway

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// callLogBase is the stamp the ReadCalls tests count seconds from.
var callLogBase = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

// callLine renders one log line with the writer's own entry type, so a test
// reads back exactly what Log writes.
func callLine(t *testing.T, at time.Time, caller string) string {
	t.Helper()
	line, err := json.Marshal(entry{
		TS:         at.UTC().Format(time.RFC3339Nano),
		Class:      "read",
		Caller:     caller,
		Verb:       "show",
		Flags:      []string{"--json"},
		Beads:      []string{"gt-qxllxo"},
		ExitCode:   0,
		DurationMS: 5,
	})
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	return string(line)
}

// writeCallLog writes lines to the live file in dir.
func writeCallLog(t *testing.T, dir string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, DefaultCallLogName), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write call log: %v", err)
	}
}

// callers lists the entries' callers, which name each fixture entry.
func callers(entries []CallEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Caller
	}
	return out
}

// TestReadCallsQuery is acceptance box 1: the entries after since, oldest
// first, at most limit, with a malformed line skipped.
func TestReadCallsQuery(t *testing.T) {
	t.Parallel()
	t0 := callLogBase
	t1 := t0.Add(time.Second)
	t2 := t0.Add(2 * time.Second)
	t3 := t0.Add(3 * time.Second)
	dir := t.TempDir()
	// File order is deliberately not stamp order, which Log can produce when a
	// writer is preempted between stamping a call and queueing its line.
	writeCallLog(t, dir,
		callLine(t, t3, "third"),
		"{not json",
		callLine(t, t1, "at-since"),
		callLine(t, t2, "second"),
	)

	t.Run("after since, oldest first, malformed skipped", func(t *testing.T) {
		got, err := ReadCalls(dir, t1, 10)
		if err != nil {
			t.Fatalf("ReadCalls: %v", err)
		}
		if want := []string{"second", "third"}; !slices.Equal(callers(got), want) {
			t.Errorf("callers = %v, want %v", callers(got), want)
		}
		first := got[0]
		if first.TS != t2.Format(time.RFC3339Nano) || first.Class != "read" ||
			first.Verb != "show" || first.ExitCode != 0 || first.DurationMS != 5 ||
			!slices.Equal(first.Flags, []string{"--json"}) || !slices.Equal(first.Beads, []string{"gt-qxllxo"}) {
			t.Errorf("entry = %+v, want the fields the log wrote", first)
		}
	})

	t.Run("limit keeps the oldest after since", func(t *testing.T) {
		got, err := ReadCalls(dir, t0, 1)
		if err != nil {
			t.Fatalf("ReadCalls: %v", err)
		}
		if want := []string{"at-since"}; !slices.Equal(callers(got), want) {
			t.Errorf("callers = %v, want %v", callers(got), want)
		}
	})

	t.Run("a line whose stamp does not parse is skipped", func(t *testing.T) {
		dir := t.TempDir()
		writeCallLog(t, dir,
			`{"ts":"not-a-time","class":"read","caller":"unplaceable"}`,
			callLine(t, t3, "placeable"),
		)
		// A since before the zero time makes a zero stamp pass the window, so
		// only the stamp check can keep the unplaceable line out.
		beforeZero := time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)
		got, err := ReadCalls(dir, beforeZero, 10)
		if err != nil {
			t.Fatalf("ReadCalls: %v", err)
		}
		if want := []string{"placeable"}; !slices.Equal(callers(got), want) {
			t.Errorf("callers = %v, want %v", callers(got), want)
		}
	})

	t.Run("a limit of zero or less is refused", func(t *testing.T) {
		for _, limit := range []int{0, -1} {
			if got, err := ReadCalls(dir, t0, limit); err == nil {
				t.Errorf("ReadCalls(limit=%d) = %v, want a refusal", limit, callers(got))
			}
		}
	})
}

// failingCallFile is a callLogFile whose calls fail when a test says so.
type failingCallFile struct {
	statErr error
	seekErr error
	readErr error
}

func (f *failingCallFile) Stat() (os.FileInfo, error) {
	if f.statErr != nil {
		return nil, f.statErr
	}
	return fakeInfo{size: maxCallReadBytes + 1}, nil
}

func (f *failingCallFile) Seek(int64, int) (int64, error) {
	return 0, f.seekErr
}

func (f *failingCallFile) Read([]byte) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	return 0, io.EOF
}

// TestReadCallsTailErrors drives each failure of the bounded tail read: a file
// that cannot be measured, seeked or read is an error, never a short list.
func TestReadCallsTailErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("log volume gone")
	cases := []struct {
		name string
		file *failingCallFile
	}{
		{"stat", &failingCallFile{statErr: boom}},
		{"seek", &failingCallFile{seekErr: boom}},
		{"read", &failingCallFile{readErr: boom}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := readCallLogTail(tc.file); !errors.Is(err, boom) {
				t.Errorf("readCallLogTail = %v, want %v", err, boom)
			}
		})
	}
}

// TestReadCallsFile is acceptance box 2: a missing log is an empty list, an
// unreadable one is an error, and only the last 8 MiB are read.
func TestReadCallsFile(t *testing.T) {
	t.Parallel()

	t.Run("missing log is an empty list", func(t *testing.T) {
		got, err := ReadCalls(t.TempDir(), time.Time{}, 10)
		if err != nil {
			t.Fatalf("ReadCalls: %v", err)
		}
		if got == nil {
			t.Fatal("got nil, want an empty list: the gateway answers [] and never null")
		}
		if len(got) != 0 {
			t.Errorf("got %d entries, want 0", len(got))
		}
	})

	t.Run("a log path that cannot be opened is an error", func(t *testing.T) {
		// A regular file where the log's directory should be: opening the log
		// under it fails with ENOTDIR, which is not the missing-file answer.
		notADir := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(notADir, nil, 0o600); err != nil {
			t.Fatalf("write placeholder file: %v", err)
		}
		if got, err := ReadCalls(notADir, time.Time{}, 10); err == nil {
			t.Fatalf("ReadCalls = %v, want an error for a log that cannot be opened", callers(got))
		}
	})

	t.Run("unreadable log is an error", func(t *testing.T) {
		dir := t.TempDir()
		// A directory named call.log opens but cannot be read, the failure an
		// operator sees when the log path is wrong.
		if err := os.Mkdir(filepath.Join(dir, DefaultCallLogName), 0o700); err != nil {
			t.Fatalf("mkdir log path: %v", err)
		}
		if got, err := ReadCalls(dir, time.Time{}, 10); err == nil {
			t.Fatalf("ReadCalls = %v, want an error for an unreadable log", callers(got))
		}
	})

	t.Run("only the last 8 MiB are read", func(t *testing.T) {
		dir := t.TempDir()
		// The window starts three bytes into a line ("zzz" + a whole entry), so
		// the first line ReadCalls sees is a line's tail that would parse as an
		// entry held whole: it must be dropped as a fragment, not served.
		fragment := callLine(t, callLogBase, "cut-fragment")
		tail := "\n" + callLine(t, callLogBase.Add(time.Second), "in-the-window") + "\n"
		need := maxCallReadBytes - int64(len(fragment)+len(tail))
		tail += strings.Repeat(strings.Repeat("y", 999)+"\n", int(need/1000))
		if rest := need % 1000; rest > 0 {
			tail += strings.Repeat("y", int(rest)-1) + "\n"
		}
		content := callLine(t, callLogBase, "beyond-the-window") + "\n" + "zzz" + fragment + tail
		if err := os.WriteFile(filepath.Join(dir, DefaultCallLogName), []byte(content), 0o600); err != nil {
			t.Fatalf("write call log: %v", err)
		}

		got, err := ReadCalls(dir, time.Time{}, 10)
		if err != nil {
			t.Fatalf("ReadCalls: %v", err)
		}
		if want := []string{"in-the-window"}; !slices.Equal(callers(got), want) {
			t.Errorf("callers = %v, want %v", callers(got), want)
		}
	})

	t.Run("rotated files are not read", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, DefaultCallLogName+".1"), []byte(callLine(t, callLogBase, "rotated")+"\n"), 0o600); err != nil {
			t.Fatalf("write rotated log: %v", err)
		}
		writeCallLog(t, dir, callLine(t, callLogBase, "live"))

		got, err := ReadCalls(dir, time.Time{}, 10)
		if err != nil {
			t.Fatalf("ReadCalls: %v", err)
		}
		if want := []string{"live"}; !slices.Equal(callers(got), want) {
			t.Errorf("callers = %v, want %v", callers(got), want)
		}
	})

	t.Run("a missing directory is refused", func(t *testing.T) {
		if got, err := ReadCalls("", time.Time{}, 10); err == nil {
			t.Errorf("ReadCalls with no directory = %v, want a refusal", callers(got))
		}
	})
}
