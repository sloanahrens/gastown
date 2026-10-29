package specdispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// gt-n05o0: each tick decided why a bead was left unslung and forgot it, so an
// operator could not tell why a bead that looked ready sat untouched.

func TestVerdictLedgerPath(t *testing.T) {
	t.Parallel()
	want := filepath.Join("/town", ".runtime", "spec_dispatch", "verdicts.json")
	if got := LedgerPath("/town"); got != want {
		t.Fatalf("LedgerPath = %q, want %q", got, want)
	}
}

func TestVerdictLedgerRecordStartsAtOneTick(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	reason := strings.Repeat("é", VerdictReasonLimit+1)

	var l Ledger
	l.Record(at, []Entry{
		{Bead: "gt-a", Rig: "gastown", Class: "skipped", Reason: "held: needs human"},
		{Bead: "gt-b", Rig: "gastown", Class: "refused", Reason: reason},
	})

	v, ok := l.Verdicts["gt-a"]
	if !ok {
		t.Fatalf("verdicts = %v, want gt-a recorded", l.Verdicts)
	}
	if v.Bead != "gt-a" || v.Rig != "gastown" || v.Class != "skipped" || v.Reason != "held: needs human" {
		t.Fatalf("verdict = %+v, want gt-a/gastown/skipped/held: needs human", v)
	}
	if !v.FirstAt.Equal(at) || !v.LastAt.Equal(at) {
		t.Fatalf("FirstAt, LastAt = %v, %v, want both %v", v.FirstAt, v.LastAt, at)
	}
	if v.Ticks != 1 {
		t.Fatalf("Ticks = %d, want 1", v.Ticks)
	}
	if !l.SavedAt.Equal(at) {
		t.Fatalf("SavedAt = %v, want %v", l.SavedAt, at)
	}

	if got, want := utf8.RuneCountInString(l.Verdicts["gt-b"].Reason), VerdictReasonLimit; got != want {
		t.Fatalf("reason runes = %d, want the %d-rune limit", got, want)
	}
	if !strings.HasPrefix(reason, l.Verdicts["gt-b"].Reason) {
		t.Fatal("the cut reason is not a prefix of the reason given")
	}
}

func TestVerdictLedgerRecordTwoTicks(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	second := first.Add(5 * time.Minute)

	var l Ledger
	l.Record(first, []Entry{{Bead: "gt-a", Rig: "gastown", Class: "skipped", Reason: "held: needs human"}})
	l.Record(second, []Entry{{Bead: "gt-a", Rig: "gastown", Class: "deferred", Reason: "held: seat busy"}})

	v := l.Verdicts["gt-a"]
	if !v.FirstAt.Equal(first) {
		t.Fatalf("FirstAt = %v, want the first tick %v", v.FirstAt, first)
	}
	if !v.LastAt.Equal(second) {
		t.Fatalf("LastAt = %v, want the second tick %v", v.LastAt, second)
	}
	if v.Reason != "held: seat busy" || v.Class != "deferred" {
		t.Fatalf("Class, Reason = %q, %q, want the second tick's deferred and reason", v.Class, v.Reason)
	}
	if v.Ticks != 2 {
		t.Fatalf("Ticks = %d, want 2", v.Ticks)
	}
	if !l.SavedAt.Equal(second) {
		t.Fatalf("SavedAt = %v, want %v", l.SavedAt, second)
	}
}

func TestVerdictLedgerRecordDropsAbsent(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	second := first.Add(5 * time.Minute)
	third := second.Add(5 * time.Minute)

	var l Ledger
	l.Record(first, []Entry{
		{Bead: "gt-gone", Rig: "gastown", Class: "skipped", Reason: "held: needs human"},
		{Bead: "gt-stays", Rig: "gastown", Class: "skipped", Reason: "blocked by gt-x"},
	})
	l.Record(second, []Entry{{Bead: "gt-stays", Rig: "gastown", Class: "skipped", Reason: "blocked by gt-x"}})

	if _, ok := l.Verdicts["gt-gone"]; ok {
		t.Fatalf("verdicts = %v, want gt-gone dropped by the tick that did not see it", l.Verdicts)
	}
	if got := l.Verdicts["gt-stays"].Ticks; got != 2 {
		t.Fatalf("gt-stays Ticks = %d, want 2", got)
	}

	// A bead that comes back starts a fresh streak: the dropped tick broke it.
	l.Record(third, []Entry{{Bead: "gt-gone", Rig: "gastown", Class: "skipped", Reason: "held: needs human"}})
	back := l.Verdicts["gt-gone"]
	if back.Ticks != 1 || !back.FirstAt.Equal(third) {
		t.Fatalf("gt-gone after returning = Ticks %d, FirstAt %v, want 1 and %v", back.Ticks, back.FirstAt, third)
	}
}

func TestVerdictLedgerLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte(`{"verdicts":`), 0o644); err != nil {
		t.Fatal(err)
	}
	notAFile := filepath.Join(dir, "a-directory")
	if err := os.Mkdir(notAFile, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "a missing file is an empty ledger", path: filepath.Join(dir, "verdicts.json")},
		{name: "a missing directory is an empty ledger", path: filepath.Join(dir, "no-such-dir", "verdicts.json")},
		{name: "a corrupt file is an empty ledger and an error", path: corrupt, wantErr: true},
		{name: "an unreadable path is an empty ledger and an error", path: notAFile, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, err := LoadLedger(tc.path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("LoadLedger(%s) err = %v, want error=%v", tc.path, err, tc.wantErr)
			}
			if l == nil {
				t.Fatal("LoadLedger returned a nil ledger, want an empty one")
			}
			if len(l.Verdicts) != 0 {
				t.Fatalf("verdicts = %v, want an empty ledger", l.Verdicts)
			}
		})
	}
}

func TestVerdictLedgerSave(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

	t.Run("round trips through a directory it creates", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), ".runtime", "spec_dispatch")
		path := filepath.Join(dir, "verdicts.json")
		l := &Ledger{}
		l.Record(at, []Entry{{Bead: "gt-a", Rig: "gastown", Class: "skipped", Reason: "held: needs human"}})
		if err := l.Save(path); err != nil {
			t.Fatalf("Save: %v", err)
		}

		got, err := LoadLedger(path)
		if err != nil {
			t.Fatalf("LoadLedger: %v", err)
		}
		v, ok := got.Verdicts["gt-a"]
		if !ok || v.Class != "skipped" || v.Reason != "held: needs human" || v.Ticks != 1 {
			t.Fatalf("loaded verdicts = %v, want gt-a skipped/held: needs human/1", got.Verdicts)
		}
		if !v.FirstAt.Equal(at) || !v.LastAt.Equal(at) || !got.SavedAt.Equal(at) {
			t.Fatalf("loaded times = %v/%v/%v, want all %v", v.FirstAt, v.LastAt, got.SavedAt, at)
		}
	})

	t.Run("leaves only the ledger file behind", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "runtime")
		path := filepath.Join(dir, "verdicts.json")
		if err := (&Ledger{}).Save(path); err != nil {
			t.Fatalf("Save: %v", err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "verdicts.json" {
			t.Fatalf("directory holds %v, want only verdicts.json", names(entries))
		}
	})

	t.Run("publishes through a rename, so the file it replaces keeps its bytes", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "verdicts.json")
		previous := filepath.Join(dir, "previous.json")
		// The second name for the target's inode stands in for a reader that
		// opened the ledger before the save. A writer that truncates the target
		// in place rewrites that inode, so the reader's copy would change too.
		before := []byte(`{"saved_at":"2026-10-29T12:00:00Z","verdicts":{}}`)
		if err := os.WriteFile(previous, before, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(previous, path); err != nil {
			t.Fatalf("hard link the old ledger into place: %v", err)
		}

		l := &Ledger{}
		l.Record(at, []Entry{{Bead: "gt-a", Rig: "gastown", Class: "skipped", Reason: "held: needs human"}})
		if err := l.Save(path); err != nil {
			t.Fatalf("Save: %v", err)
		}

		if raw, err := os.ReadFile(previous); err != nil || string(raw) != string(before) {
			t.Fatalf("the pre-save reader's bytes = %q, %v, want them untouched", raw, err)
		}
		got, err := LoadLedger(path)
		if err != nil {
			t.Fatalf("LoadLedger: %v", err)
		}
		if v, ok := got.Verdicts["gt-a"]; !ok || v.Class != "skipped" {
			t.Fatalf("loaded verdicts = %v, want the record the save published", got.Verdicts)
		}
	})

	t.Run("a rename the filesystem refuses leaves no temp file and the target as it was", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// A directory in the target's place is a rename the kernel refuses once
		// the temp file is already written, so the failure lands between the two
		// steps a reader must never see split.
		path := filepath.Join(dir, "verdicts.json")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(path, "marker")
		if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}

		if err := (&Ledger{}).Save(path); err == nil {
			t.Fatal("Save renamed over a directory, want an error")
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "verdicts.json" {
			t.Fatalf("directory holds %v, want only the target the rename could not replace", names(entries))
		}
		if raw, err := os.ReadFile(marker); err != nil || string(raw) != "x" {
			t.Fatalf("marker = %q, %v, want the target untouched", raw, err)
		}
	})

	t.Run("writes verdicts as an object, never null", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "verdicts.json")
		if err := (&Ledger{}).Save(path); err != nil {
			t.Fatalf("Save: %v", err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("file is not a JSON object: %v", err)
		}
		if string(doc["verdicts"]) != "{}" {
			t.Fatalf("verdicts = %s, want {}", doc["verdicts"])
		}
	})

	t.Run("a directory it cannot write leaves no partial file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		blocker := filepath.Join(dir, "not-a-dir")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		l := &Ledger{}
		l.Record(at, []Entry{{Bead: "gt-a", Rig: "gastown", Class: "skipped", Reason: "held: needs human"}})

		if err := l.Save(filepath.Join(blocker, "verdicts.json")); err == nil {
			t.Fatal("Save wrote through a regular file, want an error")
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "not-a-dir" {
			t.Fatalf("directory holds %v, want only the blocker file", names(entries))
		}
		if raw, err := os.ReadFile(blocker); err != nil || string(raw) != "x" {
			t.Fatalf("blocker file = %q, %v, want it untouched", raw, err)
		}
	})
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
