package metrics

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

const numstatFixture = "10\t2\tcmd/a.go\n3\t0\tcmd/b.go\n0\t5\tinternal/x/{old => new}/c.go\n-\t-\tlogo.png\n1\t1\told.txt => new.txt\n"

func TestNumstatParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want []NumstatEntry
	}{
		{"binary files and renames", numstatFixture, []NumstatEntry{
			{10, 2, "cmd/a.go"}, {3, 0, "cmd/b.go"}, {0, 5, "internal/x/new/c.go"},
			{0, 0, "logo.png"}, {1, 1, "new.txt"}}},
		{"empty", "", nil},
		{"brace rename with an empty old side", "1\t1\tsrc/{ => sub}/f.go\n", []NumstatEntry{{1, 1, "src/sub/f.go"}}},
		{"brace rename with an empty new side", "1\t1\tsrc/{sub => }/f.go\n", []NumstatEntry{{1, 1, "src/f.go"}}},
		{"a line that is not three fields is skipped", "garbage\n2\t3\n4\t5\tp/a.go\r\n", []NumstatEntry{{4, 5, "p/a.go"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseNumstat(tc.in)
			if err != nil {
				t.Fatalf("ParseNumstat: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseNumstat(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestNumstatParseRejectsACountThatIsNotAnInteger(t *testing.T) {
	t.Parallel()
	if got, err := ParseNumstat("x\t1\tp/a.go\n"); err == nil {
		t.Errorf("ParseNumstat = %v, want an error for a count of %q", got, "x")
	}
}

func TestDiffstatSingleFileHasZeroEntropy(t *testing.T) {
	t.Parallel()
	s := DiffStats([]NumstatEntry{{4, 1, "a/b.go"}})
	if s.LinesAdded != 4 || s.LinesDeleted != 1 || s.FilesTouched != 1 {
		t.Errorf("counts = %d/%d/%d, want 4/1/1", s.LinesAdded, s.LinesDeleted, s.FilesTouched)
	}
	if s.FileEntropy != 0 || s.PackageEntropy != 0 {
		t.Errorf("entropies = %v/%v, want 0/0", s.FileEntropy, s.PackageEntropy)
	}
}

func TestDiffstatTwoEqualFilesOnePackage(t *testing.T) {
	t.Parallel()
	s := DiffStats([]NumstatEntry{{2, 0, "p/a.go"}, {2, 0, "p/b.go"}})
	if math.Abs(s.FileEntropy-1) > floatTol {
		t.Errorf("FileEntropy = %v, want 1", s.FileEntropy)
	}
	if s.PackageEntropy != 0 {
		t.Errorf("PackageEntropy = %v, want 0", s.PackageEntropy)
	}
}

func TestDiffstatTwoPackages(t *testing.T) {
	t.Parallel()
	s := DiffStats([]NumstatEntry{{3, 1, "p/a.go"}, {2, 2, "q/b.go"}})
	if math.Abs(s.PackageEntropy-1) > floatTol {
		t.Errorf("PackageEntropy = %v, want 1", s.PackageEntropy)
	}
}

func TestDiffstatRootFilesShareThePackageDot(t *testing.T) {
	t.Parallel()
	s := DiffStats([]NumstatEntry{{0, 0, "a.txt"}, {0, 0, "b.txt"}})
	if s.FilesTouched != 2 || s.PackageEntropy != 0 {
		t.Errorf("files = %d, package entropy = %v; want 2 files in one package", s.FilesTouched, s.PackageEntropy)
	}
	if math.Abs(s.FileEntropy-1) > floatTol {
		t.Errorf("FileEntropy = %v, want 1: a file with no lines still weighs one", s.FileEntropy)
	}
}

// gitStub answers every git call with out and err, recording the argv.
func gitStub(out string, err error) (Runner, *[][]string) {
	calls := &[][]string{}
	run := func(_ context.Context, argv []string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), argv...))
		return []byte(out), err
	}
	return run, calls
}

func bareRepo(rig string) string { return "/bare/" + rig }

func TestGitSourceStatsForACommit(t *testing.T) {
	t.Parallel()
	run, calls := gitStub("1\t0\tp/a.txt\n", nil)
	src := &GitDiffSource{BareRepoFor: bareRepo, Run: run, Cache: DiffCache{}}
	s, err := src.Stats(context.Background(), "r", "0123abc")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.LinesAdded != 1 || s.LinesDeleted != 0 || s.FilesTouched != 1 {
		t.Errorf("counts = %d/%d/%d, want 1/0/1", s.LinesAdded, s.LinesDeleted, s.FilesTouched)
	}
	want := []string{"git", "--git-dir", "/bare/r", "show", "--numstat", "-M", "--format=", "0123abc", "--"}
	if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], want) {
		t.Errorf("git calls = %v, want one call %v", *calls, want)
	}
}

