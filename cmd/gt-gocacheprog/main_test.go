package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/gocacheprog"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// checkRefused pins one refused run: 1, empty stdout, one stderr line holding want (gt-6i1x13).
func checkRefused(t *testing.T, args []string, check func(string) error, want string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(args, strings.NewReader(""), &stdout, &stderr, check, time.Now); code != 1 || stdout.Len() != 0 {
		t.Fatalf("run = %d, stdout %q: want 1 and empty", code, stdout.String())
	}
	if got := stderr.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, want) {
		t.Errorf("stderr = %q, want one line containing %q", got, want)
	}
}

// TestRunRefusesBadArgs: a missing, relative or unusable -dir and an unknown
// flag each exit 1 with one cause line (gt-6i1x13).
func TestRunRefusesBadArgs(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"missing":      {nil, "-dir is required"},
		"relative":     {[]string{"-dir", "cache", "-max-bytes", "4"}, "not absolute"},
		"unusable":     {[]string{"-dir", filepath.Join(file, "store"), "-max-bytes", "4"}, "store"},
		"unknown flag": {[]string{"-bogus"}, "flag provided but not defined"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkRefused(t, tc.args, func(string) error { return nil }, tc.want)
		})
	}
}

// TestRunEmptyStdinIsHandshakeOnly: a usable -dir with no requests exits 0
// with the handshake on stdout, nothing on stderr, and the session's journal
// and stats line in the directory (gt-6i1x13, gt-dpjby4, gt-l4bzgs).
func TestRunEmptyStdinIsHandshakeOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-dir", dir, "-max-bytes", "4"}, strings.NewReader(""), &stdout, &stderr, writableCheck, time.Now); code != 0 || stderr.Len() != 0 {
		t.Fatalf("run = %d, stderr %q: want 0 and empty", code, stderr.String())
	}
	if got := responses(t, stdout.String()); len(got) != 1 || got[0].ID != 0 {
		t.Errorf("stdout = %q: want the handshake alone", stdout.String())
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 2 || entries[0].Name() != "journal" || entries[1].Name() != statsFileName {
		t.Errorf("dir = %v, %v: want journal and %s", entries, err, statsFileName)
	}
}

// TestRunFailingCheck: a writability check that refuses exits 1 with its cause
// on stderr (gt-6i1x13).
func TestRunFailingCheck(t *testing.T) {
	t.Parallel()
	cause := errors.New("disk is read-only")
	checkRefused(t, []string{"-dir", t.TempDir(), "-max-bytes", "4"}, func(string) error { return cause }, cause.Error())
}

// responses decodes the JSON lines out holds, failing on a line that is not a
// response.
func responses(t *testing.T, out string) []gocacheprog.Response {
	t.Helper()
	var got []gocacheprog.Response
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var resp gocacheprog.Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("stdout line %q is not a response: %v", line, err)
		}
		got = append(got, resp)
	}
	return got
}

// TestProgServeClose: a close request gets the handshake and its answer, and
// the command exits 0 with nothing on stderr (gt-dpjby4).
func TestProgServeClose(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	in := strings.NewReader(`{"ID":1,"Command":"close"}` + "\n")
	if code := run([]string{"-dir", t.TempDir(), "-max-bytes", "1024"}, in, &stdout, &stderr, writableCheck, time.Now); code != 0 || stderr.Len() != 0 {
		t.Fatalf("run = %d, stderr %q: want 0 and empty", code, stderr.String())
	}
	want := []gocacheprog.Response{{ID: 0, KnownCommands: []gocacheprog.Cmd{gocacheprog.CmdGet, gocacheprog.CmdPut, gocacheprog.CmdClose}}, {ID: 1}}
	if got := responses(t, stdout.String()); !reflect.DeepEqual(got, want) {
		t.Errorf("responses = %+v, want the handshake then the close answer %+v", got, want)
	}
}

