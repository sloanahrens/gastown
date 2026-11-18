package bdskew

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the unit tier's checks: no goroutine outlives its tests, and
// no test starts an external tool (docs/testing.md, "The rules"). The package
// reads no environment and starts nothing, so unittier.Main is enough.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// testPinnedCommit is a real bd commit sha in the full lowercase form
// LevelAtCommit accepts.
const testPinnedCommit = "b7e5e9ceefaf49a701f3e2c1487a72aa0feb077f"

// fakeLister answers ListTree with names and err, recording what it was asked,
// so no test starts a process.
type fakeLister struct {
	names  []string
	err    error
	calls  int
	commit string
	dir    string
}

func (f *fakeLister) ListTree(_ context.Context, commit, dir string) ([]string, error) {
	f.calls++
	f.commit, f.dir = commit, dir
	return f.names, f.err
}

// TestPreRollGuardMaxMigration: the highest migration number is the level, both
// halves of a migration count, and order in the listing does not matter.
func TestPreRollGuardMaxMigration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reason string
		names  []string
		want   int
	}{
		{
			reason: "the samples migration and both of its halves",
			names: []string{
				"0066_add_events_journal_actor.up.sql",
				"0066_add_events_journal_actor.down.sql",
				"0067_create_samples.up.sql",
				"0067_create_samples.down.sql",
			},
			want: 67,
		},
		{
			reason: "a listing that stops at add_events_journal_actor",
			names: []string{
				"0065_add_bead_versions.up.sql",
				"0066_add_events_journal_actor.up.sql",
				"0066_add_events_journal_actor.down.sql",
			},
			want: 66,
		},
		{
			reason: "an unordered listing takes the highest number",
			names:  []string{"0010_ten.up.sql", "0100_hundred.up.sql", "0042_forty_two.up.sql"},
			want:   100,
		},
		{
			reason: "a number that is not zero padded",
			names:  []string{"67_create_samples.up.sql"},
			want:   67,
		},
		{
			reason: "one migration",
			names:  []string{"0001_initial.up.sql"},
			want:   1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()
			got, err := MaxMigration(tt.names)
			if err != nil {
				t.Fatalf("MaxMigration(%v) returned error %v", tt.names, err)
			}
			if got != tt.want {
				t.Errorf("MaxMigration(%v) = %d, want %d", tt.names, got, tt.want)
			}
		})
	}
}

// TestPreRollGuardMaxMigrationRefuses: a listing that cannot be read as
// migrations is an error and never level zero.
func TestPreRollGuardMaxMigrationRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reason string
		names  []string
		want   error
	}{
		{reason: "a nil listing", want: ErrEmptyListing},
		{reason: "a listing with no entries", names: []string{}, want: ErrEmptyListing},
		{reason: "a name with no number", names: []string{"README.md"}, want: ErrUnnumbered},
		{reason: "a name with no underscore after its number", names: []string{"0067"}, want: ErrUnnumbered},
		{reason: "a name whose number is empty", names: []string{"_0067_create_samples.up.sql"}, want: ErrUnnumbered},
		{reason: "a name whose number holds a letter", names: []string{"00a7_create_samples.up.sql"}, want: ErrUnnumbered},
		{reason: "a name whose number is signed", names: []string{"-0067_create_samples.up.sql"}, want: ErrUnnumbered},
		{reason: "a number too large for an int", names: []string{"99999999999999999999_create_samples.up.sql"}, want: ErrNumberOutOfRange},
		{reason: "an unnumbered name after a numbered one", names: []string{"0067_create_samples.up.sql", "README.md"}, want: ErrUnnumbered},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()
			got, err := MaxMigration(tt.names)
			if !errors.Is(err, tt.want) {
				t.Fatalf("MaxMigration(%v) error = %v, want %v", tt.names, err, tt.want)
			}
			if got != 0 {
				t.Errorf("MaxMigration(%v) = %d with error %v, want 0", tt.names, got, err)
			}
		})
	}
}

