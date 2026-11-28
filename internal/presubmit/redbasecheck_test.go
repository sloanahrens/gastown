package presubmit

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// redBaseSteps answers RedBaseCheck's plan, tree and judge steps from canned results and records which ran, so the unit tier runs no process and builds no tree.
type redBaseSteps struct {
	plan                                  RedBasePlanResult
	planErr, treeErr, removeErr, judgeErr error
	findings                              []RedBaseFinding

	treeCalls, removeCalls int
	files                  []string
}

func (s *redBaseSteps) check(out io.Writer) (bool, error) {
	return RedBaseCheck(
		func() (RedBasePlanResult, error) { return s.plan, s.planErr },
		func(files []string) (string, func() error, error) {
			s.treeCalls++
			s.files = files
			if s.treeErr != nil {
				return "", nil, s.treeErr
			}
			return "tree", func() error { s.removeCalls++; return s.removeErr }, nil
		},
		func(RedBasePlanResult, string) ([]RedBaseFinding, error) { return s.findings, s.judgeErr },
		out,
	)
}

// TestRedBaseCheck: no finding is ok, a finding is not, an empty plan makes no tree, and every failure is returned with the tree removed exactly when it existed (gt-mwq44k).
func TestRedBaseCheck(t *testing.T) {
	t.Parallel()
	plan := RedBasePlanResult{Packages: []RedBasePackage{{Dir: "a", Files: []string{"a/a_test.go"}, Tests: []RedBaseTest{{Name: "TestA", File: "a/a_test.go", Line: 1}}}}}
	finding := RedBaseFinding{Dir: "a", Test: RedBaseTest{Name: "TestA", File: "a/a_test.go", Line: 1}}
	boom := errors.New("boom")
	for _, tt := range []struct {
		name       string
		steps      redBaseSteps
		writeErr   error
		wantOK     bool
		wantErr    string
		wantReport string
		wantTree   int
		wantRemove int
	}{
		{"no finding is ok", redBaseSteps{plan: plan}, nil, true, "",
			"redbase: 1 new or changed test(s) in 1 package(s), 0 passing without the change\n", 1, 1},
		{"a finding is not ok", redBaseSteps{plan: plan, findings: []RedBaseFinding{finding}}, nil, false, "",
			"a/a_test.go:1: TestA passes without the change\nredbase: 1 new or changed test(s) in 1 package(s), 1 passing without the change\n", 1, 1},
		{"an empty plan makes no tree and reports zero", redBaseSteps{}, nil, true, "",
			"redbase: 0 new or changed test(s) in 0 package(s), 0 passing without the change\n", 0, 0},
		{"a plan with no tests and a not-judged file prints its line and makes no tree", redBaseSteps{plan: RedBasePlanResult{Packages: []RedBasePackage{{Dir: "a", Files: []string{"a/a_test.go"}}}, NotJudged: []string{"plugins/x_test.go"}}}, nil, true, "",
			"redbase: not judged: plugins/x_test.go\nredbase: 0 new or changed test(s) in 0 package(s), 0 passing without the change\n", 0, 0},
		{"a failed plan is returned and no tree is made", redBaseSteps{planErr: boom}, nil, false, "boom", "", 0, 0},
		{"a failed tree is returned and nothing is judged", redBaseSteps{plan: plan, treeErr: boom}, nil, false, "boom", "", 1, 0},
		{"a failed judge is returned with the tree removed", redBaseSteps{plan: plan, judgeErr: boom}, nil, false, "boom", "", 1, 1},
		{"a failed report is returned with the tree removed", redBaseSteps{plan: plan}, errors.New("device full"), false, "device full", "", 1, 1},
		{"a failed removal is returned, not ok", redBaseSteps{plan: plan, removeErr: boom}, nil, false, "boom", "", 1, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			var out io.Writer = &buf
			if tt.writeErr != nil {
				out = &failOnce{err: tt.writeErr}
			}
			ok, err := tt.steps.check(out)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("error = %v, want nil", err)
				}
				if buf.String() != tt.wantReport {
					t.Errorf("report = %q, want %q", buf.String(), tt.wantReport)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want one naming %q", err, tt.wantErr)
			}
			if tt.steps.treeCalls != tt.wantTree || tt.steps.removeCalls != tt.wantRemove {
				t.Errorf("tree calls = %d, remove calls = %d; want %d, %d", tt.steps.treeCalls, tt.steps.removeCalls, tt.wantTree, tt.wantRemove)
			}
			if tt.steps.treeCalls > 0 && !slices.Equal(tt.steps.files, []string{"a/a_test.go"}) {
				t.Errorf("tree files = %v, want the plan's files", tt.steps.files)
			}
		})
	}
}