func TestGitSourceUnreadableCommitIsUnavailableAndNotCached(t *testing.T) {
	t.Parallel()
	run, _ := gitStub("", errors.New("git: bad object"))
	cache := DiffCache{}
	src := &GitDiffSource{BareRepoFor: bareRepo, Run: run, Cache: cache}
	_, err := src.Stats(context.Background(), "r", "deadbeef")
	if !errors.Is(err, ErrDiffUnavailable) || !strings.Contains(err.Error(), "bad object") {
		t.Errorf("Stats err = %v, want ErrDiffUnavailable carrying git's error", err)
	}
	if len(cache) != 0 {
		t.Errorf("cache = %v, want empty", cache)
	}
}

func TestGitSourceSuccessIsCachedAndReadOnce(t *testing.T) {
	t.Parallel()
	run, calls := gitStub("1\t0\tp/a.txt\n", nil)
	cache := DiffCache{}
	src := &GitDiffSource{BareRepoFor: bareRepo, Run: run, Cache: cache}
	for range 2 {
		if _, err := src.Stats(context.Background(), "r", "0123abc"); err != nil {
			t.Fatalf("Stats: %v", err)
		}
	}
	if _, ok := cache[DiffKey{Rig: "r", Commit: "0123abc"}]; !ok {
		t.Errorf("cache = %v, want the commit cached", cache)
	}
	if len(*calls) != 1 {
		t.Errorf("git ran %d times, want 1: the second read is the cache's", len(*calls))
	}
}

func TestGitSourceOutputItCannotParseIsAnErrorNotUnavailable(t *testing.T) {
	t.Parallel()
	run, _ := gitStub("x\t1\tp/a.go\n", nil)
	src := &GitDiffSource{BareRepoFor: bareRepo, Run: run, Cache: DiffCache{}}
	_, err := src.Stats(context.Background(), "r", "0123abc")
	if err == nil || errors.Is(err, ErrDiffUnavailable) {
		t.Errorf("Stats err = %v, want a parse error that is not ErrDiffUnavailable", err)
	}
}

func TestGitSourceCommitValueIsValidated(t *testing.T) {
	t.Parallel()
	// The Python's None is the empty string here: Go has no nil string.
	for _, bad := range []string{"", "--output=/x", "HEAD~3", "main..other", "abc", strings.Repeat("g", 40), strings.Repeat("a", 65)} {
		run, calls := gitStub("", nil)
		src := &GitDiffSource{BareRepoFor: bareRepo, Run: run, Cache: DiffCache{}}
		if s, err := src.Stats(context.Background(), "gastown", bad); !errors.Is(err, ErrDiffUnavailable) {
			t.Errorf("Stats(%q) = %v, %v; want ErrDiffUnavailable", bad, s, err)
		}
		if len(*calls) != 0 {
			t.Errorf("Stats(%q) reached git with %v; a ledger value that is not a hash must not", bad, *calls)
		}
	}
}

func TestGitSourceHashIsPassedBeforeAPathSeparator(t *testing.T) {
	t.Parallel()
	run, calls := gitStub("", nil)
	src := &GitDiffSource{BareRepoFor: bareRepo, Run: run, Cache: DiffCache{}}
	if _, err := src.Stats(context.Background(), "gastown", "0123abc"); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	argv := (*calls)[0]
	if got := argv[len(argv)-2:]; !reflect.DeepEqual(got, []string{"0123abc", "--"}) {
		t.Errorf("argv ends %v, want [0123abc --]", got)
	}
}

func TestGitSourceHashWithATrailingNewlineIsAccepted(t *testing.T) {
	t.Parallel()
	run, calls := gitStub("", nil)
	src := &GitDiffSource{BareRepoFor: bareRepo, Run: run}
	if _, err := src.Stats(context.Background(), "gastown", "0123abc\n"); err != nil {
		t.Errorf("Stats: %v; the Python's $ admits one trailing newline", err)
	}
	if len(*calls) != 1 {
		t.Errorf("git ran %d times, want 1", len(*calls))
	}
}