// TestProgServeMalformed: a request line that is not JSON exits 1 with its
// cause on stderr and no answer written (gt-dpjby4).
func TestProgServeMalformed(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run([]string{"-dir", t.TempDir(), "-max-bytes", "1024"}, strings.NewReader("{not json}\n"), &stdout, &stderr, writableCheck, time.Now)
	if got := stderr.String(); code != 1 || strings.Count(got, "\n") != 1 || !strings.Contains(got, "malformed request") {
		t.Fatalf("run = %d, stderr %q: want exit 1 and one line naming a malformed request", code, got)
	}
	if got := responses(t, stdout.String()); len(got) != 1 || got[0].ID != 0 {
		t.Errorf("stdout = %q: want the handshake alone", stdout.String())
	}
}

// TestProbeWritable: a failed create, close or remove refuses the directory by
// name (gt-6i1x13).
func TestProbeWritable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	closed, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		create func(string, string) (*os.File, error)
		remove func(string) error
		want   string
	}{
		"create fails": {func(string, string) (*os.File, error) { return nil, errors.New("create refused") }, os.Remove, "create refused"},
		"close fails":  {func(string, string) (*os.File, error) { return closed, nil }, os.Remove, "file already closed"},
		"remove fails": {os.CreateTemp, func(string) error { return errors.New("remove refused") }, "remove refused"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := probeWritable(dir, tc.create, tc.remove); err == nil || !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("probeWritable = %v, want an error naming %q and %q", err, dir, tc.want)
			}
		})
	}
}

// answerLog collects run's stdout and closes ready once the put's answer is
// written, so the gated get is read only after the put reached the store.
type answerLog struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	lines int
	ready chan struct{}
}

func (l *answerLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.buf.Write(p)
	l.lines += bytes.Count(p, []byte("\n"))
	if l.lines >= 2 {
		select {
		case <-l.ready:
		default:
			close(l.ready)
		}
	}
	return n, err
}

// String returns every response written so far.
func (l *answerLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// gateReader holds every read until ready closes, then reads from inner: Serve
// answers in parallel, so the get that reads a put back waits for the put's
// answer, which proves the put reached the store.
type gateReader struct {
	ready <-chan struct{}
	inner io.Reader
}

func (g *gateReader) Read(p []byte) (int, error) {
	select {
	case <-g.ready:
		return g.inner.Read(p)
	case <-time.After(10 * time.Second): //testpolicy:allow no-sleep — a hang guard: Serve owes the put's answer, so this fires only when it never writes it
		return 0, errors.New("gateReader: the put's answer never arrived")
	}
}

// TestProgSession: one session over a temp dir — a put, the get that hits it, a
// get that misses and a close — asserts the handshake, each answer, the body at
// the put's path, a zero exit and empty stderr (gt-gwqs4l).
func TestProgSession(t *testing.T) {
	t.Parallel()
	log := &answerLog{ready: make(chan struct{})}
	head := `{"ID":1,"Command":"put","ActionID":"YWE=","OutputID":"YmI=","BodySize":5}` + "\n" + `"aGVsbG8="` + "\n"
	tail := `{"ID":2,"Command":"get","ActionID":"YWE="}` + "\n" + `{"ID":3,"Command":"get","ActionID":"Y2M="}` + "\n" + `{"ID":4,"Command":"close"}` + "\n"
	in := io.MultiReader(strings.NewReader(head), &gateReader{ready: log.ready, inner: strings.NewReader(tail)})
	var stderr bytes.Buffer
	if code := run([]string{"-dir", t.TempDir(), "-max-bytes", "1024"}, in, log, &stderr, writableCheck, time.Now); code != 0 {
		t.Fatalf("run = %d, stderr %q: want exit 0", code, stderr.String())
	}
	if got := stderr.String(); got != "" {
		t.Errorf("stderr = %q, want empty", got)
	}
	got := responses(t, log.String())
	if len(got) != 5 {
		t.Fatalf("responses = %+v, want the handshake and one answer per request (5)", got)
	}
	want := gocacheprog.Response{KnownCommands: []gocacheprog.Cmd{gocacheprog.CmdGet, gocacheprog.CmdPut, gocacheprog.CmdClose}}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("handshake = %+v, want %+v", got[0], want)
	}
	// The gets answer in either order, so the answers are looked up by ID.
	byID := make(map[int64]gocacheprog.Response, len(got))
	for _, resp := range got[1:] {
		byID[resp.ID] = resp
	}
	put := byID[1]
	if put.Err != "" {
		t.Fatalf("put answer = %+v, want no error", put)
	}
	body, err := os.ReadFile(put.DiskPath)
	if err != nil {
		t.Fatalf("body at the put's path %q: %v", put.DiskPath, err)
	}
	if string(body) != "hello" {
		t.Errorf("body at the put's path = %q, want %q", body, "hello")
	}
	hit := byID[2]
	if hit.Miss {
		t.Errorf("get of the put's action id = %+v, want a hit", hit)
	}
	if string(hit.OutputID) != "bb" {
		t.Errorf("hit OutputID = %q, want %q", hit.OutputID, "bb")
	}
	if hit.Size != 5 {
		t.Errorf("hit Size = %d, want 5", hit.Size)
	}
	if hit.DiskPath != put.DiskPath {
		t.Errorf("hit DiskPath = %q, want the put's %q", hit.DiskPath, put.DiskPath)
	}
	if hit.Time == nil {
		t.Errorf("hit = %+v, want a Time", hit)
	}
	if miss := byID[3]; !miss.Miss {
		t.Errorf("get of an unwritten action id = %+v, want a miss", miss)
	}
	if last := got[len(got)-1]; last.ID != 4 {
		t.Errorf("last answer = %+v, want the close for ID 4", last)
	}
}

