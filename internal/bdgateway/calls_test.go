package bdgateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// TestCallEntryWrite is the bead's box: a line written before the write key
// existed decodes to an unknown write, and a line carrying the key decodes to
// its value.
func TestCallEntryWrite(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
		want string
	}{
		{
			name: "no write key is unknown",
			line: `{"ts":"2026-11-18T10:00:00Z","class":"read","caller":"pre-change","verb":"show","flags":[],"beads":[],"exit_code":0,"duration_ms":5}`,
			want: "unknown",
		},
		{
			name: "write true",
			line: `{"ts":"2026-11-18T10:00:00Z","class":"write","caller":"wrote","verb":"create","write":true,"flags":[],"beads":[],"exit_code":0,"duration_ms":5}`,
			want: "true",
		},
		{
			name: "write false",
			line: `{"ts":"2026-11-18T10:00:00Z","class":"read","caller":"read","verb":"show","write":false,"flags":[],"beads":[],"exit_code":0,"duration_ms":5}`,
			want: "false",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeCallLog(t, dir, tc.line)
			got, err := ReadCalls(dir, time.Time{}, 10)
			if err != nil {
				t.Fatalf("ReadCalls: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("entries = %d, want 1", len(got))
			}
			gotWrite := "unknown"
			if w := got[0].Write; w != nil {
				gotWrite = fmt.Sprint(*w)
			}
			if gotWrite != tc.want {
				t.Errorf("Write = %s, want %s", gotWrite, tc.want)
			}
		})
	}
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

