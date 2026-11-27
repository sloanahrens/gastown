package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestDoltLogStaleNotice pins the one line the log commands print when the
// host dolt.log has not been written since the Dolt server moved into its
// container (gt-3ktm5u): a file older than doltLogStaleAfter names its age and
// the command that reads the container's own log; a fresh file names nothing.
func TestDoltLogStaleNotice(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		modTime   time.Time
		container string
		want      []string // substrings the notice must hold; empty wants no notice
		refuse    string   // substring the notice must not hold
	}{
		{
			name:      "fifteen days stale names the age and the container log command",
			modTime:   now.Add(-15 * 24 * time.Hour),
			container: "gt-dolt",
			want:      []string{"15 days ago", "docker logs gt-dolt | grep 'serialization failure'"},
		},
		{
			name:      "three hours stale names hours",
			modTime:   now.Add(-3 * time.Hour),
			container: "gt-dolt",
			want:      []string{"3 hours ago", "docker logs gt-dolt"},
		},
		{
			name:      "five minutes fresh prints no notice",
			modTime:   now.Add(-5 * time.Minute),
			container: "gt-dolt",
		},
		{
			name:      "a future timestamp prints no notice",
			modTime:   now.Add(time.Minute),
			container: "gt-dolt",
		},
		{
			name:      "no container configured names the setting, not a broken command",
			modTime:   now.Add(-15 * 24 * time.Hour),
			container: "",
			want:      []string{"15 days ago", "dolt.external_container"},
			refuse:    "docker logs ",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := doltLogStaleNotice(tc.modTime, now, tc.container)
			if len(tc.want) == 0 {
				if got != "" {
					t.Fatalf("notice = %q, want none", got)
				}
				return
			}
			if strings.Contains(got, "\n") {
				t.Errorf("notice is not one line: %q", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("notice %q does not name %q", got, want)
				}
			}
			if tc.refuse != "" && strings.Contains(got, tc.refuse) {
				t.Errorf("notice %q names %q", got, tc.refuse)
			}
		})
	}
}

