package daemonimage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/steveyegge/gastown/internal/templates"
)

// fakeFetcher stands in for the network: it records each URL it is asked for and
// serves body, or fails with err.
type fakeFetcher struct {
	body io.Reader
	err  error
	urls []string
}

func (f *fakeFetcher) fetch(_ context.Context, url string) (io.ReadCloser, error) {
	f.urls = append(f.urls, url)
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(f.body), nil
}

// claudeBytes is the stand-in binary a fake fetch serves, and claudeSum its sha256.
var claudeBytes = []byte("claude binary")

func claudeSum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// claudeBuilder is a Builder whose build context is a fresh directory and whose
// fetcher is f.
func claudeBuilder(t *testing.T, f *fakeFetcher) Builder {
	t.Helper()
	return Builder{Out: t.TempDir(), Fetch: f.fetch}
}

// dirNames is the names of the entries in dir, so a test can see whether a
// refused fetch left anything behind.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestClaudeCLIPinIsExact: the constants the image is built with are an exact
// version and a sha256 in the printed form, so the pin names one binary.
func TestClaudeCLIPinIsExact(t *testing.T) {
	t.Parallel()
	if err := checkClaudePin(templates.ClaudeCLIVersion, templates.ClaudeCLISHA256); err != nil {
		t.Fatalf("the claude CLI pin in templates is refused: %v", err)
	}
}

// TestCheckClaudePinRefusesAFloatingOrMalformedPin: a version that is empty,
// floating or not an exact dotted version, or a sha256 that is not 64 lower-case
// hex digits, is refused before anything is fetched.
func TestCheckClaudePinRefusesAFloatingOrMalformedPin(t *testing.T) {
	t.Parallel()
	goodSHA := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, version, sha string
	}{
		{name: "an empty version", version: "", sha: goodSHA},
		{name: "the latest tag", version: "latest", sha: goodSHA},
		{name: "the stable tag", version: "stable", sha: goodSHA},
		{name: "a v-prefixed version", version: "v2.1.314", sha: goodSHA},
		{name: "a version with a suffix", version: "2.1.314-beta", sha: goodSHA},
		{name: "a bare major version", version: "2", sha: goodSHA},
		{name: "a version with a space", version: "2.1.314 ", sha: goodSHA},
		{name: "an empty sha256", version: "2.1.314", sha: ""},
		{name: "a short sha256", version: "2.1.314", sha: strings.Repeat("a", 63)},
		{name: "a long sha256", version: "2.1.314", sha: strings.Repeat("a", 65)},
		{name: "an upper-case sha256", version: "2.1.314", sha: strings.Repeat("A", 64)},
		{name: "a non-hex sha256", version: "2.1.314", sha: strings.Repeat("g", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := checkClaudePin(tc.version, tc.sha); err == nil {
				t.Errorf("checkClaudePin(%q, %q) accepted the pin, want a refusal", tc.version, tc.sha)
			}
		})
	}
}

// TestFetchClaudeCLIFailsOnAMismatchAndLeavesNothing: a download whose sha256 is
// not the pin's returns an error and leaves no file in the build context.
func TestFetchClaudeCLIFailsOnAMismatchAndLeavesNothing(t *testing.T) {
	t.Parallel()
	f := &fakeFetcher{body: bytes.NewReader([]byte("tampered"))}
	b := claudeBuilder(t, f)
	path, err := b.fetchClaudeCLI(context.Background(), "https://example.test/claude", "2.1.314", claudeSum(claudeBytes))
	if err == nil {
		t.Fatalf("a mismatched download returned %q and no error", path)
	}
	if path != "" {
		t.Errorf("a mismatched download returned the path %q, want none", path)
	}
	if got := dirNames(t, b.Out); len(got) != 0 {
		t.Errorf("a mismatched download left %q in the build context, want nothing", got)
	}
}

