package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/resourcesampler"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// at is the fixed reading time; its zone is behind UTC so its local date differs from the day file's.
var at = time.Date(2026, 11, 26, 22, 24, 0, 0, time.FixedZone("behind UTC", -8*60*60))

// statsLine is one docker stats reading the parser accepts.
const statsLine = `{"CPUPerc":"349.74%","MemUsage":"7.989GiB / 8GiB"}`

const podFile = "/Users/sloan/gt/.runtime/metrics/resources/2026-11-27.jsonl"

func readOK(context.Context, string) ([]byte, error) { return []byte(statsLine), nil }
func readFail(context.Context, string) ([]byte, error) {
	return nil, errors.New("docker stats: daemon not reachable")
}
func noFeed([]string, []byte) error { return nil }

// errHome is the injected home for a run with --dir: reading it would mean run
// consulted the home when the flag says it must not.
func errHome() (string, error) { return "", errors.New("home must not be read") }

// TestRunAppendsOneRowForGtDolt: the read's line becomes one row under --dir,
// and a clean run returns 0 (gt-362cts).
func TestRunAppendsOneRowForGtDolt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var got string
	read := func(_ context.Context, container string) ([]byte, error) {
		got = container
		return []byte(statsLine), nil
	}
	if code := run([]string{"--dir", dir}, &bytes.Buffer{}, func() time.Time { return at }, errHome, read, noFeed); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}
	if got != gtDolt {
		t.Errorf("read container = %q, want %q", got, gtDolt)
	}
	data, err := os.ReadFile(filepath.Join(dir, "resources", "2026-11-27.jsonl"))
	if err != nil {
		t.Fatalf("read day file: %v", err)
	}
	var row resourcesampler.Row
	if err := json.Unmarshal(data, &row); err != nil || row.Name != gtDolt {
		t.Errorf("day file = %q: want one row named %q, decode error %v", data, gtDolt, err)
	}
}

// TestRunNamesFailingStep: a failing read, a parser-refused line and an append
// the directory refuses each return 1, print one stderr line naming the step and
// leave no row (gt-n3byrd).
func TestRunNamesFailingStep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prepare func(t *testing.T, dir string)
		read    func(context.Context, string) ([]byte, error)
		step    string
	}{
		{"read", nil, func(context.Context, string) ([]byte, error) {
			return nil, errors.New("docker stats: daemon not reachable")
		}, "read"},
		{"parse", nil, func(context.Context, string) ([]byte, error) { return []byte("CPUPerc=349.74%"), nil }, "parse"},
		{"append", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, "resources"), nil, 0o644); err != nil {
				t.Fatalf("block the resources directory: %v", err)
			}
		}, func(context.Context, string) ([]byte, error) { return []byte(statsLine), nil }, "append"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tt.prepare != nil {
				tt.prepare(t, dir)
			}
			var stderr bytes.Buffer
			if code := run([]string{"--dir", dir}, &stderr, func() time.Time { return at }, errHome, tt.read, noFeed); code != 1 {
				t.Errorf("run = %d, want 1", code)
			}
			want := "gt-dolt-stats: " + tt.step + ": "
			lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
			if len(lines) != 1 || !strings.HasPrefix(lines[0], want) {
				t.Errorf("stderr = %q, want one line starting %q", stderr.String(), want)
			}
			// The append row blocks resources with a file, so ENOTDIR means the
			// day file cannot exist there.
			if _, err := os.Stat(filepath.Join(dir, "resources", "2026-11-27.jsonl")); !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTDIR) {
				t.Errorf("the %s failure wrote a row: stat error = %v", tt.step, err)
			}
		})
	}
}

// TestRunRefusesWithoutRow: a bad flag and a failing read each return 1 and
// append nothing (gt-362cts).
func TestRunRefusesWithoutRow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		flag string
		read func(context.Context, string) ([]byte, error)
	}{
		{"flags", "--nope", func(context.Context, string) ([]byte, error) { return []byte(statsLine), nil }},
		{"read fails", "", func(context.Context, string) ([]byte, error) {
			return nil, errors.New("docker stats: daemon not reachable")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			args := []string{"--dir", dir}
			if tt.flag != "" {
				args = append(args, tt.flag)
			}
			if code := run(args, &bytes.Buffer{}, func() time.Time { return at }, errHome, tt.read, noFeed); code != 1 {
				t.Errorf("run = %d, want 1", code)
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Errorf("dir = %v, %v: want empty, so no row appended", entries, err)
			}
		})
	}
}

// TestRunBoundsTheStatsRead: a read that waits for its context ends at readTimeout, and run returns 1 naming the stats read (gt-r8sgrc).

//testpolicy:allow parallel — it assigns the package's readTimeout, which no parallel test may see
func TestRunBoundsTheStatsRead(t *testing.T) {
	was := readTimeout
	readTimeout = 5 * time.Millisecond //testpolicy:allow no-global-swap — the bead specifies the package variable readTimeout so a test shortens the deadline, and the recorded run has no other seam for it
	t.Cleanup(func() { readTimeout = was })

	dir := t.TempDir()
	var stderr bytes.Buffer
	read := func(ctx context.Context, _ string) ([]byte, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second): //testpolicy:allow no-sleep — a hang guard only: the deadline under test ends the read in milliseconds, so only a missing bound reaches this
			return nil, errors.New("the read did not end at readTimeout")
		}
	}
	if code := run([]string{"--dir", dir}, &stderr, func() time.Time { return at }, errHome, read, noFeed); code != 1 {
		t.Fatalf("run = %d, want 1", code)
	}
	if got, want := strings.TrimSuffix(stderr.String(), "\n"), "gt-dolt-stats: read: docker stats: timed out after 5ms"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("dir = %v, %v: want empty, so no row appended", entries, err)
	}
}

