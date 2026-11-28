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
		wantRuns []string // the package argument of each go call, in order
	}{
		{"a passed named test is a finding", []RedBasePackage{pkg("a", "TestA")}, map[string]string{"./a": event("pass", "TestA")},
			[]RedBaseFinding{{Dir: "a", Test: test("TestA")}}, []string{"./a"}},
		{"a failed named test is not", []RedBasePackage{pkg("a", "TestA")}, map[string]string{"./a": event("fail", "TestA")},
			nil, []string{"./a"}},
		{"a passed test the plan does not name is not", []RedBasePackage{pkg("a", "TestA")}, map[string]string{"./a": event("pass", "TestOther")},
			nil, []string{"./a"}},
		{"a package with no names runs nothing", []RedBasePackage{pkg("a"), pkg("b", "TestB")}, map[string]string{"./b": event("fail", "TestB")},
			nil, []string{"./b"}},
		{"findings are sorted by package and test name", []RedBasePackage{pkg("b", "TestB"), pkg("a", "TestZ", "TestC")},
			map[string]string{"./b": event("pass", "TestB"), "./a": event("pass", "TestZ") + event("pass", "TestC")},
			[]RedBaseFinding{{Dir: "a", Test: test("TestC")}, {Dir: "a", Test: test("TestZ")}, {Dir: "b", Test: test("TestB")}},
			[]string{"./b", "./a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var runs []string
			got, err := JudgeRedBase(RedBasePlanResult{Packages: tt.packages}, "tree", func(dir string, args []string) ([]byte, error) {
				if dir != "tree" || len(args) == 0 {
					t.Fatalf("go ran in %q with %v, want the tree and a package argument", dir, args)
				}
				arg := args[len(args)-1]
				runs = append(runs, arg)
				return []byte(tt.out[arg]), nil
			})
			if err != nil || !reflect.DeepEqual(got, tt.want) || !reflect.DeepEqual(runs, tt.wantRuns) {
				t.Errorf("JudgeRedBase = %v, %v after runs %v; want %v, nil after %v", got, err, runs, tt.want, tt.wantRuns)
			}
		})
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
