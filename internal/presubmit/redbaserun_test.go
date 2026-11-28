package presubmit

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestJudgeRedBase(t *testing.T) {
	t.Parallel()
	test := func(name string) RedBaseTest { return RedBaseTest{Name: name, File: "x_test.go", Line: 1} }
	pkg := func(dir string, names ...string) RedBasePackage {
		p := RedBasePackage{Dir: dir}
		for _, n := range names {
			p.Tests = append(p.Tests, test(n))
		}
		return p
	}
	event := func(action, name string) string { return `{"Action":"` + action + `","Test":"` + name + `"}` + "\n" }
	tests := []struct {
		name     string
		packages []RedBasePackage
		out      map[string]string // go test output by package argument
		want     []RedBaseFinding
		wantRed  int      // the planned tests red on the base
		wantRuns []string // the package argument of each go call, in order
		wantErr  string   // a substring of the required error; "" means no error
	}{
		{"a passed named test is a finding, not red", []RedBasePackage{pkg("a", "TestA")}, map[string]string{"./a": event("pass", "TestA")},
			[]RedBaseFinding{{Dir: "a", Test: test("TestA")}}, 0, []string{"./a"}, ""},
		{"a failed named test is red, not a finding", []RedBasePackage{pkg("a", "TestA")}, map[string]string{"./a": event("fail", "TestA")},
			nil, 1, []string{"./a"}, ""},
		{"a passed test the plan does not name is not", []RedBasePackage{pkg("a", "TestA")}, map[string]string{"./a": event("pass", "TestOther") + event("fail", "TestA")},
			nil, 1, []string{"./a"}, ""},
		{"a package with no names runs nothing", []RedBasePackage{pkg("a"), pkg("b", "TestB")}, map[string]string{"./b": event("fail", "TestB")},
			nil, 1, []string{"./b"}, ""},
		{"findings are sorted by package and test name", []RedBasePackage{pkg("b", "TestB"), pkg("a", "TestZ", "TestC")},
			map[string]string{"./b": event("pass", "TestB"), "./a": event("pass", "TestZ") + event("pass", "TestC")},
			[]RedBaseFinding{{Dir: "a", Test: test("TestC")}, {Dir: "a", Test: test("TestZ")}, {Dir: "b", Test: test("TestB")}},
			0, []string{"./b", "./a"}, ""},
		{"a planned test with no event under a failed package is red", []RedBasePackage{pkg("a", "TestA")}, map[string]string{"./a": `{"Action":"fail","Package":"a"}` + "\n"},
			nil, 1, []string{"./a"}, ""},
		{"a fail, a pass, a skip and an unrun test under a failed package count only the fail and the unrun red", []RedBasePackage{pkg("a", "TestFail", "TestPass", "TestSkip", "TestUnrun")},
			map[string]string{"./a": event("fail", "TestFail") + event("pass", "TestPass") + event("skip", "TestSkip") + `{"Action":"fail","Package":"a"}` + "\n"},
			[]RedBaseFinding{{Dir: "a", Test: test("TestPass")}}, 2, []string{"./a"}, ""},
		{"a package pass leaves a planned test without an event an error", []RedBasePackage{pkg("a", "TestA")}, map[string]string{"./a": `{"Action":"pass","Package":"a"}` + "\n"},
			nil, 0, []string{"./a"}, "package a: test TestA"},
		{"a package skip leaves a planned test without an event an error", []RedBasePackage{pkg("a", "TestA")}, map[string]string{"./a": `{"Action":"skip","Package":"a"}` + "\n"},
			nil, 0, []string{"./a"}, "package a: test TestA"},
		{"no package result leaves a planned test without an event an error", []RedBasePackage{pkg("a", "TestA")}, map[string]string{"./a": ""},
			nil, 0, []string{"./a"}, "package a: test TestA"},
		{"a failed named test under a package pass is not an error", []RedBasePackage{pkg("a", "TestA")}, map[string]string{"./a": event("fail", "TestA") + `{"Action":"pass","Package":"a"}` + "\n"},
			nil, 1, []string{"./a"}, ""},
		{"a skipped named test under a package pass is neither red nor a finding", []RedBasePackage{pkg("a", "TestA")}, map[string]string{"./a": event("skip", "TestA") + `{"Action":"pass","Package":"a"}` + "\n"},
			nil, 0, []string{"./a"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var runs []string
			got, red, err := JudgeRedBaseRuns(RedBasePlanResult{Packages: tt.packages}, "tree", func(dir string, args []string) ([]byte, error) {
				if dir != "tree" || len(args) == 0 {
					t.Fatalf("go ran in %q with %v, want the tree and a package argument", dir, args)
				}
				arg := args[len(args)-1]
				runs = append(runs, arg)
				return []byte(tt.out[arg]), nil
			})
			if !reflect.DeepEqual(runs, tt.wantRuns) {
				t.Errorf("go ran on %v, want %v", runs, tt.wantRuns)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || got != nil {
					t.Errorf("JudgeRedBaseRuns = %v, %d, %v after runs %v; want no findings and an error holding %q", got, red, err, runs, tt.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tt.want) || red != tt.wantRed {
				t.Errorf("JudgeRedBaseRuns = %v, %d, %v after runs %v; want %v, %d, nil after %v", got, red, err, runs, tt.want, tt.wantRed, tt.wantRuns)
			}
		})
	}
}

// TestJudgeRedBaseDropsRedCount holds that the judge adapter callers use returns JudgeRedBaseRuns's findings and not its count (gt-lbmdp6).
func TestJudgeRedBaseDropsRedCount(t *testing.T) {
	t.Parallel()
	plan := RedBasePlanResult{Packages: []RedBasePackage{
		{Dir: "a", Tests: []RedBaseTest{{Name: "TestFail", File: "x_test.go", Line: 1}, {Name: "TestPass", File: "x_test.go", Line: 2}}},
	}}
	got, err := JudgeRedBase(plan, "tree", func(string, []string) ([]byte, error) {
		return []byte(`{"Action":"fail","Test":"TestFail"}` + "\n" + `{"Action":"pass","Test":"TestPass"}`), nil
	})
	want := []RedBaseFinding{{Dir: "a", Test: RedBaseTest{Name: "TestPass", File: "x_test.go", Line: 2}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("JudgeRedBase = %v, %v; want %v, nil", got, err, want)
	}
}

func TestJudgeRedBaseRunnerError(t *testing.T) {
	t.Parallel()
	broken := errors.New("go: executable file not found")
	plan := RedBasePlanResult{Packages: []RedBasePackage{
		{Dir: "a", Tests: []RedBaseTest{{Name: "TestA"}}},
		{Dir: "internal/b", Tests: []RedBaseTest{{Name: "TestB"}}},
	}}
	got, err := JudgeRedBase(plan, "tree", func(_ string, args []string) ([]byte, error) {
		if args[len(args)-1] == "./a" {
			return []byte(`{"Action":"pass","Test":"TestA"}`), nil
		}
		if want := RedBaseGoArgs("internal/b", []string{"TestB"}); !reflect.DeepEqual(args, want) {
			t.Errorf("go args = %v, want %v", args, want)
		}
		return nil, broken
	})
	if got != nil || !errors.Is(err, broken) || !strings.Contains(err.Error(), "internal/b") {
		t.Errorf("JudgeRedBase = %v, %v; want no findings and an error naming internal/b that wraps %v", got, err, broken)
	}
}