// TestRunDefaults: with no -dir the row goes under the home's
// gt/.runtime/metrics, and -container changes which container is read
// (gt-mmyalw).
func TestRunDefaults(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		args      []string
		container string
	}{
		{"no flags", nil, gtDolt},
		{"container flag", []string{"--container", "other-dolt"}, "other-dolt"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			var got string
			read := func(_ context.Context, container string) ([]byte, error) {
				got = container
				return []byte(statsLine), nil
			}
			if code := run(tt.args, &bytes.Buffer{}, func() time.Time { return at }, func() (string, error) { return home, nil }, read, noFeed); code != 0 {
				t.Fatalf("run = %d, want 0", code)
			}
			if got != tt.container {
				t.Errorf("read container = %q, want %q", got, tt.container)
			}
			if _, err := os.Stat(filepath.Join(home, "gt", ".runtime", "metrics", "resources", "2026-11-27.jsonl")); err != nil {
				t.Errorf("day file under the home metrics dir: %v", err)
			}
		})
	}
}

// TestRunRefusesWithoutHome: an unreadable home with no -dir returns 1, names
// the home step and appends nothing (gt-mmyalw).
func TestRunRefusesWithoutHome(t *testing.T) {
	t.Parallel()
	read := func(context.Context, string) ([]byte, error) { return nil, errors.New("must not be read") }
	var stderr bytes.Buffer
	homeErr := errors.New("$HOME is not defined")
	if code := run(nil, &stderr, func() time.Time { return at }, func() (string, error) { return "", homeErr }, read, noFeed); code != 1 {
		t.Fatalf("run = %d, want 1", code)
	}
	if got, want := strings.TrimSuffix(stderr.String(), "\n"), "gt-dolt-stats: home: $HOME is not defined"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

// TestRunFeedsThePod: the row reaches the pod as one line on stdin; a feed error
// exits 1 naming pod feed and keeps the local row; a failing read feeds nothing (gt-r31xgm).
func TestRunFeedsThePod(t *testing.T) {
	t.Parallel()
	wantArgv := []string{"kubectl", "--context", "colima-gt-control", "-n", "gt", "exec", "-i", "deploy/daemon", "-c", "daemon", "--", "sh", "-c", "cat >> '" + podFile + "'"}
	tests := []struct {
		name    string
		read    func(context.Context, string) ([]byte, error)
		feedErr error
		code    int
		stderr  string
		fed     bool
	}{
		{"the row is fed as one line", readOK, nil, 0, "", true},
		{"a feed error exits 1 keeping the row", readOK, errors.New("kubectl exec: exit status 1"), 1, "gt-dolt-stats: pod feed: kubectl exec: exit status 1\n", true},
		{"a failing read feeds nothing", readFail, nil, 1, "gt-dolt-stats: read: docker stats: daemon not reachable\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			var argv []string
			var stdin []byte
			feed := func(a []string, s []byte) error { argv, stdin = a, s; return tt.feedErr }
			var stderr bytes.Buffer
			if code := run([]string{"--dir", dir}, &stderr, func() time.Time { return at }, errHome, tt.read, feed); code != tt.code || stderr.String() != tt.stderr {
				t.Fatalf("run = %d, stderr = %q: want %d, %q", code, stderr.String(), tt.code, tt.stderr)
			}
			if fed := argv != nil; fed != tt.fed {
				t.Fatalf("feed called = %v, want %v", fed, tt.fed)
			} else if !fed {
				return
			}
			if !reflect.DeepEqual(argv, wantArgv) {
				t.Errorf("feed argv = %q, want %q", argv, wantArgv)
			}
			local, err := os.ReadFile(filepath.Join(dir, "resources", "2026-11-27.jsonl"))
			if err != nil || !bytes.Equal(stdin, local) || !bytes.HasSuffix(stdin, []byte("\n")) {
				t.Errorf("feed stdin = %q, want the local row's one line %q (local read: %v)", stdin, local, err)
			}
		})
	}
}

// TestPodFeedRefuses: an unencodable row and a path podAppendArgv refuses both return an error and leave feed unfed (gt-r31xgm).
func TestPodFeedRefuses(t *testing.T) {
	t.Parallel()
	nan := math.NaN()
	feed := func([]string, []byte) error { t.Error("feed was called, want no feed"); return nil }
	if err := podFeed(feed, resourcesampler.Row{CPUPct: &nan}, podFile); err == nil {
		t.Error("podFeed with an unencodable row = nil, want a refusal")
	} else if err := podFeed(feed, resourcesampler.Row{}, "/Users/sloan/gt/it's.jsonl"); err == nil {
		t.Error("podFeed with an unquotable path = nil, want a refusal")
	}
}

// TestFeedWithin: the runner sees a context bounded to feedTimeout; a run after the parent ended reports that error instead (gt-r31xgm).
func TestFeedWithin(t *testing.T) {
	t.Parallel()
	runErr := errors.New("kubectl: exit status 1")
	if err := feedWithin(context.Background(), func(ctx context.Context) error {
		if d, ok := ctx.Deadline(); !ok || time.Until(d) <= 0 || time.Until(d) > feedTimeout {
			t.Errorf("feed context deadline = %v (set %v), want one within %v", d, ok, feedTimeout)
		}
		return runErr
	}); !errors.Is(err, runErr) {
		t.Errorf("feedWithin = %v, want the runner's error", err)
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if err := feedWithin(ended, func(context.Context) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("feedWithin with an ended context = %v, want context.Canceled", err)
	}
}