// TestFetchClaudeCLIReturnsTheVerifiedBinary: a download whose sha256 matches the
// pin is written as a file named claude in the build context, and its path is
// returned.
func TestFetchClaudeCLIReturnsTheVerifiedBinary(t *testing.T) {
	t.Parallel()
	f := &fakeFetcher{body: bytes.NewReader(claudeBytes)}
	b := claudeBuilder(t, f)
	path, err := b.fetchClaudeCLI(context.Background(), "https://example.test/claude", "2.1.314", claudeSum(claudeBytes))
	if err != nil {
		t.Fatalf("fetchClaudeCLI: %v", err)
	}
	if want := filepath.Join(b.Out, "claude"); path != want {
		t.Errorf("fetchClaudeCLI returned %q, want %q", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the returned binary: %v", err)
	}
	if !bytes.Equal(got, claudeBytes) {
		t.Errorf("the binary holds %q, want the bytes the fetch served", got)
	}
	if got := dirNames(t, b.Out); !slices.Equal(got, []string{"claude"}) {
		t.Errorf("the build context holds %q, want only the claude binary", got)
	}
}

// TestFetchClaudeCLIFailedDownloadLeavesNothing: a fetch that errors returns the
// error and writes nothing.
func TestFetchClaudeCLIFailedDownloadLeavesNothing(t *testing.T) {
	t.Parallel()
	f := &fakeFetcher{err: errors.New("connection refused")}
	b := claudeBuilder(t, f)
	if _, err := b.fetchClaudeCLI(context.Background(), "https://example.test/claude", "2.1.314", claudeSum(claudeBytes)); err == nil {
		t.Fatal("a failed download returned no error")
	}
	if got := dirNames(t, b.Out); len(got) != 0 {
		t.Errorf("a failed download left %q in the build context, want nothing", got)
	}
}

// TestFetchClaudeCLIInterruptedReadLeavesNothing: a body that fails part way
// through returns the error and removes the partial download, so a truncated
// binary never takes the claude name.
func TestFetchClaudeCLIInterruptedReadLeavesNothing(t *testing.T) {
	t.Parallel()
	body := io.MultiReader(bytes.NewReader(claudeBytes[:4]), iotest.ErrReader(errors.New("connection reset")))
	f := &fakeFetcher{body: body}
	b := claudeBuilder(t, f)
	if _, err := b.fetchClaudeCLI(context.Background(), "https://example.test/claude", "2.1.314", claudeSum(claudeBytes)); err == nil {
		t.Fatal("an interrupted download returned no error")
	}
	if got := dirNames(t, b.Out); len(got) != 0 {
		t.Errorf("an interrupted download left %q in the build context, want nothing", got)
	}
}

// TestFetchClaudeCLIFailsWhenTheTempFileCannotBeMade: a build context that does
// not exist makes the temporary file fail, and the error names the staging step.
func TestFetchClaudeCLIFailsWhenTheTempFileCannotBeMade(t *testing.T) {
	t.Parallel()
	f := &fakeFetcher{body: bytes.NewReader(claudeBytes)}
	b := Builder{Out: filepath.Join(t.TempDir(), "missing"), Fetch: f.fetch}
	path, err := b.fetchClaudeCLI(context.Background(), "https://example.test/claude", "2.1.314", claudeSum(claudeBytes))
	if err == nil || !strings.Contains(err.Error(), "cannot stage the claude CLI") {
		t.Fatalf("fetchClaudeCLI = %q, %v; want an error naming the staging step", path, err)
	}
}

// closeFailsFile is a temporary file that reports its Close as failed after it
// has closed the file, so the test sees the failure with nothing left behind.
type closeFailsFile struct{ *os.File }

func (f closeFailsFile) Close() error {
	_ = f.File.Close()
	return errors.New("disk full")
}

// TestFetchClaudeCLIFailsWhenTheTempFileDoesNotClose: a temporary file whose
// Close fails returns the error, and the partial download is removed.
func TestFetchClaudeCLIFailsWhenTheTempFileDoesNotClose(t *testing.T) {
	t.Parallel()
	f := &fakeFetcher{body: bytes.NewReader(claudeBytes)}
	b := claudeBuilder(t, f)
	create := func(dir, pattern string) (claudeTemp, error) {
		tmp, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		return closeFailsFile{tmp}, nil
	}
	path, err := b.fetchClaudeCLIWith(context.Background(), "https://example.test/claude", "2.1.314", claudeSum(claudeBytes), create)
	if err == nil || !strings.Contains(err.Error(), "cannot write the claude CLI") {
		t.Fatalf("fetchClaudeCLIWith = %q, %v; want an error naming the write", path, err)
	}
	if got := dirNames(t, b.Out); len(got) != 0 {
		t.Errorf("a failed close left %q in the build context, want nothing", got)
	}
}

// TestFetchClaudeCLIFailsWhenTheNameIsTaken: a directory already named claude in
// the build context makes the move fail, and the verified download is removed
// rather than left under its temporary name.
func TestFetchClaudeCLIFailsWhenTheNameIsTaken(t *testing.T) {
	t.Parallel()
	f := &fakeFetcher{body: bytes.NewReader(claudeBytes)}
	b := claudeBuilder(t, f)
	if err := os.Mkdir(filepath.Join(b.Out, "claude"), 0o755); err != nil {
		t.Fatalf("making the claude directory: %v", err)
	}
	path, err := b.fetchClaudeCLI(context.Background(), "https://example.test/claude", "2.1.314", claudeSum(claudeBytes))
	if err == nil || !strings.Contains(err.Error(), "cannot move the claude CLI") {
		t.Fatalf("fetchClaudeCLI = %q, %v; want an error naming the move", path, err)
	}
	if got := dirNames(t, b.Out); !slices.Equal(got, []string{"claude"}) {
		t.Errorf("a refused move left %q in the build context, want only the directory it could not replace", got)
	}
}

// TestFetchClaudeCLIRefusesBeforeFetching: every refusal comes before the fetch,
// so a refused call asks the network for nothing.
func TestFetchClaudeCLIRefusesBeforeFetching(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		breakIt func(*Builder)
		version string
		sha     string
		want    string
	}{
		{name: "no fetcher", breakIt: func(b *Builder) { b.Fetch = nil }, version: "2.1.314", sha: claudeSum(claudeBytes), want: "no fetcher"},
		{name: "no build context", breakIt: func(b *Builder) { b.Out = "" }, version: "2.1.314", sha: claudeSum(claudeBytes), want: "no build context"},
		{name: "a relative build context", breakIt: func(b *Builder) { b.Out = filepath.Join("rel", "out") }, version: "2.1.314", sha: claudeSum(claudeBytes), want: "is not absolute"},
		{name: "a floating version", breakIt: func(*Builder) {}, version: "latest", sha: claudeSum(claudeBytes), want: "not an exact dotted version"},
		{name: "a malformed sha256", breakIt: func(*Builder) {}, version: "2.1.314", sha: "abc", want: "not 64 lower-case hex digits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeFetcher{body: bytes.NewReader(claudeBytes)}
			b := claudeBuilder(t, f)
			tc.breakIt(&b)
			path, err := b.fetchClaudeCLI(context.Background(), "https://example.test/claude", tc.version, tc.sha)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("fetchClaudeCLI = %q, %v; want an error naming %q", path, err, tc.want)
			}
			if len(f.urls) != 0 {
				t.Errorf("a refused fetch asked for %q, want no request", f.urls)
			}
		})
	}
}

// TestFetchClaudeCLIAsksForThePinnedURL: the production call asks the network for
// the artifact URL the pinned version names, and a binary that is not the pinned
// one is refused. The real artifact is never fetched in a test, so the bytes here
// cannot match the pin and nothing is written.
func TestFetchClaudeCLIAsksForThePinnedURL(t *testing.T) {
	t.Parallel()
	f := &fakeFetcher{body: bytes.NewReader(claudeBytes)}
	b := claudeBuilder(t, f)
	if _, err := b.FetchClaudeCLI(context.Background()); err == nil {
		t.Fatal("FetchClaudeCLI accepted bytes that are not the pinned binary")
	}
	want := "https://downloads.claude.ai/claude-code-releases/" + templates.ClaudeCLIVersion + "/linux-arm64/claude"
	if len(f.urls) != 1 || f.urls[0] != want {
		t.Errorf("FetchClaudeCLI asked for %q, want %q", f.urls, want)
	}
	if got := dirNames(t, b.Out); len(got) != 0 {
		t.Errorf("a mismatched FetchClaudeCLI left %q in the build context, want nothing", got)
	}
}
