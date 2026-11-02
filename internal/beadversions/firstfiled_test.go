package beadversions

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/metrics"
)

// filedRow is one archive first-filed row.
func filedRow(id, description, acceptance string, at time.Time) metrics.FirstFiled {
	return metrics.FirstFiled{Row: metrics.ArchiveIssueRow{
		ID:                 id,
		Description:        description,
		AcceptanceCriteria: acceptance,
		CreatedAt:          at,
	}}
}

func TestImportFirstFiledPutsAsArchiveAtCreatedAt(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	first := []metrics.FirstFiled{
		filedRow("gt-a", "## Goal\nfirst", "- [ ] a", day),
		filedRow("gt-b", "## Goal\nsecond", "- [x] b", day.Add(24*time.Hour)),
	}
	store := NewMemoryStore()

	put, skipped, err := ImportFirstFiled(store, first, FiledText)
	if err != nil {
		t.Fatalf("ImportFirstFiled: %v", err)
	}
	if put != 2 || skipped != 0 {
		t.Fatalf("put, skipped = %d, %d, want 2, 0", put, skipped)
	}
	for _, f := range first {
		versions, err := store.Versions(f.Row.ID)
		if err != nil {
			t.Fatalf("Versions(%s): %v", f.Row.ID, err)
		}
		if len(versions) != 1 {
			t.Fatalf("%s has %d version(s), want 1", f.Row.ID, len(versions))
		}
		v := versions[0]
		want := f.Row.Description + "\n\n" + f.Row.AcceptanceCriteria
		if v.Bead != f.Row.ID || v.Text != want {
			t.Errorf("version = %q %q, want %q %q", v.Bead, v.Text, f.Row.ID, want)
		}
		if v.Actor != archiveActor {
			t.Errorf("%s actor = %q, want %q", f.Row.ID, v.Actor, archiveActor)
		}
		if !v.At.Equal(f.Row.CreatedAt) {
			t.Errorf("%s at = %s, want the row's created_at %s", f.Row.ID, v.At, f.Row.CreatedAt)
		}
	}
}

func TestImportFirstFiledIsIdempotent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bead-versions.jsonl")
	first := []metrics.FirstFiled{
		filedRow("gt-a", "## Goal\nfirst", "- [ ] a", time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)),
	}

	put, skipped, err := ImportFirstFiled(NewFileStore(path), first, FiledText)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if put != 1 || skipped != 0 {
		t.Fatalf("first import put, skipped = %d, %d, want 1, 0", put, skipped)
	}

	// A fresh store, as a second run of the command has.
	store := NewFileStore(path)
	put, skipped, err = ImportFirstFiled(store, first, FiledText)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if put != 0 || skipped != 1 {
		t.Fatalf("second import put, skipped = %d, %d, want 0, 1", put, skipped)
	}
	versions, err := store.Versions("gt-a")
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("%d version(s) after two imports, want 1", len(versions))
	}
}

// stubStore is a Store whose reads and writes fail, so the import's error
// branches run without a broken disk.
type stubStore struct {
	versionsErr error
	putErr      error
}

func (s *stubStore) Put(string, string, string, time.Time) (bool, error) {
	return false, s.putErr
}

func (s *stubStore) Versions(string) ([]Version, error) { return nil, s.versionsErr }

func (s *stubStore) AsOf(string, time.Time) (Version, error) {
	return Version{}, errors.New("AsOf is not used by the import")
}

func TestImportFirstFiledRefusesWhatItCannotRecord(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	first := []metrics.FirstFiled{filedRow("gt-a", "## Goal\nfirst", "- [ ] a", day)}

	// newerText holds a later, different text for the bead, so the first-filed
	// row cannot be appended in order.
	newerText := func() Store {
		s := NewMemoryStore()
		if _, err := s.Put("gt-a", "later text", "dispatch", day.Add(time.Hour)); err != nil {
			t.Fatalf("seeding the store: %v", err)
		}
		return s
	}
	cases := []struct {
		name  string
		store Store
		first []metrics.FirstFiled
		text  func(metrics.ArchiveIssueRow) string
		want  string
	}{
		{"no store", nil, first, FiledText, "no store"},
		{"no text function", NewMemoryStore(), first, nil, "no text function"},
		{"empty bead id", NewMemoryStore(), []metrics.FirstFiled{filedRow("", "## Goal", "- [ ] a", day)}, FiledText, "empty bead id"},
		{"the store's versions cannot be read", &stubStore{versionsErr: errors.New("versions refused")}, first, FiledText, "versions refused"},
		{"the store refuses the put", &stubStore{putErr: errors.New("put refused")}, first, FiledText, "put refused"},
		{"a later text is already recorded", newerText(), first, FiledText, "time not after the latest version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			put, skipped, err := ImportFirstFiled(c.store, c.first, c.text)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want it to name %q", err, c.want)
			}
			if put != 0 || skipped != 0 {
				t.Errorf("put, skipped = %d, %d, want 0, 0 on an error", put, skipped)
			}
		})
	}
}
