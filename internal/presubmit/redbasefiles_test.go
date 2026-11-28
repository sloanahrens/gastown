package presubmit

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// planGit answers ls-files for RedBaseFiles' nested-module list and defers every other command to
// fakeGit, which answers the diff.
type planGit struct {
	fakeGit
	modules   []string
	moduleErr error
}

func (g *planGit) run(args []string) ([]byte, error) {
	if slices.Contains(args, "ls-files") {
		if g.moduleErr != nil {
			return nil, g.moduleErr
		}
		return []byte(strings.Join(g.modules, "\x00")), nil
	}
	if len(args) > 1 && args[0] == "-c" {
		args = args[2:]
	}
	return g.fakeGit.run(args)
}

func TestRedBaseFiles(t *testing.T) {
	t.Parallel()
	boom := fmt.Errorf("boom")
	// A filter row sets moduleErr so a file that slips through the filter reaches git ls-files and
	// fails the row, which also shows the module list is unasked when no test file changes.
	filter := func(line string) planGit {
		return planGit{fakeGit: fakeGit{diff: line}, moduleErr: boom}
	}
	tests := []struct {
		name          string
		git           planGit
		wantFiles     []RedBaseFile
		wantNotJudged []string
	}{
		{
			name: "added and modified root test files are sorted and marked added",
			git:  planGit{fakeGit: fakeGit{diff: "M\tpkg/z_test.go\nA\tother/b_test.go\nM\tpkg/a_test.go\nA\tother/a_test.go\n"}},
			wantFiles: []RedBaseFile{
				{Path: "other/a_test.go", Added: true},
				{Path: "other/b_test.go", Added: true},
				{Path: "pkg/a_test.go"},
				{Path: "pkg/z_test.go"},
			},
		},
		{"a deleted file gives nothing", filter("D\tpkg/a_test.go\n"), nil, nil},
		{"a status other than A or M gives nothing", filter("T\tpkg/a_test.go\n"), nil, nil},
		{"a non-test file gives nothing", filter("A\tpkg/impl.go\n"), nil, nil},
		{"a file named starting _ gives nothing", filter("A\tpkg/_helper_test.go\n"), nil, nil},
		{"a file named starting . gives nothing", filter("A\tpkg/.hidden_test.go\n"), nil, nil},
		{"a file under testdata gives nothing", filter("A\tpkg/testdata/f_test.go\n"), nil, nil},
		{"a file under a directory starting _ gives nothing", filter("A\tpkg/_skip/c_test.go\n"), nil, nil},
		{"a file under a directory starting . gives nothing", filter("A\tpkg/.git/c_test.go\n"), nil, nil},
		{
			name: "a nested module's test files are not judged while a prefix sibling is",
			git: planGit{
				fakeGit: fakeGit{diff: "A\tpluginsx/y_test.go\nA\tplugins/x_test.go\nA\tpkg/q_test.go\nA\tplugins/p/y_test.go\n"},
				modules: []string{"plugins/go.mod"},
			},
			wantFiles:     []RedBaseFile{{Path: "pkg/q_test.go", Added: true}, {Path: "pluginsx/y_test.go", Added: true}},
			wantNotJudged: []string{"plugins/p/y_test.go", "plugins/x_test.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := tt.git
			files, notJudged, err := RedBaseFiles(g.run, "main")
			if err != nil {
				t.Fatalf("RedBaseFiles() error = %v", err)
			}
			if !reflect.DeepEqual(files, tt.wantFiles) {
				t.Errorf("RedBaseFiles() files = %+v, want %+v", files, tt.wantFiles)
			}
			if !reflect.DeepEqual(notJudged, tt.wantNotJudged) {
				t.Errorf("RedBaseFiles() notJudged = %+v, want %+v", notJudged, tt.wantNotJudged)
			}
		})
	}
}

func TestRedBaseFilesRefuses(t *testing.T) {
	t.Parallel()
	boom := fmt.Errorf("boom")
	tests := []struct {
		name string
		git  planGit
		base string
		want string
	}{
		{"an empty base is returned", planGit{}, "", "empty base"},
		{"a failed diff is returned", planGit{fakeGit: fakeGit{diffErr: boom}}, "main", "git diff"},
		{"an unreadable name-status line is returned", planGit{fakeGit: fakeGit{diff: "garbage\n"}}, "main", "unreadable"},
		{"a quoted path is returned", planGit{fakeGit: fakeGit{diff: "A\t\"caf\\303\\251_test.go\"\n"}}, "main", "quoted"},
		{"a failed module list read is returned", planGit{fakeGit: fakeGit{diff: "M\tpkg/a_test.go\n"}, moduleErr: boom}, "main", "go.mod"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := tt.git
			if _, _, err := RedBaseFiles(g.run, tt.base); err == nil {
				t.Fatalf("RedBaseFiles(%q) error = nil, want one", tt.base)
			} else if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("RedBaseFiles(%q) error = %v, want it to mention %q", tt.base, err, tt.want)
			}
		})
	}
}
