package land

import (
	"reflect"
	"testing"
)

// cands is findings whose path is also their title: the grouping reads only
// the path.
func cands(paths ...string) []FollowupCandidate {
	out := make([]FollowupCandidate, 0, len(paths))
	for _, p := range paths {
		out = append(out, FollowupCandidate{Path: p, Title: p})
	}
	return out
}

// grp is the group a package's findings must come back in.
func grp(pkg string, paths ...string) followupGroup {
	return followupGroup{Package: pkg, Findings: cands(paths...)}
}

// TestFollowupGroupsAlwaysGroupsByPackage: every group names the package its
// findings land in, whether or not the landing needed a split. One package is
// one group with that Package, several are one group each in sorted order, and
// a finding with no package — no path, or a file at the repository root — is
// the group with Package "" (gt-mew3vm).
func TestFollowupGroupsAlwaysGroupsByPackage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		findings []FollowupCandidate
		want     []followupGroup
	}{
		{"one package returns one group named for it",
			cands("internal/land/a.go", "internal/land/b.go", "internal/land/c.go"),
			[]followupGroup{grp("internal/land", "internal/land/a.go", "internal/land/b.go", "internal/land/c.go")}},
		{"one finding in one package is still named for it",
			cands("internal/land/a.go"),
			[]followupGroup{grp("internal/land", "internal/land/a.go")}},
		{"two packages come back sorted",
			cands("internal/land/a.go", "docs/reference.md"),
			[]followupGroup{grp("docs", "docs/reference.md"), grp("internal/land", "internal/land/a.go")}},
		{"a root-level finding is the empty package, sorted first",
			cands("internal/land/a.go", "README.md"),
			[]followupGroup{grp("", "README.md"), grp("internal/land", "internal/land/a.go")}},
		{"a root-level finding alone is the empty package",
			cands("README.md"),
			[]followupGroup{grp("", "README.md")}},
		{"no findings is no groups", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := followupGroups(tc.findings); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("followupGroups(%v) = %+v, want %+v", tc.findings, got, tc.want)
			}
		})
	}
}
