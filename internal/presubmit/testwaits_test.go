package presubmit

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// waitDiff writes the -U0 diff that records each numbered line of src as added
// to path, one hunk per line.
func waitDiff(path, src string, added ...int) string {
	lines := strings.Split(src, "\n")
	var adds []addition
	for _, n := range added {
		adds = append(adds, addition{path: path, start: n, lines: []string{lines[n-1]}})
	}
	return unifiedDiff(adds...)
}

// src writes a test file whose function body is lines, one per line from line 4.
func src(lines ...string) string {
	return "package x\n\nfunc F(t *testing.T) {\n" + strings.Join(lines, "\n") + "\n}\n"
}

// waitFiles answers FileReader from canned file contents.
func waitFiles(files map[string]string) FileReader {
	return func(p string) ([]byte, error) {
		file, ok := files[p]
		if !ok {
			return nil, fmt.Errorf("no canned content for %s", p)
		}
		return []byte(file), nil
	}
}

// TestJudgeTestWaits judges each kind of unbounded wait, the select exemption,
// and the rule that only added test lines are judged (gt-4ndz7z).
func TestJudgeTestWaits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		path  string
		src   string
		added []int
		want  []int
	}{
		{"a bare receive is a finding", "", src("\t<-ch"), []int{4}, []int{4}},
		{"a receive in a select with a deadline case is not", "", src("\tselect {", "\tcase <-ch:", "\tcase <-after():", "\t}"), []int{5, 6}, nil},
		{"a receive in a select with one case is still a finding", "", src("\tselect {", "\tcase <-ch:", "\t}"), []int{5}, []int{5}},
		{"a default clause is a second case", "", src("\tselect {", "\tcase <-ch:", "\tdefault:", "\t}"), []int{5}, nil},
		{"an added wg.Wait is a finding", "", src("\twg.Wait()"), []int{4}, []int{4}},
		{"an added cmd.Wait is a finding", "", src("\tcmd.Wait()"), []int{4}, []int{4}},
		{"an added Wait on a receiver of another type is a finding", "", src("\tserver.Wait()"), []int{4}, []int{4}},
		{"a Wait already on the base is not judged", "", src("\twg.Wait()", "\twg.Add(1)"), []int{5}, nil},
		{"a for loop with no condition is a finding", "", src("\tfor {", "\t}"), []int{4}, []int{4}},
		{"a for loop with a condition is not", "", src("\tfor i := 0; i < 3; i++ {", "\t}"), []int{4}, nil},
		{"a for range that names no element is a finding", "", src("\tfor range ch {", "\t}"), []int{4}, []int{4}},
		{"a for range that binds the element is not", "", src("\tfor _, tt := range tests {", "\t\t_ = tt", "\t}"), []int{4}, nil},
		{"a for loop inside a select with a second case is not", "", src("\tselect {", "\tcase <-ch:", "\tdefault:", "\t\tfor {", "\t\t}", "\t}"), []int{7}, nil},
		{"a loop and a receive on one line are one finding", "", src("\tfor { <-ch }"), []int{4}, []int{4}},
		{"an added line that is not a wait is judged but not a finding", "", src("\tx := 1"), []int{4}, nil},
		{"a wait in a non-test file is not judged", "x.go", src("\t<-ch"), []int{4}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := tt.path
			if path == "" {
				path = "x_test.go"
			}
			res, err := JudgeTestWaits(strings.NewReader(waitDiff(path, tt.src, tt.added...)), waitFiles(map[string]string{path: tt.src}))
			if err != nil {
				t.Fatalf("JudgeTestWaits: %v", err)
			}
			var got []int
			for _, f := range res.Findings {
				got = append(got, f.Line)
				if want := strings.TrimSpace(strings.Split(tt.src, "\n")[f.Line-1]); f.Path != path || f.Text != want {
					t.Errorf("finding = %s:%d %q, want %s:%d %q", f.Path, f.Line, f.Text, path, f.Line, want)
				}
			}
			wantJudged := 0
			if strings.HasSuffix(path, "_test.go") {
				wantJudged = len(tt.added)
			}
			if !slices.Equal(got, tt.want) || res.Judged != wantJudged {
				t.Errorf("findings %v with %d line(s) judged, want %v with %d", got, res.Judged, tt.want, wantJudged)
			}
		})
	}
}

// TestJudgeTestWaitsCountsEveryAddedTestLine: the judged count covers every added line across the test files, so a report states how much it read.
func TestJudgeTestWaitsCountsEveryAddedTestLine(t *testing.T) {
	t.Parallel()
	recv, other := src("\t<-ch"), src("\tx := 1", "\ty := 2")
	diff := waitDiff("a_test.go", recv, 4) + waitDiff("b_test.go", other, 4, 5) + waitDiff("x.go", recv, 4)
	res, err := JudgeTestWaits(strings.NewReader(diff), waitFiles(map[string]string{"a_test.go": recv, "b_test.go": other}))
	if err != nil {
		t.Fatalf("JudgeTestWaits: %v", err)
	}
	if res.Judged != 3 || len(res.Findings) != 1 || res.Findings[0].Path != "a_test.go" || res.Findings[0].Line != 4 {
		t.Errorf("judged %d line(s), findings %v; want 3 lines and only a_test.go:4", res.Judged, res.Findings)
	}
}

// TestJudgeTestWaitsRefusesWhatItCannotJudge: an unreadable input fails the check rather than passing as a clean branch (gt-4ndz7z).
func TestJudgeTestWaitsRefusesWhatItCannotJudge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		diff  string
		files map[string]string
		want  string
	}{
		{"an unreadable hunk header", "+++ b/a_test.go\n@@ nonsense @@\n+<-ch\n", nil, "unreadable hunk header"},
		{"a changed path git quoted", "+++ \"b/x_test.go\"\n@@ -0,0 +1,1 @@\n+<-ch\n", nil, "quoted a changed path"},
		{"a test file that cannot be read", waitDiff("a_test.go", src("\t<-ch"), 4), nil, "reading a_test.go"},
		{"a test file that does not parse", waitDiff("a_test.go", src("\t<-ch"), 4), map[string]string{"a_test.go": "package x\nfunc (\n"}, "a_test.go:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := JudgeTestWaits(strings.NewReader(tt.diff), waitFiles(tt.files))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// TestReadDiffReportsAFailedGit: a git that fails fails the check with an error naming the range, never a wait (gt-4ndz7z).
func TestReadDiffReportsAFailedGit(t *testing.T) {
	t.Parallel()
	boom := errors.New("exit status 128")
	_, err := ReadDiff(func([]string) ([]byte, error) { return nil, boom }, "origin/main")
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "origin/main...HEAD") {
		t.Errorf("error = %v, want it to wrap git's failure and name the range", err)
	}
}

// TestReadDiffRefusesAnEmptyBase: an empty base is refused before git runs, rather than diffing HEAD against itself
// and passing as a clean branch (gt-whjd81).
func TestReadDiffRefusesAnEmptyBase(t *testing.T) {
	t.Parallel()
	_, err := ReadDiff(func([]string) ([]byte, error) {
		t.Error("git ran for an empty base")
		return nil, nil
	}, "")
	if err == nil || !strings.Contains(err.Error(), "empty base") {
		t.Errorf("error = %v, want one naming the empty base", err)
	}
}