// writeDoltLogStaleFixture writes the host dolt.log of townRoot with the given
// modification time.
func writeDoltLogStaleFixture(t *testing.T, townRoot string, mtime time.Time) {
	t.Helper()
	logFile := filepath.Join(townRoot, "daemon", "dolt.log")
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logFile, []byte("serving line one\nserving line two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(logFile, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// cannedTail answers the tail the unit tier does not start: it prints the two
// lines the fixture log holds, to out.
func cannedTail(_ []string, out, _ io.Writer) error {
	_, err := io.WriteString(out, "serving line one\nserving line two\n")
	return err
}

// cannedContainerLog is the container reader of a town whose Dolt runs in a
// container: it prints the container's two lines, to out. The text differs
// from cannedTail's so a test tells a container read from a host tail.
func cannedContainerLog(_ string, _ int, _ bool, out, _ io.Writer) error {
	_, err := io.WriteString(out, "container line one\ncontainer line two\n")
	return err
}

// fixedDoltLogReads is the clock, tail and container reader the dolt log
// commands read, with the clock held at now.
func fixedDoltLogReads(now time.Time) doltLogReads {
	return doltLogReads{now: func() time.Time { return now }, tail: cannedTail, containerLog: cannedContainerLog}
}

// TestDoltLogStaleNoticeWrite: the notice writer prints a stale log's notice,
// prints nothing for a fresh log, and returns an error when the log cannot be
// read, never reading it as fresh.
func TestDoltLogStaleNoticeWrite(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		age        time.Duration
		missing    bool
		wantNotice bool
		wantErr    bool
	}{
		{name: "fifteen days stale writes the notice", age: 15 * 24 * time.Hour, wantNotice: true},
		{name: "five minutes fresh writes nothing", age: 5 * time.Minute},
		{name: "missing log is an error, not a clean log", missing: true, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logFile := filepath.Join(t.TempDir(), "dolt.log")
			if !tc.missing {
				if err := os.WriteFile(logFile, []byte("line\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				mtime := now.Add(-tc.age)
				if err := os.Chtimes(logFile, mtime, mtime); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			err := writeDoltLogStaleNotice(&out, logFile, now, "gt-dolt")
			if (err != nil) != tc.wantErr {
				t.Fatalf("writeDoltLogStaleNotice error = %v, want error %t", err, tc.wantErr)
			}
			if (out.Len() > 0) != tc.wantNotice {
				t.Fatalf("notice = %q, want notice %t", out.String(), tc.wantNotice)
			}
		})
	}
}

// TestDoltLogStaleNoticeOnLogsCommand: gt dolt logs prints the stale notice
// before the log's lines, and prints no notice for a fresh log. A container
// town's lines come from the container, never the host dolt.log; an unusable
// dolt.external_container names the setting and keeps the host read.
func TestDoltLogStaleNoticeOnLogsCommand(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		container  string
		age        time.Duration
		wantNotice bool
		want       []string
		lines      string // substring of the lines the command must print
		refuse     string // substring of the other reader's lines it must not print
	}{
		{
			name:       "fifteen day old log prints the container notice before its lines",
			container:  "gt-dolt",
			age:        15 * 24 * time.Hour,
			wantNotice: true,
			want:       []string{"15 days ago", "docker logs gt-dolt"},
			lines:      "container line one",
			refuse:     "serving line one",
		},
		{
			name:       "five minute old log prints no notice",
			container:  "gt-dolt",
			age:        5 * time.Minute,
			wantNotice: false,
			lines:      "container line one",
			refuse:     "serving line one",
		},
		{
			name:       "unusable container setting names the setting",
			container:  "-v",
			age:        15 * 24 * time.Hour,
			wantNotice: true,
			want:       []string{"15 days ago", "dolt.external_container"},
			lines:      "serving line one",
			refuse:     "container line one",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := externalContainerTownRoot(t, tc.container)
			writeDoltLogStaleFixture(t, townRoot, now.Add(-tc.age))

			// One buffer takes both streams, so the order of the notice and the lines is the order written.
			var out bytes.Buffer
			if err := doltLogsTo(townRoot, &out, &out, fixedDoltLogReads(now), 50, false); err != nil {
				t.Fatalf("doltLogsTo: %v", err)
			}
			got := out.String()
			lines := strings.Index(got, tc.lines)
			if lines < 0 {
				t.Fatalf("log lines missing from output %q", got)
			}
			if strings.Contains(got, tc.refuse) {
				t.Fatalf("output %q holds %q, the other reader's lines", got, tc.refuse)
			}
			notice := strings.Index(got, "dolt.log is stale")
			if !tc.wantNotice {
				if notice >= 0 {
					t.Fatalf("output %q has a notice, want none", got)
				}
				return
			}
			if notice < 0 || notice > lines {
				t.Fatalf("notice at %d, log lines at %d, in %q: want the notice before the lines", notice, lines, got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("output %q does not name %q", got, want)
				}
			}
		})
	}
}

// TestDoltLogsContainerModeReadsTheContainer: with dolt.external_container set
// and follow false, gt dolt logs prints the container's last lines through the
// container reader, called with the container name and the line count, and
// never tails the stale host dolt.log (gt-rpoii1).
func TestDoltLogsContainerModeReadsTheContainer(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	townRoot := externalContainerTownRoot(t, "gt-dolt")
	writeDoltLogStaleFixture(t, townRoot, now.Add(-15*24*time.Hour))

	var gotContainer string
	var gotLines int
	var gotFollow bool
	tailed := false
	reads := doltLogReads{
		now:  func() time.Time { return now },
		tail: func([]string, io.Writer, io.Writer) error { tailed = true; return nil },
		containerLog: func(container string, lines int, follow bool, out, _ io.Writer) error {
			gotContainer, gotLines, gotFollow = container, lines, follow
			_, err := io.WriteString(out, "container line one\ncontainer line two\n")
			return err
		},
	}

	var out bytes.Buffer
	if err := doltLogsTo(townRoot, &out, &out, reads, 60, false); err != nil {
		t.Fatalf("doltLogsTo: %v", err)
	}

	if gotContainer != "gt-dolt" || gotLines != 60 {
		t.Errorf("container reader called with (%q, %d), want (\"gt-dolt\", 60)", gotContainer, gotLines)
	}
	if gotFollow {
		t.Error("container reader called with follow set, want false")
	}
	if tailed {
		t.Error("the stale host dolt.log was tailed")
	}
	got := out.String()
	if !strings.Contains(got, "container line one") {
		t.Errorf("output %q does not hold the container's lines", got)
	}
	if strings.Contains(got, "serving line one") {
		t.Errorf("output %q holds the host dolt.log's lines", got)
	}
}

// TestDoltLogsContainerModeUnreadableIsAnError: a container town whose
// container log the reader cannot read, as on a host with no container tool,
// fails the command with an error naming the container and the host command
// that reads it, keeps the reader's cause, and prints the stale notice naming
// that command. No stale host dolt.log line is printed and tail is never run
// (gt-85v8dd).
func TestDoltLogsContainerModeUnreadableIsAnError(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	townRoot := externalContainerTownRoot(t, "gt-dolt")
	writeDoltLogStaleFixture(t, townRoot, now.Add(-15*24*time.Hour))

	noEngine := errors.New("no container engine here")
	tailed := false
	reads := doltLogReads{
		now: func() time.Time { return now },
		tail: func(_ []string, out, _ io.Writer) error {
			tailed = true
			_, err := io.WriteString(out, "serving line one\nserving line two\n")
			return err
		},
		containerLog: func(string, int, bool, io.Writer, io.Writer) error { return noEngine },
	}

	var out, errw bytes.Buffer
	err := doltLogsTo(townRoot, &out, &errw, reads, 60, false)

	if err == nil {
		t.Fatal("doltLogsTo over a container log its reader cannot read returned no error")
	}
	if !errors.Is(err, noEngine) {
		t.Errorf("error %q does not carry the reader's cause %q", err, noEngine)
	}
	for _, want := range []string{"gt-dolt", "docker logs gt-dolt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if tailed {
		t.Error("the stale host dolt.log was tailed after the container read failed")
	}
	if strings.Contains(out.String(), "serving line one") {
		t.Errorf("output %q holds the stale host dolt.log's lines", out.String())
	}
	// The notice is what names the host command on stderr, before the error.
	for _, want := range []string{"dolt.log is stale", "docker logs gt-dolt"} {
		if !strings.Contains(errw.String(), want) {
			t.Errorf("stderr %q does not hold the stale notice naming %q", errw.String(), want)
		}
	}
}

// TestDoltLogsFollowReadsTheContainer: with dolt.external_container set and
// follow true, gt dolt logs follows the container's live log through the
// container reader, called with the container name, the line count and follow
// set, and never tails the stale host dolt.log (gt-chmmo4).
func TestDoltLogsFollowReadsTheContainer(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	townRoot := externalContainerTownRoot(t, "gt-dolt")
	writeDoltLogStaleFixture(t, townRoot, now.Add(-15*24*time.Hour))

	var gotContainer string
	var gotLines int
	var gotFollow bool
	tailed := false
	reads := doltLogReads{
		now:  func() time.Time { return now },
		tail: func([]string, io.Writer, io.Writer) error { tailed = true; return nil },
		containerLog: func(container string, lines int, follow bool, out, _ io.Writer) error {
			gotContainer, gotLines, gotFollow = container, lines, follow
			_, err := io.WriteString(out, "container line one\ncontainer line two\n")
			return err
		},
	}

	var out bytes.Buffer
	if err := doltLogsTo(townRoot, &out, &out, reads, 60, true); err != nil {
		t.Fatalf("doltLogsTo: %v", err)
	}

	if gotContainer != "gt-dolt" || gotLines != 60 {
		t.Errorf("container reader called with (%q, %d), want (\"gt-dolt\", 60)", gotContainer, gotLines)
	}
	if !gotFollow {
		t.Error("container reader called with follow unset, want true")
	}
	if tailed {
		t.Error("the stale host dolt.log was tailed")
	}
	got := out.String()
	if !strings.Contains(got, "container line one") {
		t.Errorf("output %q does not hold the container's lines", got)
	}
	if strings.Contains(got, "serving line one") {
		t.Errorf("output %q holds the host dolt.log's lines", got)
	}
}

// TestDoltLogsHostFollow: a host town with follow set tails the host dolt.log
// with -f, and never reads a container log, since a town with no
// dolt.external_container has no container whose log would hold its Dolt
// output (gt-grnust).
func TestDoltLogsHostFollow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	townRoot := externalContainerTownRoot(t, "")
	writeDoltLogStaleFixture(t, townRoot, now.Add(-5*time.Minute))

	var tailArgs []string
	containerRead := false
	reads := doltLogReads{
		now: func() time.Time { return now },
		tail: func(args []string, out, _ io.Writer) error {
			tailArgs = append([]string(nil), args...)
			_, err := io.WriteString(out, "serving line one\nserving line two\n")
			return err
		},
		containerLog: func(string, int, bool, io.Writer, io.Writer) error {
			containerRead = true
			return nil
		},
	}

	var out bytes.Buffer
	if err := doltLogsTo(townRoot, &out, &out, reads, 50, true); err != nil {
		t.Fatalf("doltLogsTo: %v", err)
	}

	want := []string{"-f", filepath.Join(townRoot, "daemon", "dolt.log")}
	if !slices.Equal(tailArgs, want) {
		t.Errorf("tail args = %q, want %q", tailArgs, want)
	}
	if containerRead {
		t.Error("the container reader was called for a host town's follow")
	}
	if !strings.Contains(out.String(), "serving line one") {
		t.Errorf("output %q does not hold the host dolt.log's lines", out.String())
	}
}

