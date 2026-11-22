package presubmit

import (
	"errors"
	"slices"
	"testing"
)

type exitStatus int

func (e exitStatus) Error() string { return "exit status" }
func (e exitStatus) ExitCode() int { return int(e) }

func refsCase(test string) fakeGit {
	head := map[string]string{"pkg/a.go": "package p\n\nfunc Changed() {}\n\nfunc Unchanged() {}\n"}
	if test != "" {
		head["pkg/a_test.go"] = test
	}
	return fakeGit{diff: unifiedDiff(addition{path: "pkg/a.go", start: 3, lines: []string{"func Changed() {}"}}), head: head}
}

func refsGit(g fakeGit, grep string, err error) Runner {
	return func(args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "grep" {
			return []byte(grep), err
		}
		return g.run(args)
	}
}

func TestIntegrationRefs(t *testing.T) {
	t.Parallel()
	deleted := refsCase("//go:build integration\n\npackage p\n\nfunc TestX() { Changed() }\n")
	deleted.diff = deleteDiff(deletedHunk{path: "pkg/a.go", start: 3, lines: []string{"func Changed() {", "\t_ = 1", "}"}})
	deleted.head["pkg/a.go"], deleted.mergeBase, deleted.atBase = "package p\n\nfunc Unchanged() {}\n", mergeSHA, map[string]string{"pkg/a.go": "package p\n\nfunc Changed() {\n\t_ = 1\n}\n\nfunc Unchanged() {}\n"}
	comment := refsCase("")
	comment.diff, comment.head["pkg/a.go"] = unifiedDiff(addition{path: "pkg/a.go", start: 3, lines: []string{"// note"}}), "package p\n\n// note\n\nfunc F() {}\n"
	tests := []struct {
		name  string
		git   Runner
		want  []IntegrationRef
		files int
		decls int
		err   bool
	}{
		{"a tagged test naming a changed function is listed", refsGit(refsCase("//go:build integration\n\npackage p\n\nfunc TestX() { Changed() }\n"), "pkg/a_test.go\x00", nil), []IntegrationRef{{"pkg/a_test.go", "Changed"}}, 1, 1, false},
		{"an untagged prefilter hit is not counted", refsGit(refsCase("package p\n\nfunc TestX() { Changed() }\n"), "pkg/a_test.go\x00", nil), nil, 0, 1, false},
		{"a negated integration constraint is not counted", refsGit(refsCase("//go:build !integration\n\npackage p\n\nfunc TestX() { Changed() }\n"), "pkg/a_test.go\x00", nil), nil, 0, 1, false},
		{"a tagged test naming only unchanged declarations is not listed", refsGit(refsCase("//go:build integration\n\npackage p\n\nfunc TestX() { Unchanged() }\n"), "pkg/a_test.go\x00", nil), nil, 1, 1, false},
		{"a comment or string mention is not a use", refsGit(refsCase("//go:build integration\n\npackage p\n\n// Changed()\nvar S = \"Changed\"\nfunc TestX() {}\n"), "pkg/a_test.go\x00", nil), nil, 1, 1, false},
		{"a tagged test in another directory is not read", refsGit(refsCase(""), "other/b_test.go\x00", nil), nil, 0, 1, false},
		{"a deleted function a tagged test still names is listed", refsGit(deleted, "pkg/a_test.go\x00", nil), []IntegrationRef{{"pkg/a_test.go", "Changed"}}, 1, 1, false},
		{"a diff touching only a comment counts no declaration", refsGit(comment, "", nil), nil, 0, 0, false},
		{"a grep that selects nothing is not an error", refsGit(refsCase(""), "", nil), nil, 0, 1, false},
		{"git grep's exit 1 is no match", refsGit(fakeGit{}, "", exitStatus(1)), nil, 0, 0, false},
		{"a candidate that cannot be read at HEAD is an error", refsGit(refsCase(""), "pkg/a_test.go\x00", nil), nil, 0, 0, true},
		{"a git grep failure is returned", refsGit(fakeGit{}, "", errors.New("boom")), nil, 0, 0, true},
		{"a ChangedDecls failure is returned", func([]string) ([]byte, error) { return nil, errors.New("boom") }, nil, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := IntegrationRefs(tt.git, "main")
			if (err != nil) != tt.err {
				t.Fatalf("error = %v, wantErr %v", err, tt.err)
			}
			if err == nil && (!slices.Equal(got.Refs, tt.want) || got.TaggedFiles != tt.files || got.Declarations != tt.decls || got.Note != IntegrationRefsNote) {
				t.Fatalf("refs %v, files %d, decls %d, note %q; want %v, %d, %d", got.Refs, got.TaggedFiles, got.Declarations, got.Note, tt.want, tt.files, tt.decls)
			}
		})
	}
}