// TestPreRollGuardLevelAtCommit: the level of a commit is read from the
// migrations directory through the lister, and an unread listing is an error
// rather than a level.
func TestPreRollGuardLevelAtCommit(t *testing.T) {
	t.Parallel()
	listerErr := errors.New("bdskew test: git cannot read the commit")
	tests := []struct {
		reason  string
		names   []string
		err     error
		want    int
		wantErr error
	}{
		{
			reason: "a pinned commit carrying the samples migration",
			names:  []string{"0066_add_events_journal_actor.up.sql", "0067_create_samples.up.sql", "0067_create_samples.down.sql"},
			want:   67,
		},
		{
			reason:  "a lister error is returned, not a level",
			err:     listerErr,
			wantErr: listerErr,
		},
		{
			reason:  "an empty listing is an error",
			wantErr: ErrEmptyListing,
		},
		{
			reason:  "an unnumbered entry is an error",
			names:   []string{"README.md"},
			wantErr: ErrUnnumbered,
		},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()
			l := &fakeLister{names: tt.names, err: tt.err}
			got, err := LevelAtCommit(context.Background(), l, testPinnedCommit)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("LevelAtCommit error = %v, want %v", err, tt.wantErr)
				}
				if got != 0 {
					t.Errorf("LevelAtCommit = %d with error %v, want 0", got, err)
				}
			} else {
				if err != nil {
					t.Fatalf("LevelAtCommit returned error %v", err)
				}
				if got != tt.want {
					t.Errorf("LevelAtCommit = %d, want %d", got, tt.want)
				}
			}
			if l.calls != 1 {
				t.Fatalf("ListTree calls = %d, want 1", l.calls)
			}
			if l.commit != testPinnedCommit {
				t.Errorf("ListTree commit = %q, want %q", l.commit, testPinnedCommit)
			}
			if l.dir != MigrationsDir {
				t.Errorf("ListTree dir = %q, want %q", l.dir, MigrationsDir)
			}
		})
	}
}

// TestPreRollGuardLevelAtCommitRefuses: a missing lister and a commit that is
// not a full sha are refused before the lister is called.
func TestPreRollGuardLevelAtCommitRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reason string
		lister Lister
		commit string
		want   error
	}{
		{reason: "no lister", commit: testPinnedCommit, want: ErrNoLister},
		{reason: "an empty commit", lister: &fakeLister{}, want: ErrBadCommit},
		{reason: "an abbreviated commit", lister: &fakeLister{}, commit: testPinnedCommit[:7], want: ErrBadCommit},
		{reason: "a commit with a non-hex character", lister: &fakeLister{}, commit: strings.Repeat("g", 40), want: ErrBadCommit},
		{reason: "an uppercase commit", lister: &fakeLister{}, commit: strings.ToUpper(testPinnedCommit), want: ErrBadCommit},
		{reason: "an option-shaped commit", lister: &fakeLister{}, commit: "--upload-pack=sh", want: ErrBadCommit},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()
			got, err := LevelAtCommit(context.Background(), tt.lister, tt.commit)
			if !errors.Is(err, tt.want) {
				t.Fatalf("LevelAtCommit(commit %q) error = %v, want %v", tt.commit, err, tt.want)
			}
			if got != 0 {
				t.Errorf("LevelAtCommit(commit %q) = %d with error %v, want 0", tt.commit, got, err)
			}
			if fake, ok := tt.lister.(*fakeLister); ok && fake.calls != 0 {
				t.Errorf("ListTree was called %d times for a refused commit, want 0", fake.calls)
			}
		})
	}
}

// TestPreRollGuardCompare: equal levels are no skew, and unequal levels name
// both.
func TestPreRollGuardCompare(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reason   string
		pin      int
		database int
		want     *Skew
	}{
		{reason: "equal levels agree", pin: 66, database: 66},
		{reason: "both at zero agree", pin: 0, database: 0},
		{reason: "a pin ahead of the database skews", pin: 67, database: 66, want: &Skew{Pin: 67, Database: 66}},
		{reason: "a pin behind the database skews", pin: 66, database: 67, want: &Skew{Pin: 66, Database: 67}},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()
			got := Compare(tt.pin, tt.database)
			switch {
			case tt.want == nil && got != nil:
				t.Fatalf("Compare(%d, %d) = %+v, want nil", tt.pin, tt.database, got)
			case tt.want != nil && got == nil:
				t.Fatalf("Compare(%d, %d) = nil, want %+v", tt.pin, tt.database, tt.want)
			case tt.want != nil && *got != *tt.want:
				t.Errorf("Compare(%d, %d) = %+v, want %+v", tt.pin, tt.database, *got, *tt.want)
			}
		})
	}
}
