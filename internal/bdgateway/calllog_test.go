package bdgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// fixedNow makes a log's stamps deterministic.
func fixedNow() time.Time {
	return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
}

// callLogPolicy is the verbs file the log's scanner reads: enough verbs to settle
// the command words the tests use, and one longer path so "dep add" scans as
// two words.
func callLogPolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := ParsePolicy(strings.NewReader(`{
		"source": "test",
		"refused_flags": ["db"],
		"verbs": [
			{"path": "show", "class": "read"},
			{"path": "list", "class": "read"},
			{"path": "create", "class": "write"},
			{"path": "update", "class": "write"},
			{"path": "dep", "class": "write"},
			{"path": "dep add", "class": "write"}
		]
	}`))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	return p
}

func testOptions(t *testing.T) CallLogOptions {
	t.Helper()
	return CallLogOptions{
		Dir:          t.TempDir(),
		Policy:       callLogPolicy(t),
		BeadPrefixes: []string{"gt", "hq"},
		Now:          fixedNow,
	}
}

// readLines reads path and returns its non-empty JSON lines.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func decodeLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return got
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestCallLogRedaction is acceptance box 1: one JSONL line, exactly the allowed
// fields, and no flag value from argv in it.
func TestCallLogRedaction(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	opts.Buffer = 4
	l, err := NewCallLog(opts)
	if err != nil {
		t.Fatalf("NewCallLog: %v", err)
	}
	// The last three tokens are values of flags: none may reach the file.
	const secretReason = "secret-reason-body"
	const secretTitle = "sk-live-abcdef123456"
	l.Log(CallRecord{
		Class:    "daemon",
		Caller:   "daemon.listLocalRoundRequests",
		Argv:     []string{"update", "gt-itjuwu", "--status=open", "--reason", secretReason, "--title", secretTitle},
		ExitCode: 0,
		Duration: 35 * time.Millisecond,
	})
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(opts.Dir, DefaultCallLogName)
	lines := readLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), lines)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, secret := range []string{secretReason, secretTitle} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("line holds flag value %q: %s", secret, raw)
		}
	}

	got := decodeLine(t, lines[0])
	wantKeys := []string{"beads", "caller", "class", "duration_ms", "exit_code", "flags", "ts", "verb"}
	if !reflect.DeepEqual(keys(got), wantKeys) {
		t.Errorf("fields = %v, want %v", keys(got), wantKeys)
	}
	// Every value below is an operand or a flag name, never a flag's value.
	checks := []struct {
		field string
		want  any
	}{
		{"ts", "2026-11-13T12:00:00Z"},
		{"class", "daemon"},
		{"caller", "daemon.listLocalRoundRequests"},
		{"verb", "update"},
		{"exit_code", float64(0)},
		{"duration_ms", float64(35)},
	}
	for _, c := range checks {
		if got[c.field] != c.want {
			t.Errorf("%s = %v, want %v", c.field, got[c.field], c.want)
		}
	}
	if want := []any{"--status", "--reason", "--title"}; !reflect.DeepEqual(got["flags"], want) {
		t.Errorf("flags = %v, want %v", got["flags"], want)
	}
	if want := []any{"gt-itjuwu"}; !reflect.DeepEqual(got["beads"], want) {
		t.Errorf("beads = %v, want %v", got["beads"], want)
	}
}

// TestCallLogDrop is acceptance box 2's first half: a full buffer drops the line
// and counts it instead of blocking the caller.
func TestCallLogDrop(t *testing.T) {
	t.Parallel()
	taken := make(chan struct{}, 1)
	unblock := make(chan struct{})
	hook := func([]byte) error {
		taken <- struct{}{}
		<-unblock
		return nil
	}
	opts := testOptions(t)
	opts.Buffer = 1
	l, err := newCallLog(opts, hook)
	if err != nil {
		t.Fatalf("newCallLog: %v", err)
	}
	rec := CallRecord{Class: "daemon", Caller: "c", Argv: []string{"show", "gt-itjuwu"}, Duration: time.Millisecond}

	l.Log(rec) // the writer takes this one and parks inside the hook
	select {
	case <-taken: // the writer holds it, so the queue is empty and the buffer free
	case <-waitCtx(t).Done():
		t.Fatal("the writer did not take the first line")
	}
	l.Log(rec) // fills the one-slot buffer
	l.Log(rec) // full: dropped
	l.Log(rec) // full: dropped
	close(unblock)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := l.Stats()
	if got.Calls != 4 {
		t.Errorf("calls = %d, want 4", got.Calls)
	}
	if got.Dropped != 2 {
		t.Errorf("dropped = %d, want 2", got.Dropped)
	}
	if got.Verbs["show"] != 4 {
		t.Errorf("verbs[show] = %d, want 4", got.Verbs["show"])
	}
}

