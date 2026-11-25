package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

// registryDerive is a derivation that answers derived for any commit and
// records, in order, the commits it was asked about.
func registryDerive(derived imagevalues.Values, seen *[]string) deriveFunc {
	return func(_ context.Context, sha string) (imagevalues.Values, error) {
		*seen = append(*seen, sha)
		return derived, nil
	}
}

// TestPairMatchesRegistry holds the install's re-derivation: the pair the
// registry answers for the file's commit is compared with the file, so an equal
// pair passes and a differing digest is refused naming the field and both
// digests. The derivation is asked about the file's commit and nothing else.
func TestPairMatchesRegistry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// edit changes the file's pair (f) or the derived pair (d); nil leaves
		// both as derived.
		edit func(f, d *imagevalues.Values)
		// want is empty when the pair matches; otherwise every string must
		// appear in the refusal.
		want []string
	}{
		{name: "an equal derived pair passes"},
		{name: "a file naming no bd image passes against a derived pair that has one", edit: func(f, _ *imagevalues.Values) { f.Bd = "" }},
		{name: "a differing daemon digest is refused naming the field", edit: func(f, _ *imagevalues.Values) {
			f.Daemon = pairMatchRef(imagevalues.DaemonImage, "111111111111")
		}, want: []string{"daemon", "sha256:111111111111", "sha256:dddddddddddd"}},
		{name: "a differing polecat digest is refused naming the field", edit: func(f, _ *imagevalues.Values) {
			f.Polecat = pairMatchRef(imagevalues.PolecatImage, "222222222222")
		}, want: []string{"polecat", "sha256:222222222222", "sha256:eeeeeeeeeeee"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, d := pairMatchPair(), pairMatchPair()
			if tc.edit != nil {
				tc.edit(&f, &d)
			}
			var seen []string
			err := pairMatchesRegistry(context.Background(), registryDerive(d, &seen), f)
			if len(seen) != 1 || seen[0] != f.Commit {
				t.Errorf("the derivation was asked about commits %q, want exactly [%q]", seen, f.Commit)
			}
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("pairMatchesRegistry refused a file equal to the derived pair: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("pairMatchesRegistry accepted a file the derivation does not match; want a refusal naming %q", tc.want)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal %q does not contain %q", err, w)
				}
			}
		})
	}
}

// TestPairMatchesRegistryNilDerive holds an install with no registry derivation
// wired to a refusal before any comparison: the error names the missing
// derivation and the file's commit, for a file that would match a derivation and
// for one a comparison would refuse, so an unwired install passes no file and
// compares none.
func TestPairMatchesRegistryNilDerive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// edit changes the file's pair; nil leaves it equal to the derived pair.
		edit func(f *imagevalues.Values)
	}{
		{name: "a file that would match a derivation"},
		{name: "a file a derivation would refuse", edit: func(f *imagevalues.Values) { f.Daemon = "not-a-digest" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := pairMatchPair()
			if tc.edit != nil {
				tc.edit(&f)
			}
			err := pairMatchesRegistry(context.Background(), nil, f)
			if err == nil {
				t.Fatal("pairMatchesRegistry passed a file with no registry derivation wired")
			}
			for _, w := range []string{"no registry derivation wired", f.Commit} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal %q does not contain %q", err, w)
				}
			}
			for _, w := range []string{"is not a digest reference", "digest sha256:"} {
				if strings.Contains(err.Error(), w) {
					t.Errorf("refusal %q compares the file, want the missing derivation refused before any comparison", err)
				}
			}
		})
	}
}

// TestPairMatchesRegistryDeriveFails holds a failed derivation to its refusal:
// the install refuses the file, wrapping the cause so the caller can match it,
// and never passes on it. A pair answered beside an error still refuses: an
// answer the derivation also reported as failed is not a pair.
func TestPairMatchesRegistryDeriveFails(t *testing.T) {
	t.Parallel()
	cause := errors.New("the registry holds no pair for this commit")
	tests := []struct {
		name string
		// derived is what the derivation answers beside the error.
		derived imagevalues.Values
		// want is every string the refusal must carry.
		want []string
	}{
		{name: "a failing derivation is refused and wraps its cause", want: []string{"cannot be read", pairMatchCommit}},
		{name: "a pair answered beside an error is refused", derived: pairMatchPair(), want: []string{"cannot be read", pairMatchCommit}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := pairMatchPair()
			var seen []string
			derive := func(_ context.Context, sha string) (imagevalues.Values, error) {
				seen = append(seen, sha)
				return tc.derived, cause
			}
			err := pairMatchesRegistry(context.Background(), derive, f)
			if err == nil {
				t.Fatal("pairMatchesRegistry passed a file whose derivation failed")
			}
			if !errors.Is(err, cause) {
				t.Errorf("refusal %q does not wrap the derivation's cause %q", err, cause)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal %q does not contain %q", err, w)
				}
			}
			if len(seen) != 1 || seen[0] != f.Commit {
				t.Errorf("the derivation was asked about commits %q, want exactly [%q]", seen, f.Commit)
			}
		})
	}
}
