package presubmit

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// planReader reads head; err short-circuits every path, and a path head lacks is an error, so a row fails on a read it expected to skip.
func planReader(head map[string]string, err error) FileReader {
	return func(p string) ([]byte, error) {
		if err != nil {
			return nil, err
		}
		src, ok := head[p]
		if !ok {
			return nil, fmt.Errorf("no %s", p)
		}
		return []byte(src), nil
	}
}

const (
	planHead = "package p\n\nimport \"testing\"\n\nfunc TestKeep(t *testing.T) {}\n\nfunc TestNew(t *testing.T) {}\n\nfunc TestChanged(t *testing.T) {\n\t_ = 2\n}\n"
	planBase = "package p\n\nimport \"testing\"\n\nfunc TestKeep(t *testing.T) {}\n\nfunc TestChanged(t *testing.T) {\n\t_ = 1\n}\n"
	planNew  = "package p\n\nimport \"testing\"\n\nfunc TestOther(t *testing.T) {}\n"
)

func TestRedBasePlan(t *testing.T) {
	t.Parallel()
	// A deleted file, a non-test file, a testdata file or a nested module's file is never read: planReader has no entry for it.
	g := planGit{
		fakeGit: fakeGit{
			diff:      "M\tpkg/a_test.go\nA\tpkg/b_test.go\nA\tplugins/x_test.go\nD\tpkg/gone_test.go\nM\tpkg/impl.go\nA\tpkg/testdata/f_test.go\n",
			mergeBase: "abc",
			atBase:    map[string]string{"pkg/a_test.go": planBase},
		},
		modules: []string{"plugins/go.mod"},
	}
	head := map[string]string{"pkg/a_test.go": planHead, "pkg/b_test.go": planNew}
	want := RedBasePlanResult{
		Packages: []RedBasePackage{{Dir: "pkg", Files: []string{"pkg/a_test.go", "pkg/b_test.go"}, Tests: []RedBaseTest{
			{Name: "TestChanged", File: "pkg/a_test.go", Line: 9},
			{Name: "TestNew", File: "pkg/a_test.go", Line: 7},
			{Name: "TestOther", File: "pkg/b_test.go", Line: 5},
		}}},
		NotJudged: []string{"plugins/x_test.go"},
	}
	got, err := RedBasePlan(g.run, planReader(head, nil), "main")
	if err != nil {
		t.Fatalf("RedBasePlan() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RedBasePlan() = %+v, want %+v", got, want)
	}
}

// A branch with no changed test file returns no merge-base call: fakeGit's empty answer would reach mergeBase as an error.
func TestRedBasePlanNoTestFile(t *testing.T) {
	t.Parallel()
	g := planGit{fakeGit: fakeGit{diff: "M\tpkg/impl.go\n"}}
	got, err := RedBasePlan(g.run, planReader(nil, nil), "main")
	if err != nil {
		t.Fatalf("RedBasePlan() error = %v", err)
	}
	if len(got.Packages) != 0 || len(got.NotJudged) != 0 {
		t.Errorf("RedBasePlan() = %+v, want no packages and nothing not judged", got)
	}
}

func TestRedBasePlanRefuses(t *testing.T) {
	t.Parallel()
	boom := fmt.Errorf("boom")
	badGo := "package p\n\nfunc f( {\n"
	tests := []struct {
		name    string
		git     planGit
		head    map[string]string
		readErr error
		want    string
	}{
		{"a failed file list is returned", planGit{fakeGit: fakeGit{diffErr: boom}}, nil, nil, "git diff"},
		{"a failed merge-base is returned", planGit{fakeGit: fakeGit{diff: "M\tpkg/a_test.go\n", mergeErr: boom}}, nil, nil, "merge-base"},
		{"a failed file read is returned", planGit{fakeGit: fakeGit{diff: "A\tpkg/a_test.go\n", mergeBase: "abc"}}, nil, boom, "reading"},
		{"a HEAD file that does not parse is returned", planGit{fakeGit: fakeGit{diff: "A\tpkg/a_test.go\n", mergeBase: "abc"}}, map[string]string{"pkg/a_test.go": badGo}, nil, "pkg/a_test.go"},
		{"a failed show at the merge base is returned", planGit{fakeGit: fakeGit{diff: "M\tpkg/a_test.go\n", mergeBase: "abc", baseErr: boom}}, nil, nil, "merge base"},
		{"a base file that does not parse is returned", planGit{fakeGit: fakeGit{diff: "M\tpkg/a_test.go\n", mergeBase: "abc", atBase: map[string]string{"pkg/a_test.go": badGo}}}, nil, nil, "pkg/a_test.go"},
		{"an integration-tagged HEAD file that does not parse is returned", planGit{fakeGit: fakeGit{diff: "A\tpkg/a_test.go\n", mergeBase: "abc"}}, map[string]string{"pkg/a_test.go": "//go:build integration\n\n" + badGo}, nil, "pkg/a_test.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			head := tt.head
			if head == nil {
				head = map[string]string{"pkg/a_test.go": planNew}
			}
			g := tt.git
			if _, err := RedBasePlan(g.run, planReader(head, tt.readErr), "main"); err == nil {
				t.Fatalf("RedBasePlan() error = nil, want one")
			} else if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("RedBasePlan() error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}