// TestReadCallsWindow is acceptance box 1 of the tail read: a window that opens
// on a line boundary keeps its first line, and a window that opens inside a
// line drops that partial line, even when it would parse whole.
func TestReadCallsWindow(t *testing.T) {
	t.Parallel()
	first := callLine(t, callLogBase.Add(time.Second), "first-whole-line")
	fragment := callLine(t, callLogBase, "cut-fragment")
	cases := []struct {
		name   string
		before string // the bytes ahead of the window; the window is the last 8 MiB
		window string // the window's first line, then whole lines
		want   []string
	}{
		{
			name:   "window opens on a line boundary",
			before: callLine(t, callLogBase, "beyond-the-window") + "\n",
			window: first + "\n",
			want:   []string{"first-whole-line"},
		},
		{
			name:   "window opens inside a line",
			before: callLine(t, callLogBase, "beyond-the-window"),
			window: fragment + "\n" + first + "\n",
			want:   []string{"first-whole-line"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// One unparsable filler line pads the window to exactly maxCallReadBytes.
			pad := maxCallReadBytes - int64(len(tc.window))
			content := tc.before + tc.window + strings.Repeat("y", int(pad)-1) + "\n"
			if err := os.WriteFile(filepath.Join(dir, DefaultCallLogName), []byte(content), 0o600); err != nil {
				t.Fatalf("write call log: %v", err)
			}

			got, err := ReadCalls(dir, time.Time{}, 10)
			if err != nil {
				t.Fatalf("ReadCalls: %v", err)
			}
			if !slices.Equal(callers(got), tc.want) {
				t.Errorf("callers = %v, want %v", callers(got), tc.want)
			}
		})
	}

	t.Run("a log that shrank after it was measured reads as empty", func(t *testing.T) {
		// The file measures past the window but reads back no bytes.
		got, err := readCallLogTail(&failingCallFile{})
		if err != nil {
			t.Fatalf("readCallLogTail: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("readCallLogTail = %q, want no bytes", got)
		}
	})
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
		// The window starts at the fragment's first byte: the "zzz" before it is
		// outside the window. The fragment is a whole entry that would parse, but
		// the window opens inside a line, so it must be dropped, not served.
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

// getCalls drives GET /v1/calls with a token, if any, and the query string.
func getCalls(t *testing.T, g *Gateway, token, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, callsPath+query, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	g.ServeHTTP(rec, req)
	return rec
}

// writePodLog makes the per-pod directory pod under dir and writes its call log.
func writePodLog(t *testing.T, dir, pod string, lines ...string) string {
	t.Helper()
	podDir := filepath.Join(dir, pod)
	if err := os.MkdirAll(podDir, 0o755); err != nil {
		t.Fatalf("make pod directory: %v", err)
	}
	writeCallLog(t, podDir, lines...)
	return podDir
}

// TestReadCallsPodsMerge is acceptance box 1: the root log and every pod's log
// come back as one list, oldest first, with since and limit applied after the
// merge.
func TestReadCallsPodsMerge(t *testing.T) {
	t.Parallel()
	at := func(s int) time.Time { return callLogBase.Add(time.Duration(s) * time.Second) }
	dir := t.TempDir()
	writeCallLog(t, dir,
		callLine(t, at(1), "root-1"),
		callLine(t, at(5), "root-5"),
	)
	// Each pod's file is out of stamp order too, so the merge must sort it.
	writePodLog(t, dir, "pod-a",
		callLine(t, at(4), "a-4"),
		callLine(t, at(2), "a-2"),
	)
	writePodLog(t, dir, "pod-b",
		callLine(t, at(0), "b-0"),
		callLine(t, at(3), "b-3"),
		callLine(t, at(6), "b-6"),
	)

	t.Run("every log merged oldest first, since excluded", func(t *testing.T) {
		got, err := ReadCalls(dir, callLogBase, 10)
		if err != nil {
			t.Fatalf("ReadCalls: %v", err)
		}
		if want := []string{"root-1", "a-2", "b-3", "a-4", "root-5", "b-6"}; !slices.Equal(callers(got), want) {
			t.Errorf("callers = %v, want %v", callers(got), want)
		}
	})

	t.Run("since and limit apply after the merge", func(t *testing.T) {
		// a-2 is at since, so it is out; the oldest three after it across all
		// the logs are b-3, a-4 and root-5.
		got, err := ReadCalls(dir, at(2), 3)
		if err != nil {
			t.Fatalf("ReadCalls: %v", err)
		}
		if want := []string{"b-3", "a-4", "root-5"}; !slices.Equal(callers(got), want) {
			t.Errorf("callers = %v, want %v", callers(got), want)
		}
	})

	t.Run("limit keeps the oldest after since across every log", func(t *testing.T) {
		// root-1 is at since, so the oldest after it is a-2 in pod-a, not the
		// root log's own next entry.
		got, err := ReadCalls(dir, at(1), 1)
		if err != nil {
			t.Fatalf("ReadCalls: %v", err)
		}
		if want := []string{"a-2"}; !slices.Equal(callers(got), want) {
			t.Errorf("callers = %v, want %v", callers(got), want)
		}
	})

	t.Run("a missing root log still reads the pods", func(t *testing.T) {
		podsOnly := t.TempDir()
		writePodLog(t, podsOnly, "pod-a", callLine(t, at(2), "a-2"))
		got, err := ReadCalls(podsOnly, callLogBase, 10)
		if err != nil {
			t.Fatalf("ReadCalls: %v", err)
		}
		if want := []string{"a-2"}; !slices.Equal(callers(got), want) {
			t.Errorf("callers = %v, want %v", callers(got), want)
		}
	})

	t.Run("only one level of pod directories is read", func(t *testing.T) {
		nested := t.TempDir()
		writePodLog(t, nested, filepath.Join("pod-a", "deeper"), callLine(t, at(2), "too-deep"))
		got, err := ReadCalls(nested, callLogBase, 10)
		if err != nil {
			t.Fatalf("ReadCalls: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("callers = %v, want none", callers(got))
		}
	})
}

// TestReadCallsPodsErrors is acceptance box 2: a pod log that cannot be read is
// an error naming its directory, never a skipped pod; an empty pod directory
// adds nothing.
func TestReadCallsPodsErrors(t *testing.T) {
	t.Parallel()

	t.Run("an unreadable pod log is an error naming its directory", func(t *testing.T) {
		dir := t.TempDir()
		writeCallLog(t, dir, callLine(t, callLogBase, "root"))
		podDir := filepath.Join(dir, "pod-b")
		// A directory where the pod's log should be: it opens, and reading it fails.
		if err := os.MkdirAll(filepath.Join(podDir, DefaultCallLogName), 0o755); err != nil {
			t.Fatalf("make unreadable pod log: %v", err)
		}
		_, err := ReadCalls(dir, time.Time{}, 10)
		if err == nil {
			t.Fatal("ReadCalls error = nil, want the unreadable pod log refused")
		}
		if !strings.Contains(err.Error(), podDir) {
			t.Errorf("error %q does not name the pod directory %s", err, podDir)
		}
	})

	t.Run("an empty pod directory adds nothing", func(t *testing.T) {
		dir := t.TempDir()
		writeCallLog(t, dir, callLine(t, callLogBase, "root"))
		if err := os.MkdirAll(filepath.Join(dir, "pod-c"), 0o755); err != nil {
			t.Fatalf("make empty pod directory: %v", err)
		}
		got, err := ReadCalls(dir, time.Time{}, 10)
		if err != nil {
			t.Fatalf("ReadCalls: %v", err)
		}
		if want := []string{"root"}; !slices.Equal(callers(got), want) {
			t.Errorf("callers = %v, want %v", callers(got), want)
		}
	})

	t.Run("an unreadable root log is an error and no pods are read", func(t *testing.T) {
		dir := t.TempDir()
		writePodLog(t, dir, "pod-a", callLine(t, callLogBase, "from-a"))
		// A directory where the root log should be: it opens, and reading it fails.
		if err := os.MkdirAll(filepath.Join(dir, DefaultCallLogName), 0o755); err != nil {
			t.Fatalf("make unreadable root log: %v", err)
		}
		if got, err := ReadCalls(dir, time.Time{}, 10); err == nil {
			t.Errorf("ReadCalls = %v, want the unreadable root log refused", callers(got))
		}
	})

	t.Run("a pod directory listing that fails is an error", func(t *testing.T) {
		boom := errors.New("listing refused")
		failListing := func(string) ([]os.DirEntry, error) { return nil, boom }
		if _, err := readCalls(t.TempDir(), time.Time{}, 10, failListing); !errors.Is(err, boom) {
			t.Errorf("readCalls error = %v, want %v", err, boom)
		}
	})

	t.Run("a log directory that is a file is an error", func(t *testing.T) {
		notADir := filepath.Join(t.TempDir(), "log-dir")
		if err := os.WriteFile(notADir, nil, 0o600); err != nil {
			t.Fatalf("write placeholder file: %v", err)
		}
		if got, err := ReadCalls(notADir, time.Time{}, 10); err == nil {
			t.Errorf("ReadCalls = %v, want the file refused as a log directory", callers(got))
		}
	})
}

// TestServeCallsShowsEveryPod is the bead's goal: a gateway whose own log is one
// pod's directory answers GET /v1/calls with every pod's calls when it is given
// the shared root, and with its own log alone when it is not.
func TestServeCallsShowsEveryPod(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	podA := writePodLog(t, root, "pod-a", callLine(t, callLogBase.Add(time.Second), "from-a"))
	writePodLog(t, root, "pod-b", callLine(t, callLogBase.Add(2*time.Second), "from-b"))
	log, err := NewCallLog(CallLogOptions{
		Dir:          podA,
		Policy:       mustPolicy(t),
		BeadPrefixes: []string{"gt"},
	})
	if err != nil {
		t.Fatalf("NewCallLog: %v", err)
	}
	t.Cleanup(func() {
		if err := log.Close(); err != nil {
			t.Errorf("close call log: %v", err)
		}
	})

	gatewayWith := func(callsDir string) *Gateway {
		t.Helper()
		g, err := NewGateway(GatewayOptions{
			Auth:       testAuthenticator(t),
			Policy:     mustPolicy(t),
			Workspaces: mustStore(t),
			Log:        log,
			CallsDir:   callsDir,
			Runner:     exitByVerb{"show": 0},
			Caps:       fixedCaps{},
		})
		if err != nil {
			t.Fatalf("NewGateway: %v", err)
		}
		return g
	}

	t.Run("the shared root shows every pod's calls", func(t *testing.T) {
		body := decodeCalls(t, getCalls(t, gatewayWith(root), testReadToken, ""))
		if want := []string{"from-a", "from-b"}; !slices.Equal(callers(body.Calls), want) {
			t.Errorf("callers = %v, want %v", callers(body.Calls), want)
		}
	})

	t.Run("no CallsDir reads the log's own directory alone", func(t *testing.T) {
		body := decodeCalls(t, getCalls(t, gatewayWith(""), testReadToken, ""))
		if want := []string{"from-a"}; !slices.Equal(callers(body.Calls), want) {
			t.Errorf("callers = %v, want %v", callers(body.Calls), want)
		}
	})
}

// decodeCalls returns the 200 body GET /v1/calls answered, failing the test
// with the body when the status is not 200.
func decodeCalls(t *testing.T, rec *httptest.ResponseRecorder) callsBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var body callsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode calls: %v", err)
	}
	return body
}

// TestCallsEndpointQuery is acceptance box 1: the endpoint returns the entries
// after since, oldest first, honours limit and its default, and refuses a limit
// over 500 and a bad since.
func TestCallsEndpointQuery(t *testing.T) {
	t.Parallel()
	t0 := callLogBase
	t1 := t0.Add(time.Second)
	t2 := t0.Add(2 * time.Second)
	t3 := t0.Add(3 * time.Second)
	dir := t.TempDir()
	log := newTestCallLogWithDir(t, dir)
	// File order is deliberately not stamp order, which Log can produce when a
	// writer is preempted between stamping a call and queueing its line.
	writeCallLog(t, dir,
		callLine(t, t3, "third"),
		"{not json",
		callLine(t, t1, "at-since"),
		callLine(t, t2, "second"),
	)
	g := newTestGateway(t, mustStore(t), log, exitByVerb{"show": 0})

	sinceQuery := func(at time.Time) string {
		return "?" + url.Values{"since": {at.Format(time.RFC3339)}}.Encode()
	}

	t.Run("after since, oldest first", func(t *testing.T) {
		body := decodeCalls(t, getCalls(t, g, testReadToken, sinceQuery(t1)))
		if want := []string{"second", "third"}; !slices.Equal(callers(body.Calls), want) {
			t.Errorf("callers = %v, want %v", callers(body.Calls), want)
		}
		first := body.Calls[0]
		if first.TS != t2.Format(time.RFC3339Nano) || first.Verb != "show" ||
			!slices.Equal(first.Flags, []string{"--json"}) || !slices.Equal(first.Beads, []string{"gt-qxllxo"}) {
			t.Errorf("entry = %+v, want the fields the log wrote", first)
		}
	})

	t.Run("limit keeps the oldest after since", func(t *testing.T) {
		q := "?" + url.Values{"since": {t0.Format(time.RFC3339)}, "limit": {"1"}}.Encode()
		body := decodeCalls(t, getCalls(t, g, testReadToken, q))
		if want := []string{"at-since"}; !slices.Equal(callers(body.Calls), want) {
			t.Errorf("callers = %v, want %v", callers(body.Calls), want)
		}
	})

	t.Run("without since the whole log is served", func(t *testing.T) {
		body := decodeCalls(t, getCalls(t, g, testReadToken, ""))
		if want := []string{"at-since", "second", "third"}; !slices.Equal(callers(body.Calls), want) {
			t.Errorf("callers = %v, want %v", callers(body.Calls), want)
		}
	})

	t.Run("an absent limit is the default", func(t *testing.T) {
		dir := t.TempDir()
		log := newTestCallLogWithDir(t, dir)
		lines := make([]string, 0, defaultCallsLimit+5)
		for i := 0; i < defaultCallsLimit+5; i++ {
			lines = append(lines, callLine(t, callLogBase.Add(time.Duration(i)*time.Second), fmt.Sprintf("n%03d", i)))
		}
		writeCallLog(t, dir, lines...)
		g := newTestGateway(t, mustStore(t), log, exitByVerb{"show": 0})

		body := decodeCalls(t, getCalls(t, g, testReadToken, ""))
		if len(body.Calls) != defaultCallsLimit {
			t.Fatalf("got %d entries, want the default %d", len(body.Calls), defaultCallsLimit)
		}
		if body.Calls[0].Caller != "n000" || body.Calls[len(body.Calls)-1].Caller != fmt.Sprintf("n%03d", defaultCallsLimit-1) {
			t.Errorf("window = %s..%s, want the oldest %d entries",
				body.Calls[0].Caller, body.Calls[len(body.Calls)-1].Caller, defaultCallsLimit)
		}
	})

	t.Run("a limit under the cap is honoured", func(t *testing.T) {
		q := "?" + url.Values{"limit": {"3"}}.Encode()
		body := decodeCalls(t, getCalls(t, g, testReadToken, q))
		if want := []string{"at-since", "second", "third"}; !slices.Equal(callers(body.Calls), want) {
			t.Errorf("callers = %v, want %v", callers(body.Calls), want)
		}
	})

	t.Run("an out-of-bounds or unreadable limit is refused", func(t *testing.T) {
		for _, limit := range []string{fmt.Sprint(maxCallsLimit + 1), "0", "-1", "many", "1.5"} {
			t.Run(limit, func(t *testing.T) {
				q := "?" + url.Values{"limit": {limit}}.Encode()
				rec := getCalls(t, g, testReadToken, q)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
				}
				var refused ErrorPayload
				if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil {
					t.Fatalf("decode refusal: %v", err)
				}
				if refused.Code != codeBadRequest || refused.Message == "" {
					t.Errorf("refusal = %+v, want code %q and a message", refused, codeBadRequest)
				}
			})
		}
	})

	t.Run("a bad since is refused", func(t *testing.T) {
		rec := getCalls(t, g, testReadToken, "?"+url.Values{"since": {"yesterday"}}.Encode())
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
		}
		var refused ErrorPayload
		if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil {
			t.Fatalf("decode refusal: %v", err)
		}
		if refused.Code != codeBadRequest || refused.Message == "" {
			t.Errorf("refusal = %+v, want code %q and a message", refused, codeBadRequest)
		}
	})
}

