package land

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rawReplyAt is the injected clock every raw-reply row runs on: the attempt starts
// at this whole second, so no row reads the filesystem clock (gt-8kwtyu). It is in
// the past, so a run that drops the clock sees this run's files as stale.
var rawReplyAt = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

// rawReplyNow stamps a file om saves during the run; rawReplyStale one an earlier run left.
var (
	rawReplyNow   = rawReplyAt.Add(time.Second)
	rawReplyStale = rawReplyAt.Add(-time.Hour)
)

// rawReplyError reviews with om exiting 2, reading its raw replies from rawDir;
// during stands in for om saving the files while the review runs.
func rawReplyError(t *testing.T, rawDir string, during func()) string {
	t.Helper()
	r := OMReviewer{
		OutDir: t.TempDir(),
		RawDir: func() string { return rawDir },
		now:    func() time.Time { return rawReplyAt },
	}
	r.run = func(_ context.Context, _ string, _ []string, _ []string, out io.Writer) (int, error) {
		if during != nil {
			during()
		}
		_, _ = io.WriteString(out, "om: reviewing\n")
		return 2, nil
	}
	_, err := reviewAt(context.Background(), r, "", t.TempDir(), "aaa", "bbb")
	if !errors.Is(err, ErrOMExecution) {
		t.Fatalf("error = %v, want it to wrap ErrOMExecution", err)
	}
	return err.Error()
}

// writeRaw writes a raw reply as om would, stamped mod so the run's clock decides it.
func writeRaw(t *testing.T, dir, name, body string, mod time.Time) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

const rawReplyStub = "om review execution error: exited 2: om: reviewing" // before the appended reply

// TestOMRawReplyAppendedToExecutionError: the error carries what om said (gt-v8ontb).
func TestOMRawReplyAppendedToExecutionError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	got := rawReplyError(t, dir, func() {
		writeRaw(t, dir, "raw-stdout.txt", "I cannot review this diff", rawReplyNow)
	})
	if want := rawReplyStub + ": raw reply: I cannot review this diff"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

// TestOMRawReplyBounded drives both attempts and every refusal: the 2048-byte
// cut, controls collapsed, a symlink, a stale or rewritten file, and a token (gt-v8ontb).
func TestOMRawReplyBounded(t *testing.T) {
	t.Parallel()
	// big's first rawReplyHeadMax bytes are max-2 copies of fill, end and a newline; tail is the part the cut drops.
	big := func(fill, end, tail string) string {
		return strings.Repeat(fill, rawReplyHeadMax-2) + end + "\n" + tail + "\n" + strings.Repeat(fill, 100_000)
	}
	for _, tc := range []struct {
		name   string
		before map[string]string              // written before the run, stamped old
		during func(t *testing.T, dir string) // what om saves while it runs
		want   string                         // the error text after rawReplyStub
	}{
		{
			name: "the first 2048 bytes of each reply",
			during: func(t *testing.T, dir string) {
				writeRaw(t, dir, "raw-stdout.txt", big("a", "X", "STDOUT-TAIL"), rawReplyNow)
				writeRaw(t, dir, "raw-reask.txt", big("c", "Y", "REASK-TAIL"), rawReplyNow)
			},
			want: ": raw reply: " + strings.Repeat("a", rawReplyHeadMax-2) + "X" +
				": raw re-ask reply: " + strings.Repeat("c", rawReplyHeadMax-2) + "Y",
		},
		{
			name: "control characters collapse to single spaces",
			during: func(t *testing.T, dir string) {
				writeRaw(t, dir, "raw-stdout.txt", "I\n\ncannot\treview\x00this\rdiff", rawReplyNow)
			},
			want: ": raw reply: I cannot review this diff",
		},
		{
			name: "an unreadable first reply adds nothing",
			during: func(t *testing.T, dir string) {
				if err := os.Mkdir(filepath.Join(dir, "raw-stdout.txt"), 0o700); err != nil {
					t.Fatal(err)
				}
				writeRaw(t, dir, "raw-reask.txt", "second reply", rawReplyNow)
			},
			want: ": raw re-ask reply: second reply",
		},
		{
			name: "a symlink is not followed",
			during: func(t *testing.T, dir string) {
				writeRaw(t, dir, "target.txt", "not-your-reply", rawReplyNow)
				if err := os.Symlink(filepath.Join(dir, "target.txt"), filepath.Join(dir, "raw-stdout.txt")); err != nil {
					t.Fatal(err)
				}
			},
			want: "",
		},
		{
			name:   "a stale reply adds nothing",
			before: map[string]string{"raw-stdout.txt": "an earlier run's reply"},
			want:   "",
		},
		{
			name:   "a stale reply rewritten by this run is attached",
			before: map[string]string{"raw-stdout.txt": "an earlier run's reply"},
			during: func(t *testing.T, dir string) {
				writeRaw(t, dir, "raw-stdout.txt", "the reply this run saved", rawReplyNow)
			},
			want: ": raw reply: the reply this run saved",
		},
		{
			name: "a token-shaped run is scrubbed",
			during: func(t *testing.T, dir string) {
				writeRaw(t, dir, "raw-stdout.txt", "auth with sk-fake0000000000000000000000000000 failed", rawReplyNow)
			},
			want: ": raw reply: auth with <redacted> failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for name, body := range tc.before {
				writeRaw(t, dir, name, body, rawReplyStale)
			}
			got := rawReplyError(t, dir, func() {
				if tc.during != nil {
					tc.during(t, dir)
				}
			})
			if want := rawReplyStub + tc.want; got != want {
				t.Errorf("error (%d bytes) = %q, want %q", len(got), elideMiddle(got, 200), elideMiddle(want, 200))
			}
		})
	}
}

// TestOMRawReplyMissingFile: with no saved reply the execution error is the text
// it had before a raw reply was attached (gt-uhrhop).
func TestOMRawReplyMissingFile(t *testing.T) {
	t.Parallel()
	if got := rawReplyError(t, t.TempDir(), nil); got != rawReplyStub {
		t.Fatalf("error = %q, want unchanged %q", got, rawReplyStub)
	}
}

// TestOMRawReplyBoundary: the injected clock sets the attempt's start, so a reply
// stamped a millisecond before it is another run's and one a millisecond after it
// is this run's, with no wall clock in the boundary (gt-tjuc75). The instant is a
// whole second, the precision the start is truncated to.
func TestOMRawReplyBoundary(t *testing.T) {
	t.Parallel()
	at := rawReplyAt
	for _, tc := range []struct {
		name string
		mod  time.Time
		want string
	}{
		{name: "a reply stamped 1ms before the start is stale", mod: at.Add(-time.Millisecond), want: ""},
		{name: "a reply stamped 1ms after the start is attached", mod: at.Add(time.Millisecond), want: ": raw reply: this run's reply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			r := OMReviewer{
				OutDir: t.TempDir(),
				RawDir: func() string { return dir },
				now:    func() time.Time { return at },
			}
			r.run = func(_ context.Context, _ string, _ []string, _ []string, out io.Writer) (int, error) {
				writeRaw(t, dir, "raw-stdout.txt", "this run's reply", tc.mod)
				_, _ = io.WriteString(out, "om: reviewing\n")
				return 2, nil
			}
			_, err := reviewAt(context.Background(), r, "", t.TempDir(), "aaa", "bbb")
			if !errors.Is(err, ErrOMExecution) {
				t.Fatalf("error = %v, want it to wrap ErrOMExecution", err)
			}
			if want := rawReplyStub + tc.want; err.Error() != want {
				t.Fatalf("error = %q, want %q", err.Error(), want)
			}
		})
	}
}