// statsLine is one line of dir/stats.jsonl, decoded; its eviction figures
// promote from evictFigures (gt-p9mvuy).
type statsLine struct {
	Gets      int    `json:"gets"`
	Hits      int    `json:"hits"`
	Misses    int    `json:"misses"`
	Puts      int    `json:"puts"`
	PutBytes  int64  `json:"put_bytes"`
	HitBytes  int64  `json:"hit_bytes"`
	Errors    int    `json:"errors"`
	Start     string `json:"start"`
	ElapsedMS int64  `json:"elapsed_ms"`
	evictFigures
}

// evictFigures holds a line's eight figures as pointers, so absent and zero differ (gt-p9mvuy).
type evictFigures struct {
	Entries        *int64 `json:"entries"`
	TotalBytes     *int64 `json:"total_bytes"`
	YoungBytes     *int64 `json:"young_bytes"`
	CapBytes       *int64 `json:"cap_bytes"`
	OverInUseBytes *int64 `json:"over_in_use_bytes"`
	EvictedEntries *int64 `json:"evicted_entries"`
	EvictedBytes   *int64 `json:"evicted_bytes"`
	EvictMS        *int64 `json:"evict_ms"`
}

// evictionFigures returns s's eight figures in goal order, and false when the
// line holds none of them (gt-p9mvuy).
func evictionFigures(s statsLine) ([8]int64, bool) {
	ps := [8]*int64{s.Entries, s.TotalBytes, s.YoungBytes, s.CapBytes, s.OverInUseBytes, s.EvictedEntries, s.EvictedBytes, s.EvictMS}
	var e [8]int64
	for i, p := range ps {
		if p == nil {
			return e, false
		}
		e[i] = *p
	}
	return e, true
}

// i64 returns a pointer to n, for a statsLine's eviction figures.
func i64(n int64) *int64 { return &n }

// readStats decodes every line of the stats file in dir.
func readStats(t *testing.T, dir string) []statsLine {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, statsFileName))
	if err != nil {
		t.Fatalf("stats file: %v", err)
	}
	var got []statsLine
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var s statsLine
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatalf("stats line %q: %v", line, err)
		}
		got = append(got, s)
	}
	return got
}

// stepReader moves *at to to on its first Read, so a session's start, records
// and end are set by its reads and not by how often the clock is read.
type stepReader struct {
	in      io.Reader
	at      *time.Time
	to      time.Time
	stepped bool
}

