package mayorrig

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the tests under unittier.Main, which fails the run when a
// goroutine outlives the tests.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// mayorRigAllowPath names the ratchet file listing every non-test Go file that
// still uses the <rig>/mayor/rig path.
const mayorRigAllowPath = "internal/mayorrig/testdata/mayor-rig-allow.txt"

// mayorRigAllowClass is the only allowlist class. An entry stays until the
// retirement bead that removes the use deletes its line.
const mayorRigAllowClass = "pending-retirement"

// The segments of the path. A use is a string that holds the path, a concatenation
// that spells it, a call whose arguments include both segments, or an identifier
// that names one of the constants that hold or build the path.
const (
	mayorSegment = "mayor"
	rigSegment   = "rig"
)

// mayorRigPathPattern matches the path as whole segments. mayor/rigs.json, the rig
// registry, contains the characters but names another file, so it is not a use.
var mayorRigPathPattern = regexp.MustCompile(`(^|[^A-Za-z0-9_-])mayor/rig($|[^A-Za-z0-9_-])`)

// mayorRigIdents are the constants that name the path: DirMayor and DirRig name its
// segments, and RigMayorPath and RigBeadsPath build it. Every occurrence counts,
// including the definitions in internal/constants.
var mayorRigIdents = map[string]bool{"DirMayor": true, "DirRig": true, "RigMayorPath": true, "RigBeadsPath": true}

// skippedScanDirs are the directories the scan does not descend into. No other
// directory is skipped by name: a skip by a name a new package could take is a
// way around the ratchet.
var skippedScanDirs = map[string]bool{
	".git":         true,
	"vendor":       true,
	"node_modules": true,
	"testdata":     true,
}

// mayorRigHit is the number of uses one non-test file makes, and the line of the
// first one.
type mayorRigHit struct {
	file  string
	line  int
	count int
}

// mayorRigEntry is one allowlist line. count is the number of uses the file may
// make; a new use fails the ratchet, and so does a count the file no longer
// reaches.
type mayorRigEntry struct {
	file   string
	count  int
	reason string
	line   int
}

// TestNoNewMayorRigUse is the ratchet: a non-test file that gains a use of the
// path fails here, naming the file, unless the allowlist lists it.
func TestNoNewMayorRigUse(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFixture(t, root, "pkg/run.go", "package pkg\n\nfunc run() string {\n\treturn \"x/mayor/rig/y\"\n}\n")
	allowPath := writeFixture(t, root, "allow.txt", "# no entries\n")

	err := checkMayorRigAt(root, allowPath)
	if err == nil {
		t.Fatal("checkMayorRigAt = nil, want a refusal naming pkg/run.go:4")
	}
	for _, want := range []string{"pkg/run.go:4 has 1 mayor/rig use(s)", allowPath} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("checkMayorRigAt error = %q, want it to contain %q", err, want)
		}
	}
}

