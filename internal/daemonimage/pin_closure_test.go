package daemonimage

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const closureRepo = "/repo"

// answerTool gives the fake the two reads the one-level walk makes for a tool
// directory at main: the listing of its entries and the source of one file.
func answerTool(f *pinsGit, tool, file, src string) {
	f.answer[closureRepo+" ls-tree --name-only main "+tool+"/"] = file + "\n"
	f.answer[closureRepo+" show main:"+file] = src
}

// closureTree answers the reads the walk makes for every directory in files
// (directory to the source of its one file, <dir>/main.go), so a test is one map
// literal; the walk reads each directory it reaches, an unanswered one fails the
// fake and not the assertion (gt-rxxk4s).
func closureTree(f *pinsGit, files map[string]string) {
	for dir, src := range files {
		answerTool(f, dir, dir+"/main.go", src)
	}
}

func TestInputClosureOneLevelCoveredByTheList(t *testing.T) {
	t.Parallel()
	f := newPinsGit()
	answerTool(f, "cmd/reporter", "cmd/reporter/main.go", `package main
import "gt.local/devops/pkg/releasebot/sub"
import "gt.local/devops/internal/ledgerdb"
`)
	answerTool(f, "cmd/release-reporter", "cmd/release-reporter/main.go", `package main
import "gt.local/devops/pkg/patrol"
`)
	answerTool(f, "cmd/cloudcheck", "cmd/cloudcheck/main.go", `package main
import "gt.local/devops/internal/ledgerdb"
`)
	closureTree(f, map[string]string{"pkg/releasebot/sub": "package sub\n", "internal/ledgerdb": "package ledgerdb\n", "pkg/patrol": "package patrol\n"})
	got, err := inputClosureGaps(f.run, closureRepo, "main")
	if err != nil {
		t.Fatalf("inputClosureGaps: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("gaps = %v, want none: every import is under the input list", got)
	}
}

func TestInputClosureReportsAnImportOutsideTheList(t *testing.T) {
	t.Parallel()
	f := newPinsGit()
	answerTool(f, "cmd/reporter", "cmd/reporter/main.go", `package main
import "gt.local/devops/internal/newthing"
`)
	answerTool(f, "cmd/release-reporter", "cmd/release-reporter/main.go", `package main
import "gt.local/devops/pkg/patrol"
`)
	answerTool(f, "cmd/cloudcheck", "cmd/cloudcheck/main.go", `package main
import "gt.local/devops/pkg/patrol"
`)
	closureTree(f, map[string]string{"internal/newthing": "package newthing\n", "pkg/patrol": "package patrol\n"})
	got, err := inputClosureGaps(f.run, closureRepo, "main")
	if err != nil {
		t.Fatalf("inputClosureGaps: %v", err)
	}
	want := []importGap{{Dir: "internal/newthing", Importer: "cmd/reporter"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("gaps = %v, want %v", got, want)
	}
}

func TestInputClosureRefusesAFailedRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(*pinsGit)
	}{
		{"a listing git refuses", func(f *pinsGit) {
			f.errs[closureRepo+" ls-tree --name-only main cmd/reporter/"] = errors.New("bad ref")
		}},
		{"a source git refuses", func(f *pinsGit) {
			answerTool(f, "cmd/reporter", "cmd/reporter/main.go", "package main\n")
			f.errs[closureRepo+" show main:cmd/reporter/main.go"] = errors.New("gone")
		}},
		{"a source that does not parse", func(f *pinsGit) {
			answerTool(f, "cmd/reporter", "cmd/reporter/main.go", "package main\nfunc f( {\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newPinsGit()
			answerTool(f, "cmd/release-reporter", "cmd/release-reporter/main.go", "package main\n")
			answerTool(f, "cmd/cloudcheck", "cmd/cloudcheck/main.go", "package main\n")
			tc.setup(f)
			got, err := inputClosureGaps(f.run, closureRepo, "main")
			if err == nil || got != nil {
				t.Errorf("gaps = %v, err = %v; want nil gaps and an error", got, err)
			}
		})
	}
}

func TestInputClosureRefusesAToolWithNoGoFiles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		listing string
	}{
		{"an empty listing", ""},
		{"a listing of a README and a test file only", "cmd/reporter/README.md\ncmd/reporter/main_test.go\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newPinsGit()
			f.answer[closureRepo+" ls-tree --name-only main cmd/reporter/"] = tc.listing
			answerTool(f, "cmd/release-reporter", "cmd/release-reporter/main.go", "package main\n")
			answerTool(f, "cmd/cloudcheck", "cmd/cloudcheck/main.go", "package main\n")
			got, err := inputClosureGaps(f.run, closureRepo, "main")
			if err == nil || !strings.Contains(err.Error(), "cmd/reporter") || got != nil {
				t.Errorf("gaps = %v, err = %v; want nil gaps and an error naming cmd/reporter", got, err)
			}
		})
	}
}