func (r *stepReader) Read(p []byte) (int, error) {
	if !r.stepped {
		r.stepped = true
		*r.at = r.to
	}
	return r.in.Read(p)
}

// TestStatsLineSessions: each session appends one line to <dir>/stats.jsonl —
// one over a put, a hit and a miss, an empty one, and one whose Serve fails —
// with exact counts, start and elapsed (gt-l4bzgs).
func TestStatsLineSessions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	at := t0
	clock := func() time.Time { return at }
	log := &answerLog{ready: make(chan struct{})}
	head := `{"ID":1,"Command":"put","ActionID":"YWE=","OutputID":"YmI=","BodySize":5}` + "\n" + `"aGVsbG8="` + "\n"
	tail := `{"ID":2,"Command":"get","ActionID":"YWE="}` + "\n" + `{"ID":3,"Command":"get","ActionID":"Y2M="}` + "\n" + `{"ID":4,"Command":"close"}` + "\n"
	in := &stepReader{in: io.MultiReader(strings.NewReader(head), &gateReader{ready: log.ready, inner: strings.NewReader(tail)}), at: &at, to: t0.Add(2 * time.Second)}
	var stderr bytes.Buffer
	if code := run([]string{"-dir", dir, "-max-bytes", "1024"}, in, log, &stderr, writableCheck, clock); code != 0 || stderr.Len() != 0 {
		t.Fatalf("first session = %d, stderr %q: want 0 and empty", code, stderr.String())
	}
	in = &stepReader{in: strings.NewReader(""), at: &at, to: t0.Add(5 * time.Second)}
	if code := run([]string{"-dir", dir, "-max-bytes", "1024"}, in, io.Discard, &stderr, writableCheck, clock); code != 0 {
		t.Fatalf("second session = %d, want 0", code)
	}
	stderr.Reset()
	in = &stepReader{in: strings.NewReader("{not json}\n"), at: &at, to: t0.Add(6 * time.Second)}
	if code := run([]string{"-dir", dir, "-max-bytes", "1024"}, in, io.Discard, &stderr, writableCheck, clock); code != 1 {
		t.Fatalf("failing session = %d, want 1", code)
	}
	if got := stderr.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "malformed request") {
		t.Errorf("failing session stderr = %q, want one line naming a malformed request", got)
	}
	want := []statsLine{
		{Gets: 2, Hits: 1, Misses: 1, Puts: 1, PutBytes: 5, HitBytes: 5, Start: "2026-01-02T03:04:05Z", ElapsedMS: 2000,
			evictFigures: evictFigures{Entries: i64(1), TotalBytes: i64(5), YoungBytes: i64(5), CapBytes: i64(1024), OverInUseBytes: i64(0), EvictedEntries: i64(0), EvictedBytes: i64(0), EvictMS: i64(0)}},
		{Start: "2026-01-02T03:04:07Z", ElapsedMS: 3000},
		{Start: "2026-01-02T03:04:10Z", ElapsedMS: 1000},
	}
	if got := readStats(t, dir); !reflect.DeepEqual(got, want) {
		t.Errorf("stats lines = %+v, want %+v", got, want)
	}
}

// TestStatsLineUnwritable: with a directory standing at the stats file's path
// the write fails for any user, one stderr line names it, and the session's
// own exit status stands (gt-l4bzgs).
func TestStatsLineUnwritable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, statsFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-dir", dir, "-max-bytes", "1024"}, strings.NewReader(""), &stdout, &stderr, writableCheck, time.Now); code != 0 {
		t.Fatalf("clean session = %d, stderr %q: want the session's 0", code, stderr.String())
	}
	if got := stderr.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, statsFileName) {
		t.Errorf("stderr = %q, want one line naming %s", got, statsFileName)
	}
	if got := responses(t, stdout.String()); len(got) != 1 || got[0].ID != 0 {
		t.Errorf("stdout = %q: want the handshake alone", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"-dir", dir, "-max-bytes", "1024"}, strings.NewReader("{not json}\n"), &stdout, &stderr, writableCheck, time.Now); code != 1 {
		t.Fatalf("failing session = %d, want the session's 1", code)
	}
	if got := stderr.String(); !strings.Contains(got, statsFileName) || !strings.Contains(got, "malformed request") {
		t.Errorf("stderr = %q, want the stats line and the serve line", got)
	}
}