// TestMayorRigAllowlistIsTight keeps the list a record of what remains: an entry
// whose file has fewer uses than its count fails until the count is lowered, and
// an entry whose file no longer has a use fails until its line is deleted.
func TestMayorRigAllowlistIsTight(t *testing.T) {
	t.Parallel()

	const oneUse = "package pkg\n\nfunc run() string {\n\treturn \"mayor/rig\"\n}\n"
	cases := []struct {
		name   string
		source string
		line   string
		want   string
	}{
		{
			name:   "a count above the file's uses fails until it is lowered",
			source: oneUse,
			line:   "pkg/run.go\t2\tpending-retirement\tpkg: the run path",
			want:   "pkg/run.go allows 2, the file has 1 use(s); lower the count to 1",
		},
		{
			name:   "an entry whose file has no use fails until its line is deleted",
			source: "package pkg\n\nfunc run() {}\n",
			line:   "pkg/run.go\t1\tpending-retirement\tpkg: the run path",
			want:   "pkg/run.go has no mayor/rig use; delete the line",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFixture(t, root, "pkg/run.go", tc.source)
			allowPath := writeFixture(t, root, "allow.txt", tc.line+"\n")

			err := checkMayorRigAt(root, allowPath)
			if err == nil {
				t.Fatalf("checkMayorRigAt = nil, want a refusal containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("checkMayorRigAt error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestMayorRigCountIsEnforced holds a listed file to its line's count. A file
// that gains a use fails even though it is listed, and an exact match passes.
func TestMayorRigCountIsEnforced(t *testing.T) {
	t.Parallel()

	const reason = "pkg: the run path"
	const oneUse = "package pkg\n\nfunc load() string {\n\treturn \"mayor/rig\"\n}\n"
	const twoUses = "package pkg\n\nfunc load() string {\n\treturn \"mayor/rig\"\n}\n\nfunc unload() string {\n\treturn \"mayor/rig/x\"\n}\n"
	cases := []struct {
		name   string
		source string
		count  string
		want   string
	}{
		{
			name:   "an extra use in a listed file fails",
			source: twoUses,
			count:  "1",
			want:   "pkg/run.go:4 has 2 mayor/rig use(s)",
		},
		{
			name:   "an exact match passes",
			source: twoUses,
			count:  "2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFixture(t, root, "pkg/run.go", tc.source)
			allowPath := writeFixture(t, root, "allow.txt", "pkg/run.go\t"+tc.count+"\tpending-retirement\t"+reason+"\n")

			err := checkMayorRigAt(root, allowPath)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("checkMayorRigAt = %v, want no refusal", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkMayorRigAt = nil, want a refusal containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("checkMayorRigAt error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestMayorRigAllowlistRules refuses every line the format does not allow: a
// wildcard path, a blank path, a blank reason, any class but pending-retirement,
// a count that is not a positive integer, a duplicate, and a line that is not
// four fields.
func TestMayorRigAllowlistRules(t *testing.T) {
	t.Parallel()

	const reason = "pkg: the run path"
	cases := []struct {
		name  string
		line  string
		extra string
		want  string
	}{
		{
			name: "a wildcard path is refused",
			line: "pkg/*.go\t1\tpending-retirement\t" + reason,
			want: "wildcard",
		},
		{
			name: "a blank path is refused",
			line: "\t1\tpending-retirement\t" + reason,
			want: "entry has no path",
		},
		{
			name: "a blank reason is refused",
			line: "pkg/run.go\t1\tpending-retirement\t",
			want: "has no reason",
		},
		{
			name: "a whitespace-only reason is refused",
			line: "pkg/run.go\t1\tpending-retirement\t   ",
			want: "has no reason",
		},
		{
			name: "a class other than pending-retirement is refused",
			line: "pkg/run.go\t1\treal\t" + reason,
			want: `class "real" is not pending-retirement`,
		},
		{
			name: "a zero count is refused",
			line: "pkg/run.go\t0\tpending-retirement\t" + reason,
			want: `count "0" for pkg/run.go is not a positive integer`,
		},
		{
			name: "a count that is not a number is refused",
			line: "pkg/run.go\tmany\tpending-retirement\t" + reason,
			want: `count "many" for pkg/run.go is not a positive integer`,
		},
		{
			name: "a blank count is refused",
			line: "pkg/run.go\t\tpending-retirement\t" + reason,
			want: `count "" for pkg/run.go is not a positive integer`,
		},
		{
			name:  "a duplicate file is refused",
			line:  "pkg/run.go\t1\tpending-retirement\t" + reason,
			extra: "pkg/run.go\t1\tpending-retirement\tsecond " + reason,
			want:  "duplicate entry for pkg/run.go",
		},
		{
			name: "a line without four tab-separated fields is refused",
			line: "pkg/run.go 1 pending-retirement " + reason,
			want: "want path<TAB>count<TAB>class<TAB>reason",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFixture(t, root, "pkg/run.go", "package pkg\n\nfunc run() string {\n\treturn \"mayor/rig\"\n}\n")
			content := tc.line + "\n"
			if tc.extra != "" {
				content += tc.extra + "\n"
			}
			allowPath := writeFixture(t, root, "allow.txt", content)

			err := checkMayorRigAt(root, allowPath)
			if err == nil {
				t.Fatalf("checkMayorRigAt = nil, want the %q refusal", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("checkMayorRigAt error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestMayorRigUseCount fixes what one use is. A string literal that contains the
// path is one use however often it repeats it, a call with both segments as
// arguments is one use (a segment may be a literal or a name the file gives the
// value), a concatenation that spells the path is one use, and each
// constants.DirMayor, DirRig, RigMayorPath or RigBeadsPath identifier is one use.
func TestMayorRigUseCount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		source string
		want   int
	}{
		{
			name:   "a literal that contains the path is one use",
			source: "package pkg\n\nfunc run() string {\n\treturn \"x/mayor/rig/y\"\n}\n",
			want:   1,
		},
		{
			name:   "a literal that names the path twice is one use",
			source: "package pkg\n\nfunc run() string {\n\treturn \"mayor/rig and mayor/rig\"\n}\n",
			want:   1,
		},
		{
			name:   "two literals that name the path are two uses",
			source: "package pkg\n\nfunc run() []string {\n\treturn []string{\"mayor/rig\", \"x/mayor/rig\"}\n}\n",
			want:   2,
		},
		{
			name:   "a filepath.Join with mayor and rig arguments is one use",
			source: "package pkg\n\nfunc run(root string) string {\n\treturn filepath.Join(root, \"mayor\", \"rig\", \"x\")\n}\n",
			want:   1,
		},
		{
			name:   "a call with mayor and no rig argument is not a use",
			source: "package pkg\n\nfunc run(root string) string {\n\treturn filepath.Join(root, \"mayor\", \"x\")\n}\n",
			want:   0,
		},
		{
			name:   "mayor and rig in a slice literal are not a use",
			source: "package pkg\n\nvar segs = []string{\"mayor\", \"rig\"}\n",
			want:   0,
		},
		{
			name:   "a literal that holds only mayor is not a use",
			source: "package pkg\n\nvar name = \"mayor\"\n",
			want:   0,
		},
		{
			name:   "the rig registry mayor/rigs.json is not the path",
			source: "package pkg\n\nvar registry = \"mayor/rigs.json\"\n",
			want:   0,
		},
		{
			name:   "a path segment that only ends in mayor/rig is not a use",
			source: "package pkg\n\nvar p = \"remayor/rig/y\"\n",
			want:   0,
		},
		{
			name:   "a path segment that only starts with mayor/rig is not a use",
			source: "package pkg\n\nvar p = \"mayor/rigx/y\"\n",
			want:   0,
		},
		{
			name:   "constants.DirMayor and constants.DirRig are a use each",
			source: "package pkg\n\nfunc run(p string) string {\n\treturn p + constants.DirMayor + constants.DirRig\n}\n",
			want:   2,
		},
		{
			name:   "the definitions in the constants package are uses",
			source: "package constants\n\nconst (\n\tDirMayor = \"mayor\"\n\tDirRig   = \"rig\"\n)\n",
			want:   2,
		},
		{
			name:   "an identifier that contains DirMayor as a prefix is not a use",
			source: "package pkg\n\nfunc run() string {\n\treturn DirMayorOld\n}\n",
			want:   0,
		},
		{
			name:   "a const holding mayor joined with a rig literal is one use",
			source: "package pkg\n\nconst m = \"mayor\"\n\nfunc run(root string) string {\n\treturn filepath.Join(root, m, \"rig\")\n}\n",
			want:   1,
		},
		{
			name:   "a name assigned mayor and joined with rig is one use",
			source: "package pkg\n\nfunc run(root string) string {\n\tm := \"mayor\"\n\treturn filepath.Join(root, m, \"rig\")\n}\n",
			want:   1,
		},
		{
			name:   "a name declared after the call that joins it is one use",
			source: "package pkg\n\nfunc run(root string) string {\n\treturn filepath.Join(root, m, r)\n}\n\nvar m, r = \"mayor\", \"rig\"\n",
			want:   1,
		},
		{
			name:   "a call joining a literal mayor with a const holding rig is one use",
			source: "package pkg\n\nconst r = \"rig\"\n\nfunc run(root string) string {\n\treturn filepath.Join(root, \"mayor\", r)\n}\n",
			want:   1,
		},
		{
			name:   "a segment read at run time is not seen (the accepted limit)",
			source: "package pkg\n\nfunc run(root, r string) string {\n\treturn filepath.Join(root, \"mayor\", r)\n}\n",
			want:   0,
		},
		{
			name:   "a path concatenated from two literals is one use",
			source: "package pkg\n\nconst p = \"mayor/\" + \"rig\"\n",
			want:   1,
		},
		{
			name:   "a concatenation with a literal that holds the path counts once",
			source: "package pkg\n\nvar p = \"x/mayor/rig/\" + \"y\"\n",
			want:   1,
		},
		{
			name:   "a call of constants.RigMayorPath is one use",
			source: "package pkg\n\nfunc run(rig string) string {\n\treturn constants.RigMayorPath(rig)\n}\n",
			want:   1,
		},
		{
			name:   "a call of constants.RigBeadsPath is one use",
			source: "package pkg\n\nfunc run(rig string) string {\n\treturn constants.RigBeadsPath(rig)\n}\n",
			want:   1,
		},
		{
			name:   "a comment naming the path is not a use",
			source: "package pkg\n\n// mayor/rig is the retired clone.\nfunc run() {}\n",
			want:   0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFixture(t, root, "pkg/run.go", tc.source)

			hits, err := mayorRigHits(root)
			if err != nil {
				t.Fatalf("mayorRigHits = %v, want no error", err)
			}
			got := 0
			for _, hit := range hits {
				got += hit.count
			}
			if got != tc.want {
				t.Fatalf("mayorRigHits counted %d uses, want %d (hits %+v)", got, tc.want, hits)
			}
		})
	}
}

// TestMayorRigNotCounted fixes what the scan does not visit: a test file, a
// testdata, vendor, node_modules or .git tree, and a non-Go file. None of them
// needs an allowlist line.
func TestMayorRigNotCounted(t *testing.T) {
	t.Parallel()

	const literal = "package pkg\n\nfunc run() string {\n\treturn \"mayor/rig\"\n}\n"
	cases := []struct {
		name   string
		path   string
		source string
	}{
		{
			name:   "a test file naming the path is not counted",
			path:   "pkg/run_test.go",
			source: literal,
		},
		{
			name:   "a testdata directory is not scanned",
			path:   "pkg/testdata/fixture.go",
			source: literal,
		},
		{
			name:   "a vendor directory is not scanned",
			path:   "vendor/example.com/x/x.go",
			source: literal,
		},
		{
			name:   "a node_modules directory is not scanned",
			path:   "node_modules/x/x.go",
			source: literal,
		},
		{
			name:   "a .git directory is not scanned",
			path:   ".git/hooks/x.go",
			source: literal,
		},
		{
			name:   "a non-Go file is not scanned",
			path:   "pkg/notes.txt",
			source: "mayor/rig\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFixture(t, root, tc.path, tc.source)
			allowPath := writeFixture(t, root, "allow.txt", "# no entries\n")

			if err := checkMayorRigAt(root, allowPath); err != nil {
				t.Fatalf("checkMayorRigAt = %v, want no refusal", err)
			}
		})
	}
}

// TestMayorRigScanRootIsNotSkippedByName keeps the skip list from passing a tree
// whose root directory has a skipped name: the skip applies below the root.
func TestMayorRigScanRootIsNotSkippedByName(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "vendor")
	writeFixture(t, root, "pkg/run.go", "package pkg\n\nfunc run() string {\n\treturn \"mayor/rig\"\n}\n")
	allowPath := writeFixture(t, root, "allow.txt", "# no entries\n")

	err := checkMayorRigAt(root, allowPath)
	if err == nil || !strings.Contains(err.Error(), "pkg/run.go:4 has 1 mayor/rig use(s)") {
		t.Fatalf("checkMayorRigAt = %v, want a refusal naming pkg/run.go:4 under a root named vendor", err)
	}
}

// TestMayorRigUnreadableAllowlistIsAnError keeps a missing, unreadable or
// unparsable allowlist from passing as an empty list: the error names the file.
func TestMayorRigUnreadableAllowlistIsAnError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		setup func(t *testing.T, root string) string
	}{
		{
			name: "a missing allowlist names the file",
			setup: func(t *testing.T, root string) string {
				return filepath.Join(root, "mayor-rig-allow.txt")
			},
		},
		{
			name: "an allowlist that is a directory names the file",
			setup: func(t *testing.T, root string) string {
				dir := filepath.Join(root, "mayor-rig-allow.txt")
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatalf("mkdir %s: %v", dir, err)
				}
				return dir
			},
		},
		{
			name: "an unparsable allowlist names the file",
			setup: func(t *testing.T, root string) string {
				return writeFixture(t, root, "mayor-rig-allow.txt", "pkg/run.go 1 pending-retirement pkg\n")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFixture(t, root, "pkg/run.go", "package pkg\n\nfunc run() {}\n")
			allowPath := tc.setup(t, root)

			err := checkMayorRigAt(root, allowPath)
			if err == nil {
				t.Fatal("checkMayorRigAt = nil, want an error naming the allowlist file")
			}
			if !strings.Contains(err.Error(), "mayor-rig-allow.txt") {
				t.Fatalf("checkMayorRigAt error = %q, want it to name mayor-rig-allow.txt", err)
			}
		})
	}
}

// TestMayorRigUnparsableSourceIsAnError keeps a Go file the parser rejects from
// being skipped: the scan fails and names the file.
func TestMayorRigUnparsableSourceIsAnError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFixture(t, root, "pkg/broken.go", "package pkg\n\nfunc run( {\n")
	allowPath := writeFixture(t, root, "allow.txt", "# no entries\n")

	err := checkMayorRigAt(root, allowPath)
	if err == nil {
		t.Fatal("checkMayorRigAt = nil, want an error naming pkg/broken.go")
	}
	if !strings.Contains(err.Error(), "pkg/broken.go") {
		t.Fatalf("checkMayorRigAt error = %q, want it to name pkg/broken.go", err)
	}
}

// unreadableDirFS is a filesystem whose directory dir cannot be opened, so the walk
// meets a read error there.
type unreadableDirFS struct {
	fs.FS
	dir string
}

func (f unreadableDirFS) Open(name string) (fs.File, error) {
	if name == f.dir {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	return f.FS.Open(name)
}

// TestMayorRigWalkErrorIsReturned keeps a directory the walk cannot read from being
// skipped: the scan returns the error and names the directory.
func TestMayorRigWalkErrorIsReturned(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFixture(t, root, "pkg/run.go", "package pkg\n\nfunc run() {}\n")
	writeFixture(t, root, "other/run.go", "package other\n\nfunc run() {}\n")

	_, err := scanMayorRig(unreadableDirFS{FS: os.DirFS(root), dir: "pkg"})
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("scanMayorRig = %v, want the permission error for pkg", err)
	}
	if !strings.Contains(err.Error(), "pkg") {
		t.Fatalf("scanMayorRig error = %q, want it to name pkg", err)
	}
}

// TestRepoMayorRig is the gate on the real tree: every mayor/rig use in the gt
// module is listed in the allowlist, and every listed use still exists.
func TestRepoMayorRig(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	if err := checkMayorRigAt(root, filepath.Join(root, mayorRigAllowPath)); err != nil {
		t.Fatal(err)
	}
}

// checkMayorRigAt fails for a non-test Go file under root that uses the path more
// often than its allowlist line allows, or that is not listed, and for an
// allowlist line that allows more than the file uses or names a file with no use.
// The allowlist is loaded first, so a broken file never reads as an empty list.
func checkMayorRigAt(root, allowPath string) error {
	entries, err := loadMayorRigAllowlist(allowPath)
	if err != nil {
		return err
	}
	hits, err := mayorRigHits(root)
	if err != nil {
		return err
	}
	if err := unlistedMayorRigHits(hits, entries, allowPath); err != nil {
		return err
	}
	return staleMayorRigEntries(entries, hits, allowPath)
}

// mayorRigHits scans every non-test Go file under root and returns one hit for
// each file that has a use. It fails on a directory or parse error rather than
// reporting a clean scan.
func mayorRigHits(root string) ([]mayorRigHit, error) {
	hits, err := scanMayorRig(os.DirFS(root))
	if err != nil {
		return nil, fmt.Errorf("scanning %s: %w", root, err)
	}
	return hits, nil
}

// scanMayorRig walks fsys and scans each non-test Go file in it. The filesystem is
// a parameter so a test can hand the walk a directory it cannot read.
func scanMayorRig(fsys fs.FS) ([]mayorRigHit, error) {
	var hits []mayorRigHit
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." && skippedScanDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		hit, err := mayorRigHitInFile(fsys, path)
		if err != nil {
			return err
		}
		if hit.count > 0 {
			hits = append(hits, hit)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return hits, nil
}

// mayorRigHitInFile parses one file and counts its uses of the path. The count is
// zero for a file with no use.
//
// The scan reads syntax only. A path assembled from a value read at run time
// (config, the environment, a function result) is not seen; that limit is accepted.
// A name is matched by its spelling within the file, so a name that holds a
// segment counts in every call of the file that joins it.
func mayorRigHitInFile(fsys fs.FS, rel string) (mayorRigHit, error) {
	data, err := fs.ReadFile(fsys, rel)
	if err != nil {
		return mayorRigHit{}, fmt.Errorf("read %s: %w", rel, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, data, 0)
	if err != nil {
		return mayorRigHit{}, fmt.Errorf("parse %s: %w", rel, err)
	}
	names := stringNames(file)
	hit := mayorRigHit{file: rel}
	note := func(pos token.Pos) {
		hit.count++
		if hit.line == 0 {
			hit.line = fset.Position(pos).Line
		}
	}
	var unquoteErr error
	ast.Inspect(file, func(n ast.Node) bool {
		if unquoteErr != nil {
			return false
		}
		switch n := n.(type) {
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			value, err := unquoteLiteral(fset, rel, n)
			if err != nil {
				unquoteErr = err
				return false
			}
			if mayorRigPathPattern.MatchString(value) {
				note(n.Pos())
			}
		case *ast.BinaryExpr:
			// A concatenation that spells the path across its operands. A literal
			// operand that holds the path alone is already counted, so it is not
			// counted again.
			if anyMayorRigPath(possibleStrings(n, names)) && !literalHoldsPath(n) {
				note(n.Pos())
			}
		case *ast.CallExpr:
			var hasMayor, hasRig bool
			for _, arg := range n.Args {
				for _, value := range possibleStrings(arg, names) {
					hasMayor = hasMayor || value == mayorSegment
					hasRig = hasRig || value == rigSegment
				}
			}
			if hasMayor && hasRig {
				note(n.Pos())
			}
		case *ast.Ident:
			if mayorRigIdents[n.Name] {
				note(n.Pos())
			}
		}
		return true
	})
	if unquoteErr != nil {
		return mayorRigHit{}, unquoteErr
	}
	return hit, nil
}

// stringNames maps each name the file assigns a string to every string it can
// hold: a const or var with a value, or a name assigned in a statement. A name
// may take its value from one declared later, so the pass repeats until no name
// gains a value. The repeat is bounded by the number of declarations.
func stringNames(file *ast.File) map[string][]string {
	type binding struct {
		name  string
		value ast.Expr
	}
	var bindings []binding
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ValueSpec:
			for i, name := range n.Names {
				if i < len(n.Values) {
					bindings = append(bindings, binding{name.Name, n.Values[i]})
				}
			}
		case *ast.AssignStmt:
			if len(n.Lhs) != len(n.Rhs) {
				return true
			}
			for i, lhs := range n.Lhs {
				if id, ok := lhs.(*ast.Ident); ok {
					bindings = append(bindings, binding{id.Name, n.Rhs[i]})
				}
			}
		}
		return true
	})
	names := map[string][]string{}
	changed := true
	for pass := 0; changed && pass <= len(bindings); pass++ {
		changed = false
		for _, b := range bindings {
			for _, value := range possibleStrings(b.value, names) {
				if !slices.Contains(names[b.name], value) {
					names[b.name] = append(names[b.name], value)
					changed = true
				}
			}
		}
	}
	return names
}

// possibleStrings returns the strings expr can evaluate to: a string literal, a
// name in names, a parenthesised expression, or a + of two of these. Any other
// expression, such as a call or a field read, gives none. A literal that does not
// unquote gives none too: the scan reports that literal when it visits it.
func possibleStrings(expr ast.Expr, names map[string][]string) []string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return nil
		}
		value, err := strconv.Unquote(e.Value)
		if err != nil {
			return nil
		}
		return []string{value}
	case *ast.ParenExpr:
		return possibleStrings(e.X, names)
	case *ast.Ident:
		return names[e.Name]
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return nil
		}
		var out []string
		for _, left := range possibleStrings(e.X, names) {
			for _, right := range possibleStrings(e.Y, names) {
				out = append(out, left+right)
			}
		}
		return out
	}
	return nil
}

// literalHoldsPath reports whether a string literal among the operands of a
// concatenation holds the path on its own.
func literalHoldsPath(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return anyMayorRigPath(possibleStrings(e, nil))
	case *ast.ParenExpr:
		return literalHoldsPath(e.X)
	case *ast.BinaryExpr:
		return literalHoldsPath(e.X) || literalHoldsPath(e.Y)
	}
	return false
}

// anyMayorRigPath reports whether one of the strings holds the path as whole segments.
func anyMayorRigPath(values []string) bool {
	for _, value := range values {
		if mayorRigPathPattern.MatchString(value) {
			return true
		}
	}
	return false
}

// unquoteLiteral returns the value of a string literal, or an error naming the
// file and line the literal is on.
func unquoteLiteral(fset *token.FileSet, rel string, lit *ast.BasicLit) (string, error) {
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", fmt.Errorf("%s:%d: %w", rel, fset.Position(lit.Pos()).Line, err)
	}
	return value, nil
}

