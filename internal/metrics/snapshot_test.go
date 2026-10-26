package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSnapshotAppendsExactlyOneParseableLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	env := envAt(fixedNow())
	env.Town = town
	rec, path, err := RunSnapshot(context.Background(), env, []Source{healthy("queue")}, noDeadline)
	if err != nil {
		t.Fatalf("RunSnapshot: %v", err)
	}
	if want := filepath.Join(town, ".runtime", "metrics", "snapshots.jsonl"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("log has %d lines, want 1:\n%s", len(lines), data)
	}
	var got Record
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("line does not parse: %v\n%s", err, lines[0])
	}
	if got.TS != rec.TS || got.Groups["queue"]["open"] != float64(3) {
		t.Fatalf("appended record = %+v, want the collected one %+v", got, rec)
	}
}

func TestSnapshotKillsHungRunnerAtDeadline(t *testing.T) {
	t.Parallel()
	// The fake waits on its context, so it returns only when RunSnapshot's
	// deadline for the hung source kills it; there is no sleep and the test
	// ends when it does.
	killed := make(chan struct{})
	env := envAt(fixedNow())
	env.Town = t.TempDir()
	env.Run = func(ctx context.Context, _ []string) ([]byte, error) {
		<-ctx.Done()
		close(killed)
		return nil, ctx.Err()
	}
	// The hung source is alone here: one that answered instantly would share
	// the same 1ms deadline and, on a loaded host, miss it too, adding a
	// second errors entry for a reason this test is not about (gt-m5mxp).
	// That other groups still fill beside a failed one is Collect's contract,
	// covered by its own tests under a budget no loaded host misses.
	hung := sourceFunc{group: "host", fn: func(ctx context.Context, env Env) (map[string]any, error) {
		_, err := env.Run(ctx, []string{"vm_stat"})
		return nil, err
	}}
	rec, _, err := RunSnapshot(context.Background(), env, []Source{hung}, time.Millisecond)
	if err != nil {
		t.Fatalf("RunSnapshot: %v", err)
	}
	guard, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case <-killed:
	case <-guard.Done():
		t.Fatal("the hung runner was not killed at its deadline")
	}
	if len(rec.Errors) != 1 || rec.Errors[0].Group != "host" {
		t.Fatalf("errors = %+v, want one entry for host", rec.Errors)
	}
	if rec.Groups["host"] != nil {
		t.Fatalf("Groups[host] = %v, want null", rec.Groups["host"])
	}
}

func TestSnapshotAppendFailureIsAnError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A file where the log's directory belongs makes the append fail.
	if err := os.WriteFile(filepath.Join(town, ".runtime"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := envAt(fixedNow())
	env.Town = town
	if _, _, err := RunSnapshot(context.Background(), env, nil, noDeadline); err == nil {
		t.Fatal("RunSnapshot returned no error when the log cannot be written")
	}
}

func TestTailFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		max  int64
		want string
	}{{4, "6789"}, {10, "0123456789"}, {100, "0123456789"}} {
		got, err := tailFile(path, tc.max)
		if err != nil || !bytes.Equal(got, []byte(tc.want)) {
			t.Errorf("tailFile(max %d) = %q, %v, want %q", tc.max, got, err, tc.want)
		}
	}
}