// fakeStatsFile fails appendStats's write or close when those are set.
type fakeStatsFile struct {
	writeErr error
	closeErr error
}

func (f fakeStatsFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}

func (f fakeStatsFile) Close() error { return f.closeErr }

// TestStatsLineRefusals: a refused open, write or close comes back from
// appendStats with its cause (gt-l4bzgs).
func TestStatsLineRefusals(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for name, open := range map[string]func(string) (io.WriteCloser, error){
		"open refused":  func(string) (io.WriteCloser, error) { return nil, boom },
		"write refused": func(string) (io.WriteCloser, error) { return fakeStatsFile{writeErr: boom}, nil },
		"close refused": func(string) (io.WriteCloser, error) { return fakeStatsFile{closeErr: boom}, nil },
	} {
		if err := appendStats(t.TempDir(), gocacheprog.Stats{}, eviction{}, start, time.Now, open); !errors.Is(err, boom) {
			t.Errorf("%s = %v, want %v", name, err, boom)
		}
	}
}

// Eviction fixtures: a put of "hello" under action "aa", and a close numbered 9
// so its answer is found by its ID in the bytes a session writes.
const (
	putHello = `{"ID":1,"Command":"put","ActionID":"YWE=","OutputID":"YmI=","BodySize":5}` + "\n" + `"aGVsbG8="` + "\n"
	closeReq = `{"ID":9,"Command":"close"}` + "\n"
)

// fixedClock returns a clock that always reads t.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// TestProgEvictStatsLine: a session whose close evicts writes the eight figures
// from the summary, the cap and the eviction's own clock span. The clock steps
// by a fixed amount per read, so the two reads around the eviction differ by
// that step and evict_ms is not a constant (gt-p9mvuy).
func TestProgEvictStatsLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var mu sync.Mutex
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now := at
		at = at.Add(250 * time.Millisecond)
		return now
	}
	log := &answerLog{ready: make(chan struct{})}
	if code, stderr := putThenClose(dir, clock, log, log); code != 0 || stderr != "" {
		t.Fatalf("run = %d, stderr %q: want 0 and empty", code, stderr)
	}
	got := readStats(t, dir)
	if len(got) != 1 {
		t.Fatalf("stats lines = %+v, want one", got)
	}
	if e, ok := evictionFigures(got[0]); !ok || e != [8]int64{1, 5, 5, 4, 0, 1, 5, 250} {
		t.Errorf("figures = %v (all present %v), want entries 1, total 5, young 5, cap 4, over 0, evicted 1/5, 250ms", e, ok)
	}
}

// handshakeThen returns the handshake and then one answer carrying id, as a
// session that answers one request writes them.
func handshakeThen(id int64) []gocacheprog.Response {
	return []gocacheprog.Response{{ID: 0, KnownCommands: []gocacheprog.Cmd{gocacheprog.CmdGet, gocacheprog.CmdPut, gocacheprog.CmdClose}}, {ID: id}}
}

// diskPathOf returns the DiskPath of the answer with id in out, or "" when out
// holds no such answer yet.
func diskPathOf(out string, id int64) string {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var resp gocacheprog.Response
		if json.Unmarshal([]byte(line), &resp) == nil && resp.ID == id {
			return resp.DiskPath
		}
	}
	return ""
}

// closeCheck is a session's answerLog that also records, as the close answer
// is written, whether the body the put's answer names is already gone.
type closeCheck struct {
	*answerLog
	seen bool
	gone bool
}