// TestInputClosureIgnoresTestFiles pins that only the non-test sources of a tool
// are read: a test file importing a directory outside the input list is not a
// gap, since the image never builds it.
func TestInputClosureIgnoresTestFiles(t *testing.T) {
	t.Parallel()
	f := newPinsGit()
	f.answer[closureRepo+" ls-tree --name-only main cmd/reporter/"] = "cmd/reporter/main.go\ncmd/reporter/main_test.go\n"
	f.answer[closureRepo+" show main:cmd/reporter/main.go"] = "package main\n"
	f.answer[closureRepo+" show main:cmd/reporter/main_test.go"] = `package main
import "gt.local/devops/internal/onlytest"
`
	answerTool(f, "cmd/release-reporter", "cmd/release-reporter/main.go", "package main\n")
	answerTool(f, "cmd/cloudcheck", "cmd/cloudcheck/main.go", "package main\n")
	got, err := inputClosureGaps(f.run, closureRepo, "main")
	if err != nil {
		t.Fatalf("inputClosureGaps: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("gaps = %v, want none: a test file's import is not built into the image", got)
	}
}

// TestInputClosureVisitsEachDirectoryOnce pins the visited set: a two-directory
// import cycle ends, and the walk reads each directory it reaches once, so the
// cycle neither repeats a read nor keeps the walk going (gt-rblgfn).
func TestInputClosureVisitsEachDirectoryOnce(t *testing.T) {
	t.Parallel()
	f := newPinsGit()
	closureTree(f, map[string]string{
		"cmd/reporter":         "package main\nimport \"gt.local/devops/pkg/patrol\"\n",
		"cmd/release-reporter": "package main\n",
		"cmd/cloudcheck":       "package main\n",
		"pkg/patrol":           "package patrol\nimport \"gt.local/devops/pkg/releasebot\"\n",
		"pkg/releasebot":       "package releasebot\nimport \"gt.local/devops/pkg/patrol\"\n",
	})
	got, err := inputClosureGaps(f.run, closureRepo, "main")
	if err != nil {
		t.Fatalf("inputClosureGaps: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("gaps = %v, want none: every directory in the cycle is under the input list", got)
	}
	const listing = closureRepo + " ls-tree --name-only main "
	reads := map[string]int{}
	for _, call := range f.calls {
		if dir, ok := strings.CutPrefix(call, listing); ok {
			reads[strings.TrimSuffix(dir, "/")]++
		}
	}
	for _, dir := range []string{"cmd/reporter", "cmd/release-reporter", "cmd/cloudcheck", "pkg/patrol", "pkg/releasebot"} {
		if reads[dir] != 1 {
			t.Errorf("ls-tree of %s ran %d times, want 1", dir, reads[dir])
		}
	}
}

func TestInputClosureReportsADeeperImportOutsideTheList(t *testing.T) {
	t.Parallel()
	f := newPinsGit()
	closureTree(f, map[string]string{
		"cmd/reporter":         "package main\nimport \"gt.local/devops/pkg/patrol\"\n",
		"cmd/release-reporter": "package main\n",
		"cmd/cloudcheck":       "package main\n",
		"pkg/patrol":           "package patrol\nimport \"gt.local/devops/pkg/other\"\n",
		"pkg/other":            "package other\n",
	})
	got, err := inputClosureGaps(f.run, closureRepo, "main")
	if err != nil {
		t.Fatalf("inputClosureGaps: %v", err)
	}
	want := []importGap{{Dir: "pkg/other", Importer: "pkg/patrol"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("gaps = %v, want %v", got, want)
	}
}
