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

// grp is the group a landing's findings must come back in: the cut key they
// share, the package the bead is filed for, and the findings in input order.
func grp(key, pkg string, paths ...string) followupGroup {
	return followupGroup{Key: key, Package: pkg, Findings: cands(paths...)}
}

// TestFollowupGroupsCutBySourceFile: a group is one source file's work. A test
// file joins the group of the source it tests, two files of one package are
// two groups, and groups come back in sorted key order. Package stays the
// directory, and a finding with no path or a root-level file has the empty
// package (gt-zfwsgg).
func TestFollowupGroupsCutBySourceFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		findings []FollowupCandidate
		want     []followupGroup
	}{
		{"a source file and its test file are one group keyed by the source",
			cands("internal/land/foo.go", "internal/land/foo_test.go"),
			[]followupGroup{grp("internal/land/foo.go", "internal/land", "internal/land/foo.go", "internal/land/foo_test.go")}},
		{"two files of one package are two groups in sorted key order",
			cands("internal/land/foo.go", "internal/land/bar.go"),
			[]followupGroup{
				grp("internal/land/bar.go", "internal/land", "internal/land/bar.go"),
				grp("internal/land/foo.go", "internal/land", "internal/land/foo.go"),
			}},
		{"a test file joins its source's group across the landing",
			cands("internal/land/foo_test.go", "internal/land/bar.go", "internal/land/foo.go"),
			[]followupGroup{
				grp("internal/land/bar.go", "internal/land", "internal/land/bar.go"),
				grp("internal/land/foo.go", "internal/land", "internal/land/foo_test.go", "internal/land/foo.go"),
			}},
		{"two packages are two groups, sorted by key",
			cands("internal/land/a.go", "docs/reference.md"),
			[]followupGroup{
				grp("docs/reference.md", "docs", "docs/reference.md"),
				grp("internal/land/a.go", "internal/land", "internal/land/a.go"),
			}},
		{"a root-level file is keyed by its own name and has the empty package, sorted first",
			cands("internal/land/a.go", "README.md"),
			[]followupGroup{
				grp("README.md", "", "README.md"),
				grp("internal/land/a.go", "internal/land", "internal/land/a.go"),
			}},
		{"a root test file joins its root source",
			cands("spec_test.go", "spec.go"),
			[]followupGroup{grp("spec.go", "", "spec_test.go", "spec.go")}},
		{"a finding with no path is the empty key and the empty package",
			cands("", "README.md"),
			[]followupGroup{
				grp("", "", ""),
				grp("README.md", "", "README.md"),
			}},
		{"an unclean path groups with its cleaned source file",
			cands("./internal//land/a.go", "internal/land/a.go"),
			[]followupGroup{grp("internal/land/a.go", "internal/land", "./internal//land/a.go", "internal/land/a.go")}},
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
