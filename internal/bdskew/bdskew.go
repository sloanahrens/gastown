// Package bdskew reads the schema level a bd commit carries and compares it
// with a database's level, so a pre-roll pair check can tell when the bd pin
// brings schema migrations the database has not run yet.
//
// The package is pure over the Lister a caller supplies: it runs no git and
// opens no network. gt-jkcuq6 supplies the Lister that reads a bare clone's
// git tree, and gt-5f0kue the pair check that compares the two levels.
package bdskew

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MigrationsDir is the directory a bd repository keeps its schema migrations
// in, relative to the repository root, and the directory LevelAtCommit asks a
// Lister for.
const MigrationsDir = "internal/storage/schema/migrations"

var (
	// ErrEmptyListing reports a listing that holds no migration file. A commit
	// whose migrations cannot be listed says nothing about its level, and
	// reading it as zero would compare equal to a database before its first
	// migration.
	ErrEmptyListing = errors.New("bdskew: empty migration listing")
	// ErrUnnumbered reports a listing entry whose name does not open with a
	// migration number, the run of digits before the first underscore.
	ErrUnnumbered = errors.New("bdskew: migration name without a leading number")
	// ErrNumberOutOfRange reports a numbered name whose number is too large for
	// an int, which is a real number rather than an unnumbered name.
	ErrNumberOutOfRange = errors.New("bdskew: migration number out of range")
	// ErrNoLister reports LevelAtCommit with no Lister to read through.
	ErrNoLister = errors.New("bdskew: no lister")
	// ErrBadCommit reports a commit that is not a full 40-character lowercase
	// sha. The value reaches git's argv in the Lister, so an abbreviation or an
	// option-shaped string is refused before that call.
	ErrBadCommit = errors.New("bdskew: commit is not a full sha")
)

// commitForm is the shape a commit must have to be read: a full lowercase sha,
// so an abbreviation another commit can claim never reaches the lister.
var commitForm = regexp.MustCompile(`^[0-9a-f]{40}$`)

// nonMigrationNames are the files bd keeps in MigrationsDir that are not
// migrations (README.md, ignored, nondeterminism-allowlist.txt at the pinned
// commit), skipped by exact whole name; any other unnumbered entry is refused.
var nonMigrationNames = map[string]struct{}{
	"README.md":                    {},
	"ignored":                      {},
	"nondeterminism-allowlist.txt": {},
}

// Lister lists the file names a directory holds at one commit of a
// repository.
type Lister interface {
	// ListTree returns the base names of the entries directly under dir at
	// commit. A listing that cannot be read is an error, never an empty slice.
	ListTree(ctx context.Context, commit, dir string) ([]string, error)
}

// MaxMigration returns the highest migration number in names, the file names
// under MigrationsDir such as 0067_create_samples.up.sql. Both halves of a
// migration carry the same number, so either one sets it.
//
// An empty listing is ErrEmptyListing, an entry that does not start with a
// number and an underscore is ErrUnnumbered, and one whose number does not fit
// an int is ErrNumberOutOfRange. The known non-migration files in
// nonMigrationNames are skipped by exact name; every other entry must be a
// migration, because skipping a name this function does not understand would
// let a mistyped migration hide a level. A listing with no migration once the
// known names are skipped is ErrEmptyListing, never level zero.
func MaxMigration(names []string) (int, error) {
	max := -1
	for _, name := range names {
		if _, ok := nonMigrationNames[name]; ok {
			continue
		}
		number, err := migrationNumber(name)
		if err != nil {
			return 0, err
		}
		if number > max {
			max = number
		}
	}
	if max < 0 {
		return 0, ErrEmptyListing
	}
	return max, nil
}

// migrationNumber reads the migration number a file name starts with: the
// digits before its first underscore.
func migrationNumber(name string) (int, error) {
	digits, _, found := strings.Cut(name, "_")
	if !found || digits == "" {
		return 0, fmt.Errorf("%w: %q", ErrUnnumbered, name)
	}
	// Atoi accepts a leading sign, and the number is a run of digits only, so
	// the run is checked before it is parsed.
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%w: %q", ErrUnnumbered, name)
		}
	}
	number, err := strconv.Atoi(digits)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrNumberOutOfRange, name)
	}
	return number, nil
}

// LevelAtCommit returns the highest migration number the repository holds at
// commit, read through l from MigrationsDir.
//
// A nil l is ErrNoLister, a commit that is not a full sha is ErrBadCommit, and
// a failed or unreadable listing is an error, so a caller never takes an
// unread level for level zero and never compares two levels it did not read.
func LevelAtCommit(ctx context.Context, l Lister, commit string) (int, error) {
	if l == nil {
		return 0, ErrNoLister
	}
	if !commitForm.MatchString(commit) {
		return 0, fmt.Errorf("%w: %q", ErrBadCommit, commit)
	}
	names, err := l.ListTree(ctx, commit, MigrationsDir)
	if err != nil {
		return 0, fmt.Errorf("bdskew: listing %s at %s: %w", MigrationsDir, commit, err)
	}
	level, err := MaxMigration(names)
	if err != nil {
		return 0, fmt.Errorf("bdskew: level at %s: %w", commit, err)
	}
	return level, nil
}

// Skew names the schema levels a bd pin and a database disagree on: the level
// the pinned bd commit carries, and the level the database has migrated to.
type Skew struct {
	Pin      int
	Database int
}

// Compare returns nil when the pin and the database are at the same schema
// level, and the Skew naming both when they are not.
func Compare(pin, database int) *Skew {
	if pin == database {
		return nil
	}
	return &Skew{Pin: pin, Database: database}
}