// TestDoltLogStaleNoticeOnDumpCommand: gt dolt dump in a container town prints
// the same stale notice on stderr, names the container instead of reading the
// host's pid, metadata and log tail, and says a log it cannot read has an
// unknown age on stderr instead of reading as fresh (gt-h0v2nw).
func TestDoltLogStaleNoticeOnDumpCommand(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		age        time.Duration
		missing    bool
		wantStderr string
	}{
		{name: "fifteen day old log prints the notice on stderr", age: 15 * 24 * time.Hour, wantStderr: "15 days ago"},
		{name: "five minute old log prints no notice", age: 5 * time.Minute},
		{name: "missing log says its age is unknown", missing: true, wantStderr: "dolt.log age unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := externalContainerTownRoot(t, "gt-dolt")
			if !tc.missing {
				writeDoltLogStaleFixture(t, townRoot, now.Add(-tc.age))
			}
			running := func(string) (bool, int, error) { return true, 4242, nil }

			var out, errw bytes.Buffer
			if err := dumpDoltDiagnostics(townRoot, &out, &errw, running, fixedDoltLogReads(now)); err != nil {
				t.Fatalf("dumpDoltDiagnostics: %v", err)
			}
			if strings.Contains(out.String(), "Recent Dolt log lines:") {
				t.Errorf("stdout %q reads the host log tail in container mode", out.String())
			}
			if !strings.Contains(out.String(), "Container mode (gt-dolt)") {
				t.Errorf("stdout %q does not name the container", out.String())
			}
			if tc.missing {
				if !strings.Contains(errw.String(), tc.wantStderr) {
					t.Errorf("stderr %q does not say %q", errw.String(), tc.wantStderr)
				}
				return
			}
			if tc.wantStderr == "" {
				if errw.Len() > 0 {
					t.Fatalf("stderr %q, want none", errw.String())
				}
			} else if !strings.Contains(errw.String(), tc.wantStderr) {
				t.Errorf("stderr %q does not say %q", errw.String(), tc.wantStderr)
			}
		})
	}
}