// loadMayorRigAllowlist parses the allowlist. Each line is
// path<TAB>count<TAB>class<TAB>reason; blank lines and # comments are skipped.
func loadMayorRigAllowlist(path string) ([]mayorRigEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read mayor/rig allowlist %s: %w", path, err)
	}
	var entries []mayorRigEntry
	seen := map[string]int{}
	for i, raw := range strings.Split(string(data), "\n") {
		text := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(text) == "" || strings.HasPrefix(strings.TrimSpace(text), "#") {
			continue
		}
		where := fmt.Sprintf("%s:%d", path, i+1)
		fields := strings.Split(text, "\t")
		if len(fields) != 4 {
			return nil, fmt.Errorf("%s: want path<TAB>count<TAB>class<TAB>reason, got %q", where, text)
		}
		file := strings.TrimSpace(fields[0])
		countText := strings.TrimSpace(fields[1])
		class := strings.TrimSpace(fields[2])
		reason := strings.TrimSpace(fields[3])
		if file == "" {
			return nil, fmt.Errorf("%s: entry has no path", where)
		}
		if strings.ContainsAny(file, "*?[") {
			return nil, fmt.Errorf("%s: wildcard path %q; each entry names one file", where, file)
		}
		count, err := strconv.Atoi(countText)
		if err != nil || count < 1 {
			return nil, fmt.Errorf("%s: count %q for %s is not a positive integer; a file with no use has no line", where, countText, file)
		}
		if class != mayorRigAllowClass {
			return nil, fmt.Errorf("%s: class %q is not pending-retirement; that is the only class", where, class)
		}
		if reason == "" {
			return nil, fmt.Errorf("%s: entry for %s has no reason", where, file)
		}
		if first, dup := seen[file]; dup {
			return nil, fmt.Errorf("%s: duplicate entry for %s (first at line %d)", where, file, first)
		}
		seen[file] = i + 1
		entries = append(entries, mayorRigEntry{file: file, count: count, reason: reason, line: i + 1})
	}
	return entries, nil
}

