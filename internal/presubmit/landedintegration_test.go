package presubmit

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// taggedSrc writes an integration-tagged test file with decls in order, so the
// first declaration is at line 5 and the second at line 7.
func taggedSrc(decls ...string) string { return integrationTag + testSrc(decls...) }

// TestChangedIntegrationTests holds one row per rule for naming the integration
// tests a landing changed and the tagged files it cannot run (gt-10zcdg).
func TestChangedIntegrationTests(t *testing.T) {
	t.Parallel()
	boom := fmt.Errorf("boom")
	// The excluded-file rows use a _plan9 name, not _darwin: plan9 is never the
	// host, so the row does not depend on the machine that runs the test.
	tests := []struct {
		name       string
		git        planGit
		head       map[string]string
		readErr    error
		want       []RedBasePackage
		wantNotRun []string
		wantErr    string
	}{
		{"added and changed tests of two tagged files are named per package, sorted", planGit{fakeGit: fakeGit{
			diff:      "A\tpkg/a_test.go\nM\tpkg/b_test.go\nA\tother/c_test.go\n",
			mergeBase: "abc",
			atBase:    map[string]string{"pkg/b_test.go": taggedSrc("func TestD(t *testing.T) {}", "func TestC(t *testing.T) { t.Log(1) }")},
		}}, map[string]string{
			"pkg/a_test.go":   taggedSrc("func TestB(t *testing.T) {}", "func TestA(t *testing.T) {}"),
			"pkg/b_test.go":   taggedSrc("func TestD(t *testing.T) {}", "func TestC(t *testing.T) { t.Log(2) }"),
			"other/c_test.go": taggedSrc("func TestZ(t *testing.T) {}"),
		}, nil, []RedBasePackage{
			{"other", []string{"other/c_test.go"}, []RedBaseTest{{"TestZ", "other/c_test.go", 5}}},
			{"pkg", []string{"pkg/a_test.go", "pkg/b_test.go"}, []RedBaseTest{{"TestA", "pkg/a_test.go", 7}, {"TestB", "pkg/a_test.go", 5}, {"TestC", "pkg/b_test.go", 7}}},
		}, nil, ""},
		{"a change with no tagged test file gives no package", planGit{fakeGit: fakeGit{
			diff: "M\tpkg/a_test.go\nA\tpkg/impl.go\n", mergeBase: "abc",
			atBase: map[string]string{"pkg/a_test.go": testSrc("func TestA(t *testing.T) {}")},
		}}, map[string]string{"pkg/a_test.go": testSrc("func TestA(t *testing.T) { t.Log(1) }")}, nil, nil, nil, ""},
		{"a tagged file whose tests did not change gives no package", planGit{fakeGit: fakeGit{
			diff: "M\tpkg/a_test.go\n", mergeBase: "abc",
			atBase: map[string]string{"pkg/a_test.go": taggedSrc("func TestA(t *testing.T) {}")},
		}}, map[string]string{"pkg/a_test.go": taggedSrc("func TestA(t *testing.T) {}")}, nil, nil, nil, ""},
		{"a tagged file the integration build excludes is in notRun and gives no package", planGit{fakeGit: fakeGit{
			diff: "A\tpkg/a_plan9_test.go\n", mergeBase: "abc",
		}}, map[string]string{"pkg/a_plan9_test.go": taggedSrc("func TestA(t *testing.T) {}")}, nil, nil, []string{"pkg/a_plan9_test.go"}, ""},
		{"a nested module's tagged file and an excluded file are in notRun, sorted", planGit{
			fakeGit: fakeGit{diff: "A\tplugins/x_test.go\nA\tpkg/a_plan9_test.go\n", mergeBase: "abc"},
			modules: []string{"plugins/go.mod"},
		}, map[string]string{
			"plugins/x_test.go":   taggedSrc("func TestA(t *testing.T) {}"),
			"pkg/a_plan9_test.go": taggedSrc("func TestB(t *testing.T) {}"),
		}, nil, nil, []string{"pkg/a_plan9_test.go", "plugins/x_test.go"}, ""},
		{"an untagged nested module file is in neither", planGit{
			fakeGit: fakeGit{diff: "A\tplugins/u_test.go\n"},
			modules: []string{"plugins/go.mod"},
		}, map[string]string{"plugins/u_test.go": testSrc("func TestA(t *testing.T) {}")}, nil, nil, nil, ""},
		{"a failed file list is an error", planGit{fakeGit: fakeGit{diffErr: boom}}, nil, nil, nil, nil, "git diff"},
		{"a failed merge-base read is an error", planGit{fakeGit: fakeGit{diff: "A\tpkg/a_test.go\n", mergeBase: "abc", mergeErr: boom}}, nil, nil, nil, nil, "merge-base"},
		{"a failed head read is an error", planGit{fakeGit: fakeGit{diff: "A\tpkg/a_test.go\n", mergeBase: "abc"}}, nil, boom, nil, nil, "reading pkg/a_test.go"},
		{"a head that does not parse is an error", planGit{fakeGit: fakeGit{diff: "A\tpkg/a_test.go\n", mergeBase: "abc"}}, map[string]string{"pkg/a_test.go": integrationTag + "package x\nfunc (\n"}, nil, nil, nil, "pkg/a_test.go"},
		{"a failed merge-base file read is an error", planGit{fakeGit: fakeGit{diff: "M\tpkg/a_test.go\n", mergeBase: "abc"}}, map[string]string{"pkg/a_test.go": taggedSrc("func TestA(t *testing.T) {}")}, nil, nil, nil, "merge base"},
		{"a failed nested module read is an error", planGit{fakeGit: fakeGit{diff: "A\tplugins/x_test.go\n"}, modules: []string{"plugins/go.mod"}}, nil, boom, nil, nil, "reading plugins/x_test.go"},
		{"a failed build check is an error", planGit{fakeGit: fakeGit{diff: "A\tpkg/a_test.go\n", mergeBase: "abc"}}, map[string]string{"pkg/a_test.go": "//go:build integration && (\n\npackage x\n"}, nil, nil, nil, "build constraints"},
		{"a base that does not parse is an error", planGit{fakeGit: fakeGit{diff: "M\tpkg/a_test.go\n", mergeBase: "abc", atBase: map[string]string{"pkg/a_test.go": "package x\nfunc (\n"}}}, map[string]string{"pkg/a_test.go": taggedSrc("func TestA(t *testing.T) {}")}, nil, nil, nil, "pkg/a_test.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := tt.git
			got, notRun, err := ChangedIntegrationTests(g.run, planReader(tt.head, tt.readErr), "main")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one naming %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ChangedIntegrationTests: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) || !slices.Equal(notRun, tt.wantNotRun) {
				t.Errorf("packages = %+v, want %+v; notRun = %v, want %v", got, tt.want, notRun, tt.wantNotRun)
			}
		})
	}
}