func (c *closeCheck) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"ID":9`)) {
		c.seen = true
		path := diskPathOf(c.String(), 1)
		_, err := os.Stat(path)
		c.gone = path != "" && errors.Is(err, fs.ErrNotExist)
	}
	return c.answerLog.Write(p)
}

// putThenClose runs one session over dir at the clock now that puts the body
// and then closes. The close is read only once the put's answer is written. It
// returns the exit status and stderr.
func putThenClose(dir string, now func() time.Time, log *answerLog, out io.Writer) (int, string) {
	in := io.MultiReader(strings.NewReader(putHello), &gateReader{ready: log.ready, inner: strings.NewReader(closeReq)})
	var stderr bytes.Buffer
	code := run([]string{"-dir", dir, "-max-bytes", "4"}, in, out, &stderr, writableCheck, now)
	return code, stderr.String()
}

// TestProgEvictAtClose: a put over a -max-bytes of 4 is evicted by the close
// request, and the body is gone when the close answer is written (gt-n9cohm).
func TestProgEvictAtClose(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := &answerLog{ready: make(chan struct{})}
	out := &closeCheck{answerLog: log}
	code, stderr := putThenClose(dir, fixedClock(time.Now()), log, out)
	if code != 0 || stderr != "" {
		t.Fatalf("run = %d, stderr %q: want 0 and empty", code, stderr)
	}
	if !out.seen || !out.gone {
		t.Errorf("close answer seen = %v, body gone when it was written = %v: want both", out.seen, out.gone)
	}
	if path := diskPathOf(log.String(), 1); path == "" {
		t.Errorf("put answer = %q: want a disk path", log.String())
	} else if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("body at %q after the session: %v, want it evicted", path, err)
	}
}

// TestProgEvictWithinInterval: after an eviction writes the snapshot, a session
// that closes within ten minutes of it is refused, so the body it put stays
// (gt-n9cohm).
func TestProgEvictWithinInterval(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	at := time.Now()
	first := &answerLog{ready: make(chan struct{})}
	if code, stderr := putThenClose(dir, fixedClock(at), first, first); code != 0 || stderr != "" {
		t.Fatalf("first session = %d, stderr %q: want 0 and empty", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.jsonl")); err != nil {
		t.Fatalf("snapshot after the first session: %v: want it written", err)
	}
	second := &answerLog{ready: make(chan struct{})}
	if code, stderr := putThenClose(dir, fixedClock(at), second, second); code != 0 || stderr != "" {
		t.Fatalf("second session = %d, stderr %q: want 0 and empty", code, stderr)
	}
	path := diskPathOf(second.String(), 1)
	if path == "" {
		t.Fatalf("put answer = %q: want a disk path", second.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("body at %q after the second session: %v, want it kept", path, err)
	}
	if got := readStats(t, dir); len(got) != 2 {
		t.Errorf("stats lines = %+v, want the two sessions'", got)
	} else if e, ok := evictionFigures(got[1]); ok {
		t.Errorf("refused session's figures = %v: want none, since its eviction did not run", e)
	}
}

// journalSwap replaces the journal file with a non-empty directory of its name
// on its first Read. Eviction then writes the snapshot, and its removal of the
// journal fails (gt-n9cohm).
type journalSwap struct {
	dir     string
	inner   io.Reader
	swapped bool
	err     error
}

func (s *journalSwap) Read(p []byte) (int, error) {
	if !s.swapped {
		s.swapped = true
		s.err = swapJournal(s.dir)
	}
	if s.err != nil {
		return 0, s.err
	}
	return s.inner.Read(p)
}

// swapJournal replaces the one journal file under dir/journal with a directory
// of the same name that holds a file.
func swapJournal(dir string) error {
	jdir := filepath.Join(dir, "journal")
	entries, err := os.ReadDir(jdir)
	if err != nil {
		return err
	}
	if len(entries) != 1 {
		return fmt.Errorf("journal dir holds %d entries, want the one journal", len(entries))
	}
	path := filepath.Join(jdir, entries[0].Name())
	if err := os.Remove(path); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(path, "held"), nil, 0o644)
}

// TestProgEvictErrorRanFalse: an eviction that fails before it writes the
// snapshot adds 1 to the stats errors and one stderr line, leaves the snapshot
// as it was, and keeps the responses and the exit status (gt-n9cohm).
func TestProgEvictErrorRanFalse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	at := time.Now()
	snap := filepath.Join(dir, "index.jsonl")
	const garbage = "not a snapshot\n"
	if err := os.WriteFile(snap, []byte(garbage), 0o644); err != nil {
		t.Fatal(err)
	}
	// An hour old, so the gate lets this eviction run and it fails reading the snapshot.
	old := at.Add(-time.Hour)
	if err := os.Chtimes(snap, old, old); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-dir", dir, "-max-bytes", "4"}, strings.NewReader(closeReq), &stdout, &stderr, writableCheck, fixedClock(at)); code != 0 {
		t.Fatalf("run = %d, stderr %q: want the session's 0", code, stderr.String())
	}
	if got := stderr.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "evict:") {
		t.Errorf("stderr = %q, want one line naming the eviction", got)
	}
	if data, err := os.ReadFile(snap); err != nil || string(data) != garbage {
		t.Errorf("snapshot = %q, %v: want it untouched, since the eviction failed before it wrote one", data, err)
	}
	if got := responses(t, stdout.String()); !reflect.DeepEqual(got, handshakeThen(9)) {
		t.Errorf("responses = %+v, want the handshake then the close answer", got)
	}
	if got := readStats(t, dir); len(got) != 1 || got[0].Errors != 1 {
		t.Errorf("stats lines = %+v, want one line with errors 1", got)
	} else if e, ok := evictionFigures(got[0]); ok {
		t.Errorf("figures = %v: want none, since the eviction did not run", e)
	}
}

// TestProgEvictErrorRanTrue: an eviction whose journal removal fails after the
// snapshot is written adds 1 to the stats errors and one stderr line, keeps the
// responses and the exit status, and leaves the snapshot written (gt-n9cohm).
func TestProgEvictErrorRanTrue(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	in := &journalSwap{dir: dir, inner: strings.NewReader(closeReq)}
	if code := run([]string{"-dir", dir, "-max-bytes", "4"}, in, &stdout, &stderr, writableCheck, fixedClock(time.Now())); code != 0 {
		t.Fatalf("run = %d, stderr %q: want the session's 0", code, stderr.String())
	}
	if in.err != nil {
		t.Fatalf("swapping the journal for a directory: %v", in.err)
	}
	if got := stderr.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "remove journal") {
		t.Errorf("stderr = %q, want one line naming the failed journal removal", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.jsonl")); err != nil {
		t.Errorf("snapshot: %v, want it written before the removal failed", err)
	}
	if got := responses(t, stdout.String()); !reflect.DeepEqual(got, handshakeThen(9)) {
		t.Errorf("responses = %+v, want the handshake then the close answer", got)
	}
	if got := readStats(t, dir); len(got) != 1 || got[0].Errors != 1 {
		t.Errorf("stats lines = %+v, want one line with errors 1", got)
	} else if e, ok := evictionFigures(got[0]); !ok || e != [8]int64{0, 0, 0, 4, 0, 0, 0, 0} {
		t.Errorf("figures = %v (all present %v), want entries 0, total 0, young 0, cap 4, over 0, evicted 0/0, 0ms", e, ok)
	}
}

// TestProgEvictRefusesBadInput: a missing, non-numeric, zero or negative
// -max-bytes, or a journal that cannot be opened, exits 1 before the
// handshake (gt-n9cohm).
func TestProgEvictRefusesBadInput(t *testing.T) {
	t.Parallel()
	for name, limit := range map[string]string{"missing": "", "not a number": "lots", "zero": "0", "negative": "-1"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			args := []string{"-dir", t.TempDir()}
			if limit != "" {
				args = append(args, "-max-bytes", limit)
			}
			checkRefused(t, args, func(string) error { return nil }, "max-bytes")
		})
	}
	// A regular file named journal stops OpenJournal from creating its directory.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "journal"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	checkRefused(t, []string{"-dir", dir, "-max-bytes", "4"}, func(string) error { return nil }, "journal")
}
