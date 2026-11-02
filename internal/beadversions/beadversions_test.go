package beadversions

import (
	"errors"
	"testing"
	"time"
)

var testStart = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// TestPutRecordsOnlyChanges: a text identical to the latest version records
// nothing; a changed text records exactly one new version.
func TestPutRecordsOnlyChanges(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()

	recorded, err := store.Put("gt-1", "first text", "furiosa", testStart)
	if err != nil || !recorded {
		t.Fatalf("first Put = %v, %v; want recorded", recorded, err)
	}

	recorded, err = store.Put("gt-1", "first text", "furiosa", testStart.Add(time.Minute))
	if err != nil {
		t.Fatalf("identical Put error = %v; want nil", err)
	}
	if recorded {
		t.Error("identical Put recorded a version; want recorded=false")
	}

	recorded, err = store.Put("gt-1", "second text", "furiosa", testStart.Add(2*time.Minute))
	if err != nil || !recorded {
		t.Fatalf("changed Put = %v, %v; want recorded", recorded, err)
	}

	versions, err := store.Versions("gt-1")
	if err != nil {
		t.Fatalf("Versions error = %v; want nil", err)
	}
	if len(versions) != 2 {
		t.Fatalf("Versions length = %d; want 2", len(versions))
	}
	if versions[0].Text != "first text" || versions[1].Text != "second text" {
		t.Errorf("Versions texts = %q, %q; want first text, second text",
			versions[0].Text, versions[1].Text)
	}
}

// TestAsOfReturnsTheVersionInForce: AsOf answers with the latest version at or
// before the query time, and ErrNotFound before the first and for a bead with
// no versions.
func TestAsOfReturnsTheVersionInForce(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	first := testStart
	second := testStart.Add(time.Hour)
	if _, err := store.Put("gt-1", "one", "furiosa", first); err != nil {
		t.Fatalf("Put one error = %v; want nil", err)
	}
	if _, err := store.Put("gt-1", "two", "furiosa", second); err != nil {
		t.Fatalf("Put two error = %v; want nil", err)
	}

	tests := []struct {
		name    string
		bead    string
		at      time.Time
		want    string
		wantErr error
	}{
		{name: "before first", bead: "gt-1", at: first.Add(-time.Second), wantErr: ErrNotFound},
		{name: "first boundary", bead: "gt-1", at: first, want: "one"},
		{name: "between", bead: "gt-1", at: first.Add(30 * time.Minute), want: "one"},
		{name: "second boundary", bead: "gt-1", at: second, want: "two"},
		{name: "after last", bead: "gt-1", at: second.Add(time.Hour), want: "two"},
		{name: "bead with no versions", bead: "gt-none", at: second, wantErr: ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := store.AsOf(tt.bead, tt.at)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("AsOf error = %v; want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if got.Text != tt.want {
				t.Errorf("AsOf text = %q; want %q", got.Text, tt.want)
			}
		})
	}
}

// TestHashIsSha256OfText: a version's hash is the sha256 hex of the text, checked
// against a known digest for "hello".
func TestHashIsSha256OfText(t *testing.T) {
	t.Parallel()
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"

	store := NewMemoryStore()
	if _, err := store.Put("gt-1", "hello", "furiosa", testStart); err != nil {
		t.Fatalf("Put error = %v; want nil", err)
	}
	versions, err := store.Versions("gt-1")
	if err != nil {
		t.Fatalf("Versions error = %v; want nil", err)
	}
	if len(versions) != 1 {
		t.Fatalf("Versions length = %d; want 1", len(versions))
	}
	if versions[0].Hash != want {
		t.Errorf("Hash = %q; want %q", versions[0].Hash, want)
	}
}

// TestPutRefusesEmptyBeadAndZeroTime: Put refuses an empty bead id and a zero
// time with their typed errors, and records nothing.
func TestPutRefusesEmptyBeadAndZeroTime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		bead string
		at   time.Time
		want error
	}{
		{name: "empty bead", bead: "", at: testStart, want: ErrEmptyBead},
		{name: "zero time", bead: "gt-1", at: time.Time{}, want: ErrZeroTime},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := NewMemoryStore()
			recorded, err := store.Put(tt.bead, "text", "furiosa", tt.at)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Put error = %v; want %v", err, tt.want)
			}
			if recorded {
				t.Error("Put recorded a version; want recorded=false")
			}
			if tt.bead != "" {
				versions, err := store.Versions(tt.bead)
				if err != nil {
					t.Fatalf("Versions error = %v; want nil", err)
				}
				if len(versions) != 0 {
					t.Errorf("Versions length = %d; want 0", len(versions))
				}
			}
		})
	}
}

