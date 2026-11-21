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
