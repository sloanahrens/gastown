package events

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

var pruneNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// ev is one events line stamped age before pruneNow.
func ev(age time.Duration, msg string) string {
	return fmt.Sprintf(`{"ts":%q,"type":"mail","actor":%q}`, pruneNow.Add(-age).Format(time.RFC3339), msg)
}

func prunePath(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), EventsFile)
	writeLines(t, path, lines...)
	return path
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func mustPrune(t *testing.T, path string, opts PruneOptions) PruneResult {
	t.Helper()
	res, err := Prune(path, opts, pruneNow)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	return res
}

func TestPrune_DropsLinesOlderThanMaxAge(t *testing.T) {
	t.Parallel()
	old1, old2 := ev(9*24*time.Hour, "a"), ev(8*24*time.Hour, "b")
	kept := []string{ev(6*24*time.Hour, "c"), ev(time.Minute, "d")}
	path := prunePath(t, append([]string{old1, old2}, kept...)...)

	res := mustPrune(t, path, PruneOptions{MaxAge: 7 * 24 * time.Hour})

	if got := readLines(t, path); !reflect.DeepEqual(got, kept) {
		t.Errorf("kept %q, want %q", got, kept)
	}
	if res.LinesDropped != 2 || !res.Pruned() || res.BytesBefore-res.BytesAfter != int64(len(old1)+len(old2)+2) {
		t.Errorf("result %+v", res)
	}
}

func TestPrune_NothingToDropLeavesFileInPlace(t *testing.T) {
	t.Parallel()
	path := prunePath(t, ev(time.Hour, "a"), ev(time.Minute, "b"))
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	res := mustPrune(t, path, PruneOptions{MaxAge: 24 * time.Hour, MaxBytes: 1 << 20})

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pruned() || !os.SameFile(before, after) {
		t.Errorf("rewrote a file with nothing to drop: %+v", res)
	}
}

// Lines without a readable ts are dropped only when an older line follows
// them: a head of unparseable lines is never taken as "old".
func TestPrune_UnparseableLines(t *testing.T) {
	t.Parallel()
	old, fresh := ev(48*time.Hour, "old"), ev(time.Minute, "new")
	cases := []struct {
		name  string
		lines []string
		want  []string
	}{
		{"between old lines", []string{old, "garbage", old, fresh}, []string{fresh}},
		{"before the first in-window line", []string{old, "garbage", fresh}, []string{"garbage", fresh}},
		{"no timestamps at all", []string{"x", `{"type":"mail"}`}, []string{"x", `{"type":"mail"}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := prunePath(t, tc.lines...)
			mustPrune(t, path, PruneOptions{MaxAge: 24 * time.Hour})
			if got := readLines(t, path); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("kept %q, want %q", got, tc.want)
			}
		})
	}
}

// A partial trailing line (a writer died mid-append) is copied byte for byte,
// even when every complete line is old.
func TestPrune_PreservesPartialTrailingLine(t *testing.T) {
	t.Parallel()
	path := prunePath(t, ev(48*time.Hour, "old"))
	partial := `{"ts":"2026-09-30T11:59:00Z","ty`
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(partial); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	mustPrune(t, path, PruneOptions{MaxAge: 24 * time.Hour})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != partial {
		t.Errorf("file = %q, want only the partial line %q", data, partial)
	}
}

// Over the size cap, the newest half of the cap is kept, cut on a line
// boundary, so the next runs have room before rewriting again.
func TestPrune_SizeCapKeepsNewestHalf(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := range 100 {
		lines = append(lines, ev(time.Duration(100-i)*time.Minute, fmt.Sprintf("m%02d", i)))
	}
	path := prunePath(t, lines...)
	lineLen := int64(len(lines[0]) + 1)
	maxBytes := 40 * lineLen

	res := mustPrune(t, path, PruneOptions{MaxAge: 7 * 24 * time.Hour, MaxBytes: maxBytes})

	got := readLines(t, path)
	if !reflect.DeepEqual(got, lines[len(lines)-len(got):]) {
		t.Fatalf("kept lines are not the newest suffix: %q", got)
	}
	if res.BytesAfter > maxBytes/2 || res.BytesAfter < maxBytes/2-lineLen {
		t.Errorf("kept %d bytes, want just under %d", res.BytesAfter, maxBytes/2)
	}
	if res.LinesDropped != 100-len(got) {
		t.Errorf("LinesDropped = %d, want %d", res.LinesDropped, 100-len(got))
	}
}

func TestPrune_MissingFileIsNoOp(t *testing.T) {
	t.Parallel()
	res := mustPrune(t, filepath.Join(t.TempDir(), EventsFile), PruneOptions{MaxAge: time.Hour})
	if res != (PruneResult{}) {
		t.Errorf("result %+v", res)
	}
}

// A writer holding the lock makes Prune give up without touching the file.
func TestPrune_LockBusy(t *testing.T) {
	t.Parallel()
	path := prunePath(t, ev(48*time.Hour, "old"))
	held := flock.New(path + ".lock")
	if err := held.Lock(); err != nil {
		t.Fatal(err)
	}
	defer held.Unlock() //nolint:errcheck

	_, err := Prune(path, PruneOptions{MaxAge: time.Hour, LockTimeout: time.Millisecond}, pruneNow)

	if !errors.Is(err, ErrPruneLockBusy) {
		t.Fatalf("err = %v, want ErrPruneLockBusy", err)
	}
	if got := readLines(t, path); len(got) != 1 {
		t.Errorf("file changed under a held lock: %q", got)
	}
}

func TestPrune_RemovesStaleTemp(t *testing.T) {
	t.Parallel()
	path := prunePath(t, ev(48*time.Hour, "old"), ev(time.Minute, "new"))
	writeLines(t, path+PruneTempSuffix, "crash residue")

	mustPrune(t, path, PruneOptions{MaxAge: 24 * time.Hour})

	if _, err := os.Stat(path + PruneTempSuffix); !os.IsNotExist(err) {
		t.Errorf("temp left behind: %v", err)
	}
	if got := readLines(t, path); len(got) != 1 {
		t.Errorf("kept %q", got)
	}
}

// A tail open across a prune delivers the next append once and does not
// replay the retained history.
func TestPrune_TailFollowsWithoutReplay(t *testing.T) {
	t.Parallel()
	path := prunePath(t, ev(48*time.Hour, "old"), ev(time.Minute, "kept"))
	tail := openTestTail(t, path)

	mustPrune(t, path, PruneOptions{MaxAge: 24 * time.Hour})
	next := ev(0, "next")
	appendLines(t, path, next)

	if got := poll(t, tail); !reflect.DeepEqual(got, []string{next}) {
		t.Errorf("tail after prune = %q, want only %q", got, next)
	}
}
