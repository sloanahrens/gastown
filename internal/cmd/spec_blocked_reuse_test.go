package cmd

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
)

var (
	reusedAt   = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	reusedHeld = map[string]beadsql.RigHash{"gt": {Head: "h1", Working: "w1"}}
	reusedList = []*beads.Issue{{ID: "gt-a", Priority: 1, BlockedBy: []string{"gt-x"}}}
)

// reusedCall reads rig gt at time at under heads through a reader over mem.
func reusedCall(t *testing.T, mem *specAnnotated, heads map[string]beadsql.RigHash, at time.Time, read func(string) ([]*beads.Issue, error)) []*beads.Issue {
	t.Helper()
	got, err := reusedBlockedReader(mem, heads, func() time.Time { return at }, read)("gt")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestReusedBlockedReaderReuses: the same hashes inside five minutes answer the
// first list without a read; a changed hash, five minutes on or nil heads read.
func TestReusedBlockedReaderReuses(t *testing.T) {
	t.Parallel()
	moved := map[string]beadsql.RigHash{"gt": {Head: "h2", Working: "w1"}}
	cases := []struct {
		name  string
		heads map[string]beadsql.RigHash
		after time.Duration
		reads int
	}{
		{"same hashes inside five minutes", reusedHeld, time.Minute, 1},
		{"changed hash", moved, time.Minute, 2},
		{"five minutes later", reusedHeld, 5 * time.Minute, 2},
		{"nil heads", nil, time.Minute, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reads := 0
			read := func(string) ([]*beads.Issue, error) { reads++; return reusedList, nil }
			mem := loadSpecAnnotated("", "")
			reusedCall(t, mem, reusedHeld, reusedAt, read)
			got := reusedCall(t, mem, tc.heads, reusedAt.Add(tc.after), read)
			if !reflect.DeepEqual(got, reusedList) || reads != tc.reads {
				t.Errorf("got %v after %d reads; want %v after %d", got, reads, reusedList, tc.reads)
			}
		})
	}
}

// TestReusedBlockedReaderFailedRead: a failed read returns its error with no
// issues and stores nothing, so the next call under the same hashes reads again.
func TestReusedBlockedReaderFailedRead(t *testing.T) {
	t.Parallel()
	boom := errors.New("board unreadable")
	reads := 0
	read := reusedBlockedReader(loadSpecAnnotated("", ""), reusedHeld, func() time.Time { return reusedAt },
		func(string) ([]*beads.Issue, error) { reads++; return nil, boom })
	for call := 1; call <= 2; call++ {
		if got, err := read("gt"); !errors.Is(err, boom) || got != nil || reads != call {
			t.Fatalf("call %d = %v, %v after %d reads; want nil, %v after %d", call, got, err, reads, boom, call)
		}
	}
}

// TestReusedBlockedReaderHashBeforeRead: a head that moves during the read is
// noted under the hash held before it, so the next call under the old hash reads.
func TestReusedBlockedReaderHashBeforeRead(t *testing.T) {
	t.Parallel()
	heads := map[string]beadsql.RigHash{"gt": {Head: "h1", Working: "w1"}}
	reads := 0
	read := func(string) ([]*beads.Issue, error) {
		reads++
		heads["gt"] = beadsql.RigHash{Head: "h2", Working: "w1"}
		return reusedList, nil
	}
	mem := loadSpecAnnotated("", "")
	reusedCall(t, mem, heads, reusedAt, read)
	reusedCall(t, mem, heads, reusedAt, read)
	if reads != 2 {
		t.Errorf("reads = %d after a head moved during the first read; want 2", reads)
	}
}

// TestReusedBlockedReaderStoresNothing: a nil memory, nil heads or a rig absent
// from heads reads every time, and nothing it read is stored for held hashes.
func TestReusedBlockedReaderStoresNothing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		mem   *specAnnotated
		heads map[string]beadsql.RigHash
	}{
		{"nil memory", nil, reusedHeld},
		{"nil heads", loadSpecAnnotated("", ""), nil},
		{"rig absent", loadSpecAnnotated("", ""), map[string]beadsql.RigHash{"be": reusedHeld["gt"]}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reads := 0
			read := func(string) ([]*beads.Issue, error) { reads++; return reusedList, nil }
			reusedCall(t, tc.mem, tc.heads, reusedAt, read)
			reusedCall(t, tc.mem, tc.heads, reusedAt, read)
			stored := 0
			if tc.mem != nil {
				stored = len(tc.mem.blocked)
			}
			if reads != 2 || stored != 0 {
				t.Errorf("reads = %d, stored %d rigs; want 2 reads and none stored", reads, stored)
			}
		})
	}
}
