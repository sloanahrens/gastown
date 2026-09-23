package guardlint

import (
	"os"
	"path/filepath"
	"testing"
)

func checkSource(t *testing.T, src string) []Finding {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	findings, err := Check(dir)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return findings
}

func TestFlagsTrueOnErrorPath(t *testing.T) {
	findings := checkSource(t, `package sample

func guard(input string) (bool, error) {
	_, err := parse(input)
	if err != nil {
		return true, nil
	}
	return false, nil
}

func parse(s string) (string, error) { return s, nil }
`)
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", findings)
	}
	if findings[0].Func != "guard" || findings[0].Slot != "bool" {
		t.Errorf("finding = %+v, want Func=guard Slot=bool", findings[0])
	}
}

func TestFlagsSwallowedErrorOnErrorPath(t *testing.T) {
	// The exact shape gt-udrrw's worked example fixed: bool is false (a
	// legitimate-looking negative), but the error that caused it is dropped.
	findings := checkSource(t, `package sample

func lookup(id string) (string, bool, error) {
	out, err := exec(id)
	if err != nil || out == "" {
		return "", false, nil
	}
	return out, true, nil
}

func exec(s string) (string, error) { return s, nil }
`)
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", findings)
	}
	if findings[0].Func != "lookup" || findings[0].Slot != "error" {
		t.Errorf("finding = %+v, want Func=lookup Slot=error", findings[0])
	}
}

func TestDoesNotFlagPropagatedError(t *testing.T) {
	findings := checkSource(t, `package sample

func guard(input string) (bool, error) {
	_, err := parse(input)
	if err != nil {
		return false, err
	}
	return true, nil
}

func parse(s string) (string, error) { return s, nil }
`)
	if len(findings) != 0 {
		t.Fatalf("findings = %v, want none — the error is propagated, not swallowed", findings)
	}
}

func TestDoesNotFlagNonGuardShapedFunctions(t *testing.T) {
	findings := checkSource(t, `package sample

func compute(input string) (int, error) {
	if input == "" {
		return 0, nil
	}
	return len(input), nil
}
`)
	if len(findings) != 0 {
		t.Fatalf("findings = %v, want none — result shape is (int, error), not (bool, error)", findings)
	}
}

func TestDoesNotFlagOutsideErrorBranch(t *testing.T) {
	findings := checkSource(t, `package sample

func guard(input string) (bool, error) {
	if input == "" {
		return true, nil
	}
	return false, nil
}
`)
	if len(findings) != 0 {
		t.Fatalf("findings = %v, want none — neither branch tests an error", findings)
	}
}

func TestFlagsNestedInsideLoop(t *testing.T) {
	findings := checkSource(t, `package sample

func guard(inputs []string) (bool, error) {
	for _, in := range inputs {
		_, err := parse(in)
		if err != nil {
			for i := 0; i < 1; i++ {
				return true, nil
			}
		}
	}
	return false, nil
}

func parse(s string) (string, error) { return s, nil }
`)
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1 (nested inside a for loop inside the err branch)", findings)
	}
}

func TestKeyIsStableAcrossLineShifts(t *testing.T) {
	a := Finding{File: "pkg/file.go", Line: 10, Func: "Guard", Slot: "bool"}
	b := Finding{File: "pkg/file.go", Line: 40, Func: "Guard", Slot: "bool"}
	if a.Key() != b.Key() {
		t.Errorf("Key() differs across line numbers: %q vs %q", a.Key(), b.Key())
	}
}
