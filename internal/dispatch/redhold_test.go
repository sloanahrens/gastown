package dispatch

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestRedHoldFileRoundTrip: what WriteRedHold publishes is what ReadRedHold reads back, field for field.
func TestRedHoldFileRoundTrip(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	since := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	want := RedHoldRecord{Reasons: []RedHoldReason{
		{Kind: RedHoldMainRed, Since: since, Detail: "main is red", CureBeads: []string{"gt-a", "gt-b"}},
		{Kind: RedHoldSweepNoResult, Since: since.Add(time.Hour)},
	}}
	if err := WriteRedHold(townRoot, want); err != nil {
		t.Fatalf("WriteRedHold: %v", err)
	}
	if got := RedHoldPath(townRoot); got != filepath.Join(townRoot, "daemon", "red-hold") {
		t.Fatalf("RedHoldPath = %q, want the daemon directory", got)
	}
	got, err := ReadRedHold(townRoot)
	if err != nil {
		t.Fatalf("ReadRedHold after WriteRedHold: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadRedHold = %+v, want %+v", got, want)
	}
}

// TestRedHoldFileReadRefusals: a missing file is an empty record and nil; a path that cannot be read or parsed is an error.
func TestRedHoldFileReadRefusals(t *testing.T) {
	t.Parallel()
	if rec, err := ReadRedHold(t.TempDir()); err != nil || len(rec.Reasons) != 0 {
		t.Fatalf("ReadRedHold on a missing file = %+v, %v; want empty and nil", rec, err)
	}
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(RedHoldPath(townRoot), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRedHold(townRoot); err == nil {
		t.Fatal("ReadRedHold on invalid JSON = nil, want an error")
	}
	unreadable := t.TempDir()
	if err := os.MkdirAll(RedHoldPath(unreadable), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRedHold(unreadable); err == nil {
		t.Fatal("ReadRedHold on a path that is a directory = nil, want an error")
	}
}

// TestRedHoldFileEmptyRemoves: an empty record removes the file, a missing file
// is not an error, and a removal that fails is an error.
func TestRedHoldFileEmptyRemoves(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := WriteRedHold(townRoot, RedHoldRecord{Reasons: []RedHoldReason{{Kind: RedHoldMainRed}}}); err != nil {
		t.Fatalf("WriteRedHold: %v", err)
	}
	if _, err := os.Stat(RedHoldPath(townRoot)); err != nil {
		t.Fatalf("the written file: %v", err)
	}
	if err := WriteRedHold(townRoot, RedHoldRecord{}); err != nil {
		t.Fatalf("WriteRedHold of an empty record: %v", err)
	}
	if _, err := os.Stat(RedHoldPath(townRoot)); !os.IsNotExist(err) {
		t.Fatalf("the file after an empty record: err = %v, want not exist", err)
	}
	if err := WriteRedHold(townRoot, RedHoldRecord{}); err != nil {
		t.Fatalf("WriteRedHold of an empty record with no file: %v", err)
	}
	blocked := t.TempDir()
	if err := os.MkdirAll(filepath.Join(RedHoldPath(blocked), "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteRedHold(blocked, RedHoldRecord{}); err == nil {
		t.Fatal("WriteRedHold at a path that cannot be removed = nil, want an error")
	}
}

// writeRedHold publishes rec the way the daemon does.
func writeRedHold(t *testing.T, townRoot string, rec RedHoldRecord) {
	t.Helper()
	if err := WriteRedHold(townRoot, rec); err != nil {
		t.Fatalf("WriteRedHold: %v", err)
	}
}

// writeRedHoldRaw writes content to the red hold path, creating the daemon
// directory, so a test can present a document WriteRedHold would not publish.
func writeRedHoldRaw(t *testing.T, townRoot, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(RedHoldPath(townRoot)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(RedHoldPath(townRoot), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDispatchRedHold: a readable record names both kinds, the oldest since
// (RFC3339) and the file; a missing file is open; invalid JSON, no reasons and
// an unreadable path hold fail closed. OperatorHold returns RedHold's reason in
// every case.
func TestDispatchRedHold(t *testing.T) {
	t.Parallel()
	oldest := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	tests := []struct {
		name string
		set  func(t *testing.T, townRoot string)
		want string // "" is open, "unreadable" is the fail-closed wording
	}{
		{"two reasons", func(t *testing.T, townRoot string) {
			writeRedHold(t, townRoot, RedHoldRecord{Reasons: []RedHoldReason{
				{Kind: RedHoldMainRed, Since: oldest.Add(time.Hour)},
				{Kind: RedHoldSweepNoResult, Since: oldest}, // second, so the reader must pick it
			}})
		}, "red hold: main-red, sweep-no-result since 2026-11-20T09:30:00Z"},
		{"absent", func(*testing.T, string) {}, ""},
		{"invalid JSON", func(t *testing.T, townRoot string) { writeRedHoldRaw(t, townRoot, "{not json") }, "unreadable"},
		{"no reasons", func(t *testing.T, townRoot string) { writeRedHoldRaw(t, townRoot, `{"reasons":[]}`) }, "unreadable"},
		{"unreadable path", func(t *testing.T, townRoot string) {
			if err := os.MkdirAll(RedHoldPath(townRoot), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "unreadable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			tt.set(t, townRoot)
			reason := RedHold(townRoot)
			switch tt.want {
			case "":
				if reason != "" {
					t.Fatalf("RedHold = %q, want open", reason)
				}
			case "unreadable":
				if !strings.Contains(reason, "red hold unreadable") || !strings.Contains(reason, "remove the file by hand") {
					t.Fatalf("RedHold = %q, want the fail-closed wording and the by-hand instruction", reason)
				}
			default:
				if !strings.HasPrefix(reason, tt.want) || !strings.Contains(reason, RedHoldPath(townRoot)) {
					t.Fatalf("RedHold = %q, want prefix %q and the path", reason, tt.want)
				}
			}
			if got := operatorHold(townRoot, testKey, time.Now()); got != reason {
				t.Fatalf("operatorHold = %q, want RedHold's %q with no other hold set", got, reason)
			}
		})
	}
}

// TestDispatchRedHoldIsAHoldViewSource: HoldView lists "red hold" after the
// boot hold and before the Ring.
func TestDispatchRedHoldIsAHoldViewSource(t *testing.T) {
	t.Parallel()
	var names []string
	for _, source := range viewOf(t.TempDir(), time.Now()).Sources {
		names = append(names, source.Name)
	}
	want := []string{OperatorHoldSource, "town ESTOP", "boot hold", "red hold", "Ring"}
	if !slices.Equal(names, want) {
		t.Fatalf("HoldView sources = %q, want %q", names, want)
	}
}

// TestDispatchRedHoldEmptyTownRootHolds: no town root holds rather than reading
// daemon/red-hold from the working directory.
func TestDispatchRedHoldEmptyTownRootHolds(t *testing.T) {
	t.Parallel()
	if reason := RedHold(""); !strings.Contains(reason, "no town root") {
		t.Fatalf("RedHold(\"\") = %q, want it to name the missing town root", reason)
	}
}
