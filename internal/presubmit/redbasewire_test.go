package presubmit

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// The head body differs from the merge-base body, so the branch changed TestChanged.
const (
	wireHeadSrc = "package p\n\nimport \"testing\"\n\nfunc TestChanged(t *testing.T) {\n\t_ = 2\n}\n"
	wireBaseSrc = "package p\n\nimport \"testing\"\n\nfunc TestChanged(t *testing.T) {\n\t_ = 1\n}\n"
)

// wireGit answers the worktree commands and defers the rest to planGit (see redbaseplan_test.go), so the test runs no process and builds no real tree.
type wireGit struct {
	planGit
	addErr, removeErr error
	removes           int
}

func (g *wireGit) run(args []string) ([]byte, error) {
	if len(args) > 1 && args[0] == "worktree" {
		if args[1] == "add" {
			return nil, g.addErr
		}
		g.removes++
		return nil, g.removeErr
	}
	return g.planGit.run(args)
}

// TestRedBaseRunCheck: a changed test that passes in the tree is not ok and names its line, one that fails is ok and names the count, go runs in the tree under dir, the tree is removed, and a go or git error is returned and never ok (gt-o9cxm4).
func TestRedBaseRunCheck(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tree := filepath.Join(dir, "red-base")
	boom := errors.New("boom")
	plan := wireGit{planGit: planGit{fakeGit: fakeGit{diff: "M\tpkg/a_test.go\n", mergeBase: "abc", atBase: map[string]string{"pkg/a_test.go": wireBaseSrc}}}}
	tests := []struct {
		name       string
		git        wireGit
		goOut      string
		goErr      error
		wantOK     bool
		wantErr    string
		wantReport string
		wantRuns   int
		wantRemove int
	}{
		{
			name:  "a changed test that passes in the tree is not ok and reports its line",
			git:   plan,
			goOut: `{"Action":"pass","Test":"TestChanged"}` + "\n",
			wantReport: "pkg/a_test.go:5: TestChanged passes without the change\n" +
				"redbase: 1 new or changed test(s) in 1 package(s), 1 passing without the change\n",
			wantRuns:   1,
			wantRemove: 1,
		},
		{
			name:       "a changed test that fails in the tree is ok and reports the summary",
			git:        plan,
			goOut:      `{"Action":"fail","Test":"TestChanged"}` + "\n",
			wantOK:     true,
			wantReport: "redbase: 1 new or changed test(s) in 1 package(s), 0 passing without the change\n",
			wantRuns:   1,
			wantRemove: 1,
		},
		{
			name:       "a failed go run is returned and not ok",
			git:        plan,
			goErr:      boom,
			wantErr:    "boom",
			wantRuns:   1,
			wantRemove: 1,
		},
		{
			name:    "a failed git diff is returned and not ok",
			git:     wireGit{planGit: planGit{fakeGit: fakeGit{diffErr: boom}}},
			wantErr: "boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := tt.git
			runs := 0
			a := RedBaseAdapters{
				Git:   g.run,
				Read:  func(string) ([]byte, error) { return []byte(wireHeadSrc), nil },
				Mkdir: func(string) error { return nil },
				Write: func(string, []byte) error { return nil },
				GoTest: func(d string, _ []string) ([]byte, error) {
					runs++
					if d != tree {
						t.Errorf("go ran in %q, want the tree %q", d, tree)
					}
					return []byte(tt.goOut), tt.goErr
				},
			}
			var buf bytes.Buffer
			ok, err := RedBaseRunCheck(a, "main", dir, &buf)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || buf.Len() != 0 {
					t.Errorf("error = %v, report = %q; want one naming %q and no report", err, buf.String(), tt.wantErr)
				}
			} else if err != nil || buf.String() != tt.wantReport {
				t.Errorf("error = %v, report = %q; want nil and %q", err, buf.String(), tt.wantReport)
			}
			if runs != tt.wantRuns {
				t.Errorf("go runs = %d, want %d", runs, tt.wantRuns)
			}
			if g.removes != tt.wantRemove {
				t.Errorf("worktree removes = %d, want %d", g.removes, tt.wantRemove)
			}
		})
	}
}

// TestRedBaseRunCheckRefusesNilAdapter: each nil adapter field returns an error naming it before any adapter or the report runs (gt-o9cxm4).
func TestRedBaseRunCheckRefusesNilAdapter(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		nilOf func(*RedBaseAdapters)
		want  string
	}{
		{"Git", func(a *RedBaseAdapters) { a.Git = nil }, "Git"},
		{"Read", func(a *RedBaseAdapters) { a.Read = nil }, "Read"},
		{"Mkdir", func(a *RedBaseAdapters) { a.Mkdir = nil }, "Mkdir"},
		{"Write", func(a *RedBaseAdapters) { a.Write = nil }, "Write"},
		{"GoTest", func(a *RedBaseAdapters) { a.GoTest = nil }, "GoTest"},
	} {
		t.Run("a nil "+tt.name+" is refused", func(t *testing.T) {
			t.Parallel()
			ran := false
			a := RedBaseAdapters{
				Git:    func([]string) ([]byte, error) { ran = true; return nil, nil },
				Read:   func(string) ([]byte, error) { ran = true; return nil, nil },
				Mkdir:  func(string) error { ran = true; return nil },
				Write:  func(string, []byte) error { ran = true; return nil },
				GoTest: func(string, []string) ([]byte, error) { ran = true; return nil, nil },
			}
			tt.nilOf(&a)
			var buf bytes.Buffer
			ok, err := RedBaseRunCheck(a, "main", t.TempDir(), &buf)
			if ok || err == nil || !strings.Contains(err.Error(), tt.want) || buf.Len() != 0 {
				t.Errorf("RedBaseRunCheck = %v, %v, report %q; want not ok, an error naming %q and no report", ok, err, buf.String(), tt.want)
			}
			if ran {
				t.Error("an adapter ran, want none")
			}
		})
	}
}
