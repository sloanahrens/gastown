package presubmit

import (
	"bytes"
	"errors"
	"testing"
)

func TestRedBaseReportLines(t *testing.T) {
	t.Parallel()
	rt := func(name string, line int) RedBaseTest { return RedBaseTest{Name: name, File: "x_test.go", Line: line} }
	pkg := func(dir string, tests ...RedBaseTest) RedBasePackage { return RedBasePackage{Dir: dir, Tests: tests} }
	plan := func(pkgs ...RedBasePackage) RedBasePlanResult { return RedBasePlanResult{Packages: pkgs} }
	for _, tt := range []struct {
		name     string
		plan     RedBasePlanResult
		findings []RedBaseFinding
		want     string
		wantOK   bool
	}{
		{"findings print one line each in order, then the count, and are not ok", plan(pkg("b", rt("TestB", 3)), pkg("a", rt("TestA", 7), rt("TestC", 8))),
			[]RedBaseFinding{{Test: rt("TestB", 3)}, {Test: rt("TestA", 7)}},
			"x_test.go:3: TestB passes without the change\nx_test.go:7: TestA passes without the change\nredbase: 3 new or changed test(s) in 2 package(s), 2 passing without the change\n", false},
		{"no findings is ok with the count line, and a package with no test counts in neither total", plan(pkg("a"), pkg("b", rt("TestA", 3))),
			nil, "redbase: 1 new or changed test(s) in 1 package(s), 0 passing without the change\n", true},
		{"an empty plan is ok and counts zero", plan(), nil,
			"redbase: 0 new or changed test(s) in 0 package(s), 0 passing without the change\n", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if ok, err := RedBaseReport(&buf, tt.plan, tt.findings); err != nil || ok != tt.wantOK || buf.String() != tt.want {
				t.Errorf("RedBaseReport = %v, %v, %q; want %v, nil, %q", ok, err, buf.String(), tt.wantOK, tt.want)
			}
		})
	}
}

func TestRedBaseReportWriteError(t *testing.T) {
	t.Parallel()
	broken := errors.New("device full")
	for name, findings := range map[string][]RedBaseFinding{
		"a failed finding line is returned": {{Test: RedBaseTest{Name: "TestA", File: "x_test.go", Line: 1}}},
		"a failed count line is returned":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if ok, err := RedBaseReport(&failOnce{err: broken}, RedBasePlanResult{}, findings); ok || !errors.Is(err, broken) {
				t.Errorf("RedBaseReport = %v, %v; want false and an error wrapping %v", ok, err, broken)
			}
		})
	}
}

// failOnce fails its first write and then accepts the rest, so a guard that ignores the error lets the report read clean.
type failOnce struct{ err error }

func (f *failOnce) Write(p []byte) (int, error) {
	if f.err == nil {
		return len(p), nil
	}
	err := f.err
	f.err = nil
	return 0, err
}