// TestDoltDumpHostKeepsTheHostReads: a town whose Dolt is the town's own host
// server keeps the dump's host reads — the live pid, the host log path, the log
// tail and the closing advisory (gt-h0v2nw).
func TestDoltDumpHostKeepsTheHostReads(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	townRoot := plainTownRoot(t, "2026-11-02T09:30:00Z")
	writeDoltLogStaleFixture(t, townRoot, now.Add(-5*time.Minute))
	running := func(string) (bool, int, error) { return true, 4242, nil }

	var out, errw bytes.Buffer
	if err := dumpDoltDiagnostics(townRoot, &out, &errw, running, fixedDoltLogReads(now)); err != nil {
		t.Fatalf("dumpDoltDiagnostics: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"Live PID:   4242",
		"Log file:   " + filepath.Join(townRoot, "daemon", "dolt.log"),
		"Recent Dolt log lines:",
		"serving line one",
		"No signal was sent.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dump %q does not name %q", got, want)
		}
	}
	if errw.Len() != 0 {
		t.Errorf("stderr %q, want none", errw.String())
	}
}

// TestDoltLogStaleNoticeOnLogsCommandUnreadableLogIsAnError: a log whose age cannot
// be read, here because its parent is a file (a stat error that is not "not
// found"), is the command's error and prints nothing: it never reads as fresh and
// never runs the tail.
func TestDoltLogStaleNoticeOnLogsCommandUnreadableLogIsAnError(t *testing.T) {
	t.Parallel()
	townRoot := externalContainerTownRoot(t, "gt-dolt")
	if err := os.WriteFile(filepath.Join(townRoot, "daemon"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	tailed := false
	reads := doltLogReads{
		now:  func() time.Time { return time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC) },
		tail: func([]string, io.Writer, io.Writer) error { tailed = true; return nil },
	}
	var out, errw bytes.Buffer

	err := doltLogsTo(townRoot, &out, &errw, reads, 50, false)

	if err == nil {
		t.Fatal("doltLogsTo over a log it cannot stat returned no error")
	}
	if tailed || out.Len() != 0 || errw.Len() != 0 {
		t.Errorf("tailed=%v out=%q err=%q, want nothing printed and no tail", tailed, out.String(), errw.String())
	}
}

// TestDoltLogContainerArgs: the container reader's docker logs arguments carry
// --tail with the line count and the container name last, and -f only when
// follow is set (gt-rtiyhh).
func TestDoltLogContainerArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		lines  int
		follow bool
		want   []string
	}{
		{name: "no follow reads the last lines and exits", lines: 50, want: []string{"logs", "--tail", "50", "gt-dolt"}},
		{name: "follow adds -f before the container", lines: 50, follow: true, want: []string{"logs", "--tail", "50", "-f", "gt-dolt"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := doltLogContainerArgs("gt-dolt", tc.lines, tc.follow)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("doltLogContainerArgs = %q, want %q", got, tc.want)
			}
		})
	}
}