// TestCallLogRotate is acceptance box 2's second half: rotation keeps the newest
// file under the size limit and keeps only MaxFiles files.
func TestCallLogRotate(t *testing.T) {
	t.Parallel()
	const (
		maxBytes = 400
		calls    = 40
	)
	opts := testOptions(t)
	opts.MaxBytes = maxBytes
	opts.MaxFiles = 3
	// Room for every line, so this test measures rotation and not a drop.
	opts.Buffer = calls
	l, err := NewCallLog(opts)
	if err != nil {
		t.Fatalf("NewCallLog: %v", err)
	}
	for i := 0; i < calls; i++ {
		l.Log(CallRecord{
			Class:  "daemon",
			Caller: fmt.Sprintf("caller-%02d", i),
			Argv:   []string{"show", "gt-itjuwu"},
		})
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	entries, err := os.ReadDir(opts.Dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != opts.MaxFiles {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("kept %d files (%v), want %d", len(entries), names, opts.MaxFiles)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		if info.Size() > maxBytes {
			t.Errorf("%s is %d bytes, over the %d limit", e.Name(), info.Size(), maxBytes)
		}
	}

	lines := readLines(t, filepath.Join(opts.Dir, DefaultCallLogName))
	last := decodeLine(t, lines[len(lines)-1])
	if last["caller"] != "caller-39" {
		t.Errorf("newest file's last line caller = %v, want caller-39", last["caller"])
	}
}

// TestCallLogStats is acceptance box 3: calls, errors, dropped and per-verb
// counts come out of Stats.
func TestCallLogStats(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	l, err := NewCallLog(opts)
	if err != nil {
		t.Fatalf("NewCallLog: %v", err)
	}
	l.Log(CallRecord{Class: "daemon", Caller: "a", Argv: []string{"show", "gt-itjuwu"}})
	l.Log(CallRecord{Class: "daemon", Caller: "b", Argv: []string{"list", "--json"}, ExitCode: 1})
	l.Log(CallRecord{Class: "read", Caller: "c", Argv: []string{"show", "gt-other"}})

	got := l.Stats()
	want := Stats{Calls: 3, Errors: 1, Dropped: 0, Verbs: map[string]uint64{"show": 2, "list": 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Stats = %+v, want %+v", got, want)
	}
	// The snapshot is a copy: a caller cannot mutate the log's counters.
	got.Verbs["show"] = 99
	if next := l.Stats(); next.Verbs["show"] != 2 {
		t.Errorf("Stats shares its verb map: show = %d after mutation", next.Verbs["show"])
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestCallLogUnparsedCounted checks an argv the scanner refuses is counted, so
// a call the log cannot read is visible in Stats rather than silently absent.
func TestCallLogUnparsedCounted(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	l, err := NewCallLog(opts)
	if err != nil {
		t.Fatalf("NewCallLog: %v", err)
	}
	l.Log(CallRecord{Class: "daemon", Caller: "c", Argv: []string{"--if-status", "open", "show", "gt-a"}})
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := l.Stats().Verbs[unparsedVerb]; got != 1 {
		t.Errorf("verbs[%s] = %d, want 1", unparsedVerb, got)
	}
}

// TestCallLogAfterClose checks a line logged after Close is dropped and counted
// rather than blocking on or writing to the closed queue.
func TestCallLogAfterClose(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	l, err := NewCallLog(opts)
	if err != nil {
		t.Fatalf("NewCallLog: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	done := make(chan struct{})
	go func() {
		l.Log(CallRecord{Class: "daemon", Caller: "c", Argv: []string{"show", "gt-a"}})
		close(done)
	}()
	select {
	case <-done:
	case <-waitCtx(t).Done():
		t.Fatal("Log after Close blocked")
	}
	if got := l.Stats(); got.Calls != 1 || got.Dropped != 1 {
		t.Errorf("Stats = calls %d dropped %d, want 1 and 1", got.Calls, got.Dropped)
	}
}

// TestCallLogBoundedFields checks the class and caller, which the caller
// writes, are bounded and stripped of control characters before they reach a
// line.
func TestCallLogBoundedFields(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	l, err := NewCallLog(opts)
	if err != nil {
		t.Fatalf("NewCallLog: %v", err)
	}
	l.Log(CallRecord{Class: "daemon\x1b", Caller: "ab\x00\x07cd\nef", Argv: []string{"show", "gt-a"}})
	long := strings.Repeat("a", 100)
	l.Log(CallRecord{Class: "daemon", Caller: long, Argv: []string{"show", "gt-a"}})
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := readLines(t, filepath.Join(opts.Dir, DefaultCallLogName))
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	first := decodeLine(t, lines[0])
	if first["caller"] != "abcdef" || first["class"] != "daemon" {
		t.Errorf("class, caller = %v, %v; want daemon, abcdef", first["class"], first["caller"])
	}
	if got := decodeLine(t, lines[1])["caller"]; got != long[:maxFieldBytes] {
		t.Errorf("long caller = %v (len %d), want %d bytes", got, len(fmt.Sprint(got)), maxFieldBytes)
	}
}

// TestCallLogWriteError checks a failed write is reported by Close, not read as
// a clean log.
func TestCallLogWriteError(t *testing.T) {
	t.Parallel()
	diskFull := errors.New("disk full")
	hook := func([]byte) error { return diskFull }
	opts := testOptions(t)
	l, err := newCallLog(opts, hook)
	if err != nil {
		t.Fatalf("newCallLog: %v", err)
	}
	l.Log(CallRecord{Class: "daemon", Caller: "c", Argv: []string{"show", "gt-itjuwu"}})
	if err := l.Close(); !errors.Is(err, diskFull) {
		t.Fatalf("Close = %v, want %v", err, diskFull)
	}
	// Closing again reports the same failure rather than a clean second close.
	if err := l.Close(); !errors.Is(err, diskFull) {
		t.Fatalf("second Close = %v, want %v", err, diskFull)
	}
	// The line the writer could not store is counted, so a failing log never
	// reads as clean in Stats.
	if got := l.Stats(); got.Calls != 1 || got.Dropped != 1 {
		t.Errorf("Stats = calls %d dropped %d, want 1 and 1", got.Calls, got.Dropped)
	}
}

// TestCallLogVerbBounded checks argv cannot grow the per-verb map without
// bound: every verb past maxVerbKeys is counted under otherVerb.
func TestCallLogVerbBounded(t *testing.T) {
	t.Parallel()
	const calls = 1000
	opts := testOptions(t)
	opts.Buffer = calls
	l, err := newCallLog(opts, func([]byte) error { return nil })
	if err != nil {
		t.Fatalf("newCallLog: %v", err)
	}
	for i := 0; i < calls; i++ {
		l.Log(CallRecord{Class: "daemon", Caller: "c", Argv: []string{fmt.Sprintf("v%d", i)}})
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := l.Stats()
	if got.Calls != calls {
		t.Errorf("calls = %d, want %d", got.Calls, calls)
	}
	if len(got.Verbs) != maxVerbKeys+1 {
		t.Errorf("kept %d verb keys, want %d", len(got.Verbs), maxVerbKeys+1)
	}
	if want := uint64(calls - maxVerbKeys); got.Verbs[otherVerb] != want {
		t.Errorf("verbs[%s] = %d, want %d", otherVerb, got.Verbs[otherVerb], want)
	}
}

// TestCallLogVerbValidation checks an operand that is not a bd command word is
// neither written as the verb nor counted, so argv cannot key the stats map
// with arbitrary text.
func TestCallLogVerbValidation(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	l, err := NewCallLog(opts)
	if err != nil {
		t.Fatalf("NewCallLog: %v", err)
	}
	l.Log(CallRecord{Class: "daemon", Caller: "c", Argv: []string{"GT-Itjuwu"}})
	l.Log(CallRecord{Class: "daemon", Caller: "c", Argv: []string{strings.Repeat("a", maxVerbLen+1)}})
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, line := range readLines(t, filepath.Join(opts.Dir, DefaultCallLogName)) {
		if verb := decodeLine(t, line)["verb"]; verb != "" {
			t.Errorf("verb = %v, want empty", verb)
		}
	}
	got := l.Stats()
	if got.Calls != 2 {
		t.Errorf("calls = %d, want 2", got.Calls)
	}
	if len(got.Verbs) != 0 {
		t.Errorf("verbs = %v, want none", got.Verbs)
	}
}

// TestCallLogFilesystemRefused drives the two ways the live file cannot be
// opened: a directory path that is a file, and a name already taken by a
// directory.
func TestCallLogFilesystemRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	notDir := filepath.Join(dir, "logs")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if l, err := NewCallLog(CallLogOptions{Dir: notDir, Policy: callLogPolicy(t), BeadPrefixes: []string{"gt"}}); err == nil {
		l.Close()
		t.Fatal("NewCallLog over a file succeeded, want an error")
	}

	logDir := filepath.Join(dir, "calllog")
	if err := os.MkdirAll(filepath.Join(logDir, DefaultCallLogName), 0o755); err != nil {
		t.Fatalf("make directory: %v", err)
	}
	if l, err := NewCallLog(CallLogOptions{Dir: logDir, Policy: callLogPolicy(t), BeadPrefixes: []string{"gt"}}); err == nil {
		l.Close()
		t.Fatal("NewCallLog over a directory named call.log succeeded, want an error")
	}
}

// TestLogWriterKeepOneFile covers the one-file setting: rotation removes the
// live file rather than renaming it to a backup.
func TestLogWriterKeepOneFile(t *testing.T) {
	t.Parallel()
	w := newLogWriter("call.log", 10, 1)
	w.file = &fakeFile{size: 9}
	w.size = 9
	removed := ""
	w.remove = func(name string) error { removed = name; return nil }
	w.openFile = func(string) (logFile, error) { return &fakeFile{}, nil }
	if err := w.write([]byte("0123456789\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if removed != "call.log" {
		t.Errorf("removed %q, want the live file", removed)
	}
}

// fakeFile is a logFile whose calls fail when a test says so.
type fakeFile struct {
	size     int64
	writeErr error
	statErr  error
	closeErr error
}

func (f *fakeFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.size += int64(len(p))
	return len(p), nil
}

func (f *fakeFile) Stat() (os.FileInfo, error) {
	if f.statErr != nil {
		return nil, f.statErr
	}
	return fakeInfo{size: f.size}, nil
}

func (f *fakeFile) Close() error { return f.closeErr }

type fakeInfo struct{ size int64 }

func (f fakeInfo) Name() string       { return "fake" }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) Mode() os.FileMode  { return 0 }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return nil }

// TestLogWriterFailurePaths drives every write, rotate, rename, remove and
// close failure the writer can meet, so none is read as a clean log.
func TestLogWriterFailurePaths(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	line := []byte("{\"x\":1}\n")
	tests := []struct {
		name string
		run  func(t *testing.T) error
	}{
		{
			name: "open",
			run: func(t *testing.T) error {
				w := newLogWriter("call.log", 100, 3)
				w.openFile = func(string) (logFile, error) { return nil, boom }
				return w.write(line)
			},
		},
		{
			name: "stat",
			run: func(t *testing.T) error {
				w := newLogWriter("call.log", 100, 3)
				w.openFile = func(string) (logFile, error) { return &fakeFile{statErr: boom}, nil }
				return w.open()
			},
		},
		{
			name: "stat and close",
			run: func(t *testing.T) error {
				w := newLogWriter("call.log", 100, 3)
				closeBoom := errors.New("close boom")
				w.openFile = func(string) (logFile, error) {
					return &fakeFile{statErr: boom, closeErr: closeBoom}, nil
				}
				err := w.open()
				if !errors.Is(err, boom) || !errors.Is(err, closeBoom) {
					t.Errorf("open = %v, want both failures", err)
				}
				return err
			},
		},
		{
			name: "write",
			run: func(t *testing.T) error {
				w := newLogWriter("call.log", 100, 3)
				w.file = &fakeFile{writeErr: boom}
				return w.write(line)
			},
		},
		{
			name: "rotate close",
			run: func(t *testing.T) error {
				w := newLogWriter("call.log", 100, 3)
				w.file = &fakeFile{size: 95, closeErr: boom}
				w.size = 95
				return w.write(line)
			},
		},
		{
			name: "reopen after rotate",
			run: func(t *testing.T) error {
				w := newLogWriter("call.log", 100, 3)
				w.file = &fakeFile{size: 95}
				w.size = 95
				w.openFile = func(string) (logFile, error) { return nil, boom }
				w.remove = func(string) error { return nil }
				w.rename = func(string, string) error { return nil }
				return w.write(line)
			},
		},
		{
			name: "rotate remove",
			run: func(t *testing.T) error {
				w := newLogWriter("call.log", 100, 3)
				w.remove = func(string) error { return boom }
				return w.rotate()
			},
		},
		{
			name: "rotate shift rename",
			run: func(t *testing.T) error {
				w := newLogWriter("call.log", 100, 3)
				w.file = &fakeFile{}
				w.remove = func(string) error { return nil }
				w.rename = func(string, string) error { return boom }
				return w.rotate()
			},
		},
		{
			name: "rotate live rename",
			run: func(t *testing.T) error {
				w := newLogWriter("call.log", 100, 3)
				w.file = &fakeFile{}
				w.remove = func(string) error { return nil }
				w.rename = func(from, _ string) error {
					if from == w.path {
						return boom
					}
					return nil
				}
				return w.rotate()
			},
		},
		{
			name: "close",
			run: func(t *testing.T) error {
				w := newLogWriter("call.log", 100, 3)
				w.file = &fakeFile{closeErr: boom}
				return w.close()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.run(t); !errors.Is(err, boom) {
				t.Fatalf("%s = %v, want %v", tt.name, err, boom)
			}
		})
	}
}

func TestCallLogOptionsRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts CallLogOptions
	}{
		{name: "no directory", opts: CallLogOptions{Policy: callLogPolicy(t), BeadPrefixes: []string{"gt"}}},
		{name: "no policy", opts: CallLogOptions{Dir: t.TempDir(), BeadPrefixes: []string{"gt"}}},
		{name: "no bead prefixes", opts: CallLogOptions{Dir: t.TempDir(), Policy: callLogPolicy(t)}},
		{name: "bad bead prefix", opts: CallLogOptions{Dir: t.TempDir(), Policy: callLogPolicy(t), BeadPrefixes: []string{"Gt-"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if l, err := NewCallLog(tt.opts); err == nil {
				l.Close()
				t.Fatal("NewCallLog succeeded, want an error")
			}
		})
	}
}

// TestCallLogArgvParse pins what argv contributes and what it must never
// contribute: flag values (inline or separate) and non-id operands.
func TestCallLogArgvParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		argv  []string
		verb  string
		flags []string
		beads []string
	}{
		{
			name:  "verb and id",
			argv:  []string{"show", "gt-itjuwu"},
			verb:  "show",
			flags: []string{},
			beads: []string{"gt-itjuwu"},
		},
		{
			name:  "inline flag value is dropped",
			argv:  []string{"update", "gt-itjuwu", "--status=open"},
			verb:  "update",
			flags: []string{"--status"},
			beads: []string{"gt-itjuwu"},
		},
		{
			name:  "separate flag value is dropped",
			argv:  []string{"update", "gt-itjuwu", "--reason", "a reason with gt-fake in it"},
			verb:  "update",
			flags: []string{"--reason"},
			beads: []string{"gt-itjuwu"},
		},
		{
			// bd's tree records no arity for a command flag, so the scanner
			// cannot tell --title's value from an operand: an id-shaped token
			// stays, free text does not.
			name:  "an id-shaped value of a command flag is never logged",
			argv:  []string{"create", "--title", "gt-fake", "refactor-sling"},
			verb:  "create",
			flags: []string{"--title"},
			beads: []string{},
		},
		{
			name:  "an id after a command flag is not kept: the flag may take it as its value",
			argv:  []string{"list", "--ready", "gt-abc"},
			verb:  "list",
			flags: []string{"--ready"},
			beads: []string{},
		},
		{
			name:  "clustered short token logs its first name only",
			argv:  []string{"update", "gt-a", "-nfoo"},
			verb:  "update",
			flags: []string{"-n"},
			beads: []string{"gt-a"},
		},
		{
			name:  "argv the scanner cannot place is unparsed",
			argv:  []string{"--if-status", "open", "show", "gt-a"},
			verb:  unparsedVerb,
			flags: []string{},
			beads: []string{},
		},
		{
			name:  "unknown prefix is not an id",
			argv:  []string{"show", "refactor-sling"},
			verb:  "show",
			flags: []string{},
			beads: []string{},
		},
		{
			name:  "dotted id and two ids",
			argv:  []string{"dep", "add", "gt-s3rec.7", "hq-cv-abc1"},
			verb:  "dep",
			flags: []string{},
			beads: []string{"gt-s3rec.7", "hq-cv-abc1"},
		},
		{
			name:  "global flag value skipped before the verb",
			argv:  []string{"--db", "hq", "list", "--json"},
			verb:  "list",
			flags: []string{"--db", "--json"},
			beads: []string{},
		},
		{
			name:  "flag-looking operand after separator is not a flag",
			argv:  []string{"show", "--", "gt-itjuwu"},
			verb:  "show",
			flags: []string{},
			beads: []string{"gt-itjuwu"},
		},
		{
			name:  "boolean root flag keeps the bead id after it",
			argv:  []string{"list", "--json", "gt-abc"},
			verb:  "list",
			flags: []string{"--json"},
			beads: []string{"gt-abc"},
		},
		{
			name:  "attached short root flag value is not logged",
			argv:  []string{"list", "-Cdir"},
			verb:  "list",
			flags: []string{"-C"},
			beads: []string{},
		},
		{
			name:  "attached long root flag value is not logged",
			argv:  []string{"list", "--db=beads"},
			verb:  "list",
			flags: []string{"--db"},
			beads: []string{},
		},
		{
			name:  "dash token that names no flag is not logged",
			argv:  []string{"list", "-5"},
			verb:  "list",
			flags: []string{},
			beads: []string{},
		},
		{
			name:  "operand that is not a command word names no verb",
			argv:  []string{"GT-Itjuwu", "gt-abc"},
			verb:  "",
			flags: []string{},
			beads: []string{"gt-abc"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := testOptions(t)
			l, err := NewCallLog(opts)
			if err != nil {
				t.Fatalf("NewCallLog: %v", err)
			}
			l.Log(CallRecord{Class: "daemon", Caller: "c", Argv: tt.argv})
			if err := l.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			got := decodeLine(t, readLines(t, filepath.Join(opts.Dir, DefaultCallLogName))[0])
			if got["verb"] != tt.verb || !reflect.DeepEqual(got["flags"], asAny(tt.flags)) ||
				!reflect.DeepEqual(got["beads"], asAny(tt.beads)) {
				t.Errorf("argv %q logged verb %v flags %v beads %v, want %q %v %v",
					tt.argv, got["verb"], got["flags"], got["beads"], tt.verb, tt.flags, tt.beads)
			}
		})
	}
}

// asAny is a string slice as encoding/json decodes it.
func asAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// waitCtx bounds one wait in a test: a wait that outlives it fails the test by
// name instead of hanging until the package timeout. The deadline is not a
// sleep; every wait it bounds returns as soon as its event happens.
func waitCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestCallLogBeadsOnlyBeforeFlags: a bead-shaped token after a command flag may
// be that flag's value (--parent gt-b), and a value is never logged, so only
// operands that precede every flag are read as bead ids.
func TestCallLogBeadsOnlyBeforeFlags(t *testing.T) {
	t.Parallel()
	l, err := newCallLog(testOptions(t), func([]byte) error { return nil })
	if err != nil {
		t.Fatalf("newCallLog: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	for _, tc := range []struct {
		name string
		argv []string
		want []string
	}{
		{"id before a command flag with an id-shaped value", []string{"update", "gt-aaaa", "--parent", "gt-bbbb"}, []string{"gt-aaaa"}},
		{"id after a command flag is not read", []string{"update", "--parent", "gt-bbbb", "gt-aaaa"}, []string{}},
		{"several ids before any flag", []string{"close", "gt-aaaa", "gt-cccc"}, []string{"gt-aaaa", "gt-cccc"}},
		{"no flags at all", []string{"show", "gt-aaaa"}, []string{"gt-aaaa"}},
	} {
		got := l.parseArgv(tc.argv).beads
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: beads = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCallLogLoggedCountsAreBounded: an argv with hundreds of flags or ids logs
// at most maxLoggedItems of each.
func TestCallLogLoggedCountsAreBounded(t *testing.T) {
	t.Parallel()
	l, err := newCallLog(testOptions(t), func([]byte) error { return nil })
	if err != nil {
		t.Fatalf("newCallLog: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	argv := []string{"close"}
	for i := 0; i < 3*maxLoggedItems; i++ {
		argv = append(argv, "gt-aaaa")
	}
	if got := len(l.parseArgv(argv).beads); got != maxLoggedItems {
		t.Errorf("beads logged = %d, want %d", got, maxLoggedItems)
	}
}