// TestCallsEndpointLog is acceptance box 2: a missing log answers an empty
// list with 200, an unreadable log answers 500, and a request without a token
// is refused; the body carries the log's dropped counter.
func TestCallsEndpointLog(t *testing.T) {
	t.Parallel()

	t.Run("a missing log is an empty list", func(t *testing.T) {
		dir := t.TempDir()
		log := newTestCallLogWithDir(t, dir)
		// NewCallLog creates the live file; removing it is the volume whose log
		// has not been written yet, which must read as empty, not as a failure.
		if err := os.Remove(filepath.Join(dir, DefaultCallLogName)); err != nil {
			t.Fatalf("remove live log: %v", err)
		}
		g := newTestGateway(t, mustStore(t), log, exitByVerb{"show": 0})

		body := decodeCalls(t, getCalls(t, g, testReadToken, ""))
		if body.Calls == nil {
			t.Fatal("got nil, want an empty list: the endpoint answers [] and never null")
		}
		if len(body.Calls) != 0 {
			t.Errorf("got %d entries, want 0", len(body.Calls))
		}
	})

	t.Run("an unreadable log is a 500 with a fixed message", func(t *testing.T) {
		dir := t.TempDir()
		log := newTestCallLogWithDir(t, dir)
		if err := os.Remove(filepath.Join(dir, DefaultCallLogName)); err != nil {
			t.Fatalf("remove live log: %v", err)
		}
		// A directory at the log's path opens but cannot be read: the failure an
		// operator sees when the log path is wrong.
		if err := os.Mkdir(filepath.Join(dir, DefaultCallLogName), 0o700); err != nil {
			t.Fatalf("mkdir log path: %v", err)
		}
		g := newTestGateway(t, mustStore(t), log, exitByVerb{"show": 0})

		rec := getCalls(t, g, testReadToken, "")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
		}
		var refused ErrorPayload
		if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil {
			t.Fatalf("decode refusal: %v", err)
		}
		if refused.Code != codeInternal || refused.Message != callsLogUnreadableMessage {
			t.Errorf("refusal = %+v, want code %q and the fixed message", refused, codeInternal)
		}
		var served map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &served); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, ok := served["calls"]; ok {
			t.Errorf("a 500 carries a calls field: %s", rec.Body.String())
		}
	})

	t.Run("a request without a token is refused", func(t *testing.T) {
		g := newTestGateway(t, mustStore(t), newTestCallLog(t), exitByVerb{"show": 0})
		rec := getCalls(t, g, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("the body carries the log's dropped counter", func(t *testing.T) {
		dir := t.TempDir()
		writeCallLog(t, dir, callLine(t, callLogBase, "shown"))
		// A writer that cannot store a line is the drop this test counts; the
		// hook makes that failure happen without a disk error.
		log, err := newCallLog(CallLogOptions{
			Dir:          dir,
			Policy:       mustPolicy(t),
			BeadPrefixes: []string{"gt", "hq"},
		}, func([]byte) error { return errors.New("log volume gone") })
		if err != nil {
			t.Fatalf("newCallLog: %v", err)
		}
		log.Log(CallRecord{Argv: []string{"show"}})
		// Close returns the hook's failure, which is the drop being counted.
		_ = log.Close()
		g := newTestGateway(t, mustStore(t), log, exitByVerb{"show": 0})

		rec := getCalls(t, g, testReadToken, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
		}
		// The counter is a pointer, so a body that dropped the field reads as nil
		// rather than the zero a real value could not be told from.
		var body struct {
			Calls   []CallEntry `json:"calls"`
			Dropped *uint64     `json:"dropped"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode calls: %v", err)
		}
		if body.Dropped == nil {
			t.Fatal("the body carries no dropped counter")
		}
		if *body.Dropped != 1 {
			t.Errorf("dropped = %d, want 1", *body.Dropped)
		}
		if want := []string{"shown"}; !slices.Equal(callers(body.Calls), want) {
			t.Errorf("callers = %v, want %v", callers(body.Calls), want)
		}
	})
}
