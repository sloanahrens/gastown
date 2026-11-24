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

// rawReplyError reviews with om exiting 2, reading its raw replies from rawDir;
// during stands in for om saving the files while the review runs.
func rawReplyError(t *testing.T, rawDir string, during func()) string {
	t.Helper()
	r := OMReviewer{OutDir: t.TempDir(), RawDir: func() string { return rawDir }}
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

// writeRaw writes a raw reply as om would; a nonzero mod stamps that mtime.
func writeRaw(t *testing.T, dir, name, body string, mod time.Time) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if !mod.IsZero() {
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
}

const rawReplyStub = "om review execution error: exited 2: om: reviewing" // before the appended reply

// TestOMRawReplyAppendedToExecutionError: the error carries what om said (gt-v8ontb).
func TestOMRawReplyAppendedToExecutionError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	got := rawReplyError(t, dir, func() {
		writeRaw(t, dir, "raw-stdout.txt", "I cannot review this diff", time.Time{})
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
				writeRaw(t, dir, "raw-stdout.txt", big("a", "X", "STDOUT-TAIL"), time.Time{})
				writeRaw(t, dir, "raw-reask.txt", big("c", "Y", "REASK-TAIL"), time.Time{})
			},
			want: ": raw reply: " + strings.Repeat("a", rawReplyHeadMax-2) + "X" +
				": raw re-ask reply: " + strings.Repeat("c", rawReplyHeadMax-2) + "Y",
		},
		{
			name: "control characters collapse to single spaces",
			during: func(t *testing.T, dir string) {
				writeRaw(t, dir, "raw-stdout.txt", "I\n\ncannot\treview\x00this\rdiff", time.Time{})
			},
			want: ": raw reply: I cannot review this diff",
		},
		{
			name: "an unreadable first reply adds nothing",
			during: func(t *testing.T, dir string) {
				if err := os.Mkdir(filepath.Join(dir, "raw-stdout.txt"), 0o700); err != nil {
					t.Fatal(err)
				}
				writeRaw(t, dir, "raw-reask.txt", "second reply", time.Time{})
			},
			want: ": raw re-ask reply: second reply",
		},
		{
			name: "a symlink is not followed",
			during: func(t *testing.T, dir string) {
				writeRaw(t, dir, "target.txt", "not-your-reply", time.Time{})
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
				writeRaw(t, dir, "raw-stdout.txt", "the reply this run saved", time.Time{})
			},
			want: ": raw reply: the reply this run saved",
		},
		{
			name: "a token-shaped run is scrubbed",
			during: func(t *testing.T, dir string) {
				writeRaw(t, dir, "raw-stdout.txt", "auth with sk-fake0000000000000000000000000000 failed", time.Time{})
			},
			want: ": raw reply: auth with <redacted> failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for name, body := range tc.before {
				writeRaw(t, dir, name, body, time.Now().Add(-time.Hour))
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
