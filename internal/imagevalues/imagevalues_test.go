package imagevalues_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

const (
	testCommit = "0123456789abcdef0123456789abcdef01234567"
	testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

// testValues is a valid pair at the fixture commit.
func testValues() imagevalues.Values {
	return imagevalues.Values{
		Daemon:  imagevalues.Ref(imagevalues.DaemonImage, testDigest),
		Polecat: imagevalues.Ref(imagevalues.PolecatImage, testDigest),
		Commit:  testCommit,
	}
}

// TestImageValuesRoundTrips holds the writer and reader together: what Write
// records, Load answers whole.
func TestImageValuesRoundTrips(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "values", "images.yaml")
	want := testValues()
	if err := imagevalues.Write(path, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := imagevalues.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
}

// TestImageValuesLoadRefusesAMalformedFile holds the reader's refusals: a file
// that does not parse, or one whose pair or commit is not the shape a deploy
// can use, is an error rather than a partial pair.
func TestImageValuesLoadRefusesAMalformedFile(t *testing.T) {
	t.Parallel()
	good := testValues()
	rows := []struct {
		name string
		file string
		want string
	}{
		{"not YAML", "daemon: [", "does not parse"},
		{"no daemon line", "polecat: " + good.Polecat + "\ncommit: " + good.Commit + "\n", "no daemon line"},
		{"no polecat line", "daemon: " + good.Daemon + "\ncommit: " + good.Commit + "\n", "no polecat line"},
		{"no commit line", "daemon: " + good.Daemon + "\npolecat: " + good.Polecat + "\n", "no commit line"},
		{"a tag instead of a digest", "daemon: gastown-daemon:0123456789ab\npolecat: " + good.Polecat + "\ncommit: " + good.Commit + "\n", "daemon"},
		{"an uppercase digest", "daemon: " + strings.Replace(good.Daemon, "sha256:1111", "sha256:111A", 1) + "\npolecat: " + good.Polecat + "\ncommit: " + good.Commit + "\n", "daemon"},
		{"a short commit", "daemon: " + good.Daemon + "\npolecat: " + good.Polecat + "\ncommit: 0123456\n", "commit"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "images.yaml")
			if err := os.WriteFile(path, []byte(row.file), 0o600); err != nil {
				t.Fatalf("writing the fixture: %v", err)
			}
			got, err := imagevalues.Load(path)
			if err == nil {
				t.Fatalf("Load = %+v, want an error", got)
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Errorf("Load error = %q, want it to name %q", err, row.want)
			}
		})
	}
}

// TestImageValuesWriteRefusesAPairItCannotRecord holds the writer's refusals
// before any file is touched: an invalid pair, a directory it cannot create,
// and a target it cannot replace.
func TestImageValuesWriteRefusesAPairItCannotRecord(t *testing.T) {
	t.Parallel()

	t.Run("an invalid pair", func(t *testing.T) {
		t.Parallel()
		err := imagevalues.Write(filepath.Join(t.TempDir(), "images.yaml"), imagevalues.Values{})
		if err == nil || !strings.Contains(err.Error(), "no daemon line") {
			t.Fatalf("Write = %v, want a refusal naming the missing daemon line", err)
		}
	})

	t.Run("a directory that cannot be created", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		file := filepath.Join(dir, "not-a-directory")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("writing the fixture: %v", err)
		}
		err := imagevalues.Write(filepath.Join(file, "images.yaml"), testValues())
		if err == nil || !strings.Contains(err.Error(), "create") {
			t.Fatalf("Write below a regular file = %v, want a create refusal", err)
		}
	})

	t.Run("a target that cannot be replaced", func(t *testing.T) {
		t.Parallel()
		err := imagevalues.Write(t.TempDir(), testValues())
		if err == nil || !strings.Contains(err.Error(), "write") {
			t.Fatalf("Write onto a directory = %v, want a write refusal", err)
		}
	})
}

// TestImageValuesLoadKeepsAMissingFileDistinct holds the absence rule: a file
// that is not there reports fs.ErrNotExist, so the caller can tell a first run
// from a file it could not read.
func TestImageValuesLoadKeepsAMissingFileDistinct(t *testing.T) {
	t.Parallel()
	if _, err := imagevalues.Load(filepath.Join(t.TempDir(), "absent.yaml")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load of a missing file = %v, want fs.ErrNotExist", err)
	}
}

// TestImageValuesValidators holds the shapes the command and the reader share.
func TestImageValuesValidators(t *testing.T) {
	t.Parallel()
	commitRows := map[string]bool{
		testCommit: true,
		"0123456789abcdef0123456789abcdef0123456":    false,
		"0123456789abcdef0123456789abcdef0123456789": false,
		"0123456789ABCDEF0123456789abcdef01234567":   false,
	}
	for value, want := range commitRows {
		if got := imagevalues.ValidCommit(value); got != want {
			t.Errorf("ValidCommit(%q) = %v, want %v", value, got, want)
		}
	}
	digestRows := map[string]bool{
		testDigest:                          true,
		"sha256:" + strings.Repeat("a", 63): false,
		"sha512:" + strings.Repeat("a", 64): false,
		"sha256:" + strings.Repeat("A", 64): false,
	}
	for value, want := range digestRows {
		if got := imagevalues.ValidDigest(value); got != want {
			t.Errorf("ValidDigest(%q) = %v, want %v", value, got, want)
		}
	}
}
