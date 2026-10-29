package land

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestOMThresholdAtReadsTheBaseCommit: the bar is the .om.json at base. An
// absent file or key is no bar, and a read or parse failure is an error, never
// a bar of 0 (gt-uhuw9).
func TestOMThresholdAtReadsTheBaseCommit(t *testing.T) {
	t.Parallel()
	readErr := errors.New("object unreadable")
	tests := []struct {
		name    string
		repo    Repo
		want    float64
		wantErr bool
	}{
		{name: "the base threshold", repo: baseRepo{omJSON: `{"threshold":0.75}`}, want: 0.75},
		{name: "no .om.json at base", repo: baseRepo{}, want: 0},
		{name: "a base .om.json without the key", repo: baseRepo{omJSON: `{"rubric":"x"}`}, want: 0},
		{name: "an unparseable base .om.json", repo: baseRepo{omJSON: `{"threshold":`}, wantErr: true},
		{name: "a base read that fails", repo: baseRepo{err: readErr}, wantErr: true},
		{name: "no repository", repo: nil, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := OMThresholdAt(tc.repo, "base")
			if (err != nil) != tc.wantErr {
				t.Fatalf("OMThresholdAt error = %v, want error %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("OMThresholdAt = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReviewThresholdBaseOverridesAnEditedTree: a diff that sets the tree's
// .om.json threshold to 0 is still judged against the base's 0.75. om answers
// approve at 0.30, and the landing's verdict is a request for changes.
func TestReviewThresholdBaseOverridesAnEditedTree(t *testing.T) {
	t.Parallel()
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, omConfigFile), []byte(`{"threshold":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var argv []string
	r := OMReviewer{OutDir: t.TempDir()}
	r.run = fakeOM(t, `{"score":0.30,"verdict":"approve","findings":[]}`, 0, nil, &argv)
	v, err := r.ReviewBead(context.Background(), ReviewInput{
		Dir: tree, Base: "aaa", Head: "bbb", Repo: baseRepo{omJSON: `{"threshold":0.75}`}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != VerdictRequestChanges || !strings.Contains(v.Summary, "0.75") {
		t.Fatalf("verdict %+v; the base bar of 0.75 refuses a 0.30 approve", v)
	}
}

// TestReviewThresholdBaseIsGivenToOM: om is run with the base bar on its argv,
// so its own verdict uses the same bar the landing checks. A base with no bar
// adds no flag.
func TestReviewThresholdBaseIsGivenToOM(t *testing.T) {
	t.Parallel()
	var argv []string
	r := OMReviewer{OutDir: t.TempDir()}
	r.run = fakeOM(t, `{"score":0.90,"verdict":"approve","findings":[]}`, 0, nil, &argv)
	if _, err := r.ReviewBead(context.Background(), ReviewInput{
		Dir: t.TempDir(), Base: "aaa", Head: "bbb", Repo: baseRepo{omJSON: `{"threshold":0.75}`}}); err != nil {
		t.Fatal(err)
	}
	if got, ok := argvFlagValue(argv, "--threshold"); !ok || got != "0.75" {
		t.Fatalf("--threshold = %q (present %v), want the base bar 0.75: %v", got, ok, argv)
	}

	argv = nil
	if _, err := reviewAt(context.Background(), r, "", t.TempDir(), "aaa", "bbb"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(argv, "--threshold") {
		t.Fatalf("argv carries --threshold with no base bar: %v", argv)
	}
}

// TestReviewThresholdBaseFailsClosed: a base bar that cannot be read fails the
// review before om runs, and Review, which has no repository, never runs om.
func TestReviewThresholdBaseFailsClosed(t *testing.T) {
	t.Parallel()
	calls := 0
	r := OMReviewer{OutDir: t.TempDir()}
	r.run = func(_ context.Context, _ string, _ []string, argv []string, _ io.Writer) (int, error) {
		if omSubcommand(argv) != "version" {
			calls++
		}
		return 0, nil
	}
	if _, err := r.ReviewBead(context.Background(), ReviewInput{
		Dir: t.TempDir(), Base: "aaa", Head: "bbb", Repo: baseRepo{err: errors.New("no such ref")}}); err == nil {
		t.Fatal("ReviewBead with an unreadable base bar returned no error")
	}
	if _, err := r.Review(context.Background(), t.TempDir(), "aaa", "bbb"); err == nil {
		t.Fatal("Review, which has no repository to read the base bar from, returned no error")
	}
	if calls != 0 {
		t.Fatalf("om ran %d times with no base bar, want 0", calls)
	}
}
