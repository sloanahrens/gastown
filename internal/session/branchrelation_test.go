package session

import (
	"errors"
	"testing"
)

// A classifier over a fixed history: each pair names an ancestor and the
// descendant it is an ancestor of. Any other pair is not an ancestor.
func historyAncestry(pairs ...[2]string) func(string, string) (bool, error) {
	set := map[[2]string]bool{}
	for _, p := range pairs {
		set[p] = true
	}
	return func(a, d string) (bool, error) {
		return set[[2]string{a, d}], nil
	}
}

// classifyBranchRelation reads two heads against an ancestry function and
// names all four relations: equal, local ahead, local behind and diverged.
func TestClassifyBranchRelation(t *testing.T) {
	t.Parallel()
	// base is an ancestor of both heads; local and remote each add a commit.
	cases := []struct {
		name       string
		local      string
		remote     string
		ancestry   func(string, string) (bool, error)
		wantRelate branchRelation
	}{
		{
			name:       "equal",
			local:      "tip",
			remote:     "tip",
			ancestry:   historyAncestry(),
			wantRelate: relationEqual,
		},
		{
			name:       "local ahead holds every remote commit and more",
			local:      "local",
			remote:     "base",
			ancestry:   historyAncestry([2]string{"base", "local"}),
			wantRelate: relationAhead,
		},
		{
			name:       "local behind is an ancestor of the remote",
			local:      "base",
			remote:     "remote",
			ancestry:   historyAncestry([2]string{"base", "remote"}),
			wantRelate: relationBehind,
		},
		{
			name:       "diverged: each side holds a commit the other lacks",
			local:      "local",
			remote:     "remote",
			ancestry:   historyAncestry(),
			wantRelate: relationDiverged,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := classifyBranchRelation(tc.local, tc.remote, tc.ancestry)
			if err != nil {
				t.Fatalf("classifyBranchRelation: %v", err)
			}
			if got != tc.wantRelate {
				t.Errorf("relation = %v, want %v", got, tc.wantRelate)
			}
		})
	}
}

// An ancestry query that fails is returned, never read as a relation: a
// branch that cannot be compared is not classed as behind or diverged.
func TestBranchRelationReturnsAnAncestryError(t *testing.T) {
	t.Parallel()
	boom := errors.New("git merge-base failed")
	failing := func(string, string) (bool, error) { return false, boom }

	if _, err := classifyBranchRelation("local", "remote", failing); !errors.Is(err, boom) {
		t.Fatalf("classifyBranchRelation with a failing ancestry = %v, want the ancestry error", err)
	}
}

// The diverged refusal names the branch, both heads, the merge-base and the
// commit counts, and carries git's refusal on one line.
func TestBranchRelationDivergedRefusalNamesTheDivergenceOnOneLine(t *testing.T) {
	t.Parallel()
	refusal := errors.New("To http://forgejo/x.git\n ! [rejected]  b -> b (non-fast-forward)\nerror: failed to push some refs")
	err := divergedRefusal(divergence{
		Branch: "polecat/x/gt-1+abc", Local: "aaa", Remote: "bbb", MergeBase: "mmm",
		LocalOnly: 2, RemoteOnly: 1,
	}, refusal)

	want := "branch polecat/x/gt-1+abc diverged from origin: local aaa, origin bbb, merge-base mmm; " +
		"local holds 2 commits origin lacks, origin holds 1 local lacks; nothing is forced: " +
		"To http://forgejo/x.git | ! [rejected]  b -> b (non-fast-forward) | error: failed to push some refs"
	if err.Error() != want {
		t.Errorf("diverged refusal =\n%q\nwant\n%q", err.Error(), want)
	}
}