// unlistedMayorRigHits reports the files the allowlist does not excuse: a file
// with no line, and a file that uses the path more often than its line allows.
// Each message names the file, its line and the allowlist in use.
func unlistedMayorRigHits(hits []mayorRigHit, entries []mayorRigEntry, allowPath string) error {
	allowed := map[string]int{}
	for _, entry := range entries {
		allowed[entry.file] = entry.count
	}
	var b strings.Builder
	for _, hit := range hits {
		limit, listed := allowed[hit.file]
		switch {
		case !listed:
			fmt.Fprintf(&b, "  %s:%d has %d mayor/rig use(s) and no line in %s\n", hit.file, hit.line, hit.count, allowPath)
		case hit.count > limit:
			fmt.Fprintf(&b, "  %s:%d has %d mayor/rig use(s); %s allows %d\n", hit.file, hit.line, hit.count, allowPath, limit)
		}
	}
	if b.Len() == 0 {
		return nil
	}
	return errors.New("non-test code uses <rig>/mayor/rig and " + allowPath + " does not allow that many. The retirement removes this path, so do not add a use; use the replacement instead:\n" + b.String())
}

// staleMayorRigEntries reports the allowlist entries that allow more than the tree
// holds: a file with no use loses its line, and a count above the file's uses is
// lowered to match.
func staleMayorRigEntries(entries []mayorRigEntry, hits []mayorRigHit, allowPath string) error {
	live := map[string]int{}
	for _, hit := range hits {
		live[hit.file] = hit.count
	}
	var b strings.Builder
	for _, entry := range entries {
		have := live[entry.file]
		switch {
		case have == 0:
			fmt.Fprintf(&b, "  %s:%d %s has no mayor/rig use; delete the line\n", allowPath, entry.line, entry.file)
		case have < entry.count:
			fmt.Fprintf(&b, "  %s:%d %s allows %d, the file has %d use(s); lower the count to %d\n", allowPath, entry.line, entry.file, entry.count, have, have)
		}
	}
	if b.Len() == 0 {
		return nil
	}
	return errors.New("allowlisted uses are stale in " + allowPath + ": delete a line whose file no longer has a use, and lower a count the file no longer reaches, so the list only shrinks:\n" + b.String())
}

// writeFixture writes one file under root, creating its directory, and returns
// its path.
func writeFixture(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// repoRoot returns the directory above this package that holds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
