package bdgateway

import (
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

func testOptions(t *testing.T) CallLogOptions {
	t.Helper()
	return CallLogOptions{
		Dir:          t.TempDir(),
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
	<-taken    // the writer holds it, so the queue is empty and the buffer free
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

// TestCallLogWriteError checks a failed write is reported by Close, not read as
// a clean log.
func TestCallLogWriteError(t *testing.T) {
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
}

func TestCallLogOptionsRefused(t *testing.T) {
	tests := []struct {
		name string
		opts CallLogOptions
	}{
		{name: "no directory", opts: CallLogOptions{BeadPrefixes: []string{"gt"}}},
		{name: "no bead prefixes", opts: CallLogOptions{Dir: t.TempDir()}},
		{name: "bad bead prefix", opts: CallLogOptions{Dir: t.TempDir(), BeadPrefixes: []string{"Gt-"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
			name:  "title that looks like an id after a flag stays out",
			argv:  []string{"create", "--title", "gt-fake", "refactor-sling"},
			verb:  "create",
			flags: []string{"--title"},
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