// TestPutRefusesOutOfOrderTime: Put refuses a time at or before the bead's
// latest version, so the history AsOf searches stays strictly ordered.
func TestPutRefusesOutOfOrderTime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		at   time.Time
	}{
		{name: "earlier time", text: "later text", at: testStart.Add(-time.Second)},
		{name: "equal time", text: "later text", at: testStart},
		{name: "identical text earlier", text: "first text", at: testStart.Add(-time.Second)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := NewMemoryStore()
			if _, err := store.Put("gt-1", "first text", "furiosa", testStart); err != nil {
				t.Fatalf("setup Put error = %v; want nil", err)
			}

			recorded, err := store.Put("gt-1", tt.text, "furiosa", tt.at)
			if !errors.Is(err, ErrOutOfOrder) {
				t.Fatalf("Put error = %v; want ErrOutOfOrder", err)
			}
			if recorded {
				t.Error("Put recorded a version; want recorded=false")
			}
			versions, err := store.Versions("gt-1")
			if err != nil {
				t.Fatalf("Versions error = %v; want nil", err)
			}
			if len(versions) != 1 {
				t.Errorf("Versions length = %d; want 1", len(versions))
			}
		})
	}
}

// TestVersionsReturnsOldestFirstAndACopy: Versions lists history oldest first
// and returns a copy, so a caller's edit does not change the store.
func TestVersionsReturnsOldestFirstAndACopy(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	for i, text := range []string{"one", "two", "three"} {
		at := testStart.Add(time.Duration(i) * time.Minute)
		if _, err := store.Put("gt-1", text, "furiosa", at); err != nil {
			t.Fatalf("Put %q error = %v; want nil", text, err)
		}
	}

	versions, err := store.Versions("gt-1")
	if err != nil {
		t.Fatalf("Versions error = %v; want nil", err)
	}
	want := []string{"one", "two", "three"}
	if len(versions) != len(want) {
		t.Fatalf("Versions length = %d; want %d", len(versions), len(want))
	}
	for i := range want {
		if versions[i].Text != want[i] {
			t.Errorf("versions[%d].Text = %q; want %q", i, versions[i].Text, want[i])
		}
	}

	versions[0].Text = "edited"
	again, err := store.Versions("gt-1")
	if err != nil {
		t.Fatalf("second Versions error = %v; want nil", err)
	}
	if again[0].Text != "one" {
		t.Errorf("after caller edit, Versions text = %q; want one", again[0].Text)
	}
}

// TestZeroValueStoreIsUsable: a MemoryStore declared without NewMemoryStore
// records and reads versions instead of panicking on a nil map.
func TestZeroValueStoreIsUsable(t *testing.T) {
	t.Parallel()
	var store MemoryStore

	recorded, err := store.Put("gt-1", "text", "furiosa", testStart)
	if err != nil || !recorded {
		t.Fatalf("Put = %v, %v; want recorded", recorded, err)
	}
	versions, err := store.Versions("gt-1")
	if err != nil {
		t.Fatalf("Versions error = %v; want nil", err)
	}
	if len(versions) != 1 || versions[0].Text != "text" {
		t.Errorf("Versions = %+v; want one version with text", versions)
	}
}

// TestReadsRefuseEmptyBead: Versions and AsOf refuse an empty bead id rather
// than treating it as a bead of that name.
func TestReadsRefuseEmptyBead(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()

	if _, err := store.Versions(""); !errors.Is(err, ErrEmptyBead) {
		t.Errorf("Versions error = %v; want ErrEmptyBead", err)
	}
	if _, err := store.AsOf("", testStart); !errors.Is(err, ErrEmptyBead) {
		t.Errorf("AsOf error = %v; want ErrEmptyBead", err)
	}
}
