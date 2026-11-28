package presubmit

import (
	"errors"
	"go/ast"
	"io"
	"slices"
	"strings"
	"testing"
)

// testSrc writes a test file with decls as its top-level declarations.
func testSrc(decls ...string) string {
	return "package x\n\n" + strings.Join(decls, "\n\n") + "\n"
}

// TestChangedTests holds one row per rule for naming the tests a change added or whose body it changed (gt-13dre6).
func TestChangedTests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		base      string
		newAtHead bool
		head      string
		want      []string
		wantErr   string
	}{
		{"a new file gives all its tests sorted", "", true, testSrc("func TestB(t *testing.T) {}", "func TestA(t *testing.T) {}"), []string{"TestA", "TestB"}, ""},
		{"an added and a body-changed test are named, sorted", testSrc("func TestC(t *testing.T) {}", "func TestA(t *testing.T) { t.Log(1) }"), false, testSrc("func TestA(t *testing.T) { t.Log(2) }", "func TestB(t *testing.T) {}", "func TestC(t *testing.T) {}"), []string{"TestA", "TestB"}, ""},
		{"a comment-only change is not a body change", testSrc("func TestA(t *testing.T) {", "// old", "t.Log(1)", "}"), false, testSrc("func TestA(t *testing.T) {", "// new", "t.Log(1)", "}"), nil, ""},
		{"a layout-only change is not a body change", testSrc("func TestA(t *testing.T) { t.Log(1) }"), false, testSrc("func TestA(t *testing.T) {", "t.Log(1)", "}"), nil, ""},
		{"TestMain, a benchmark, a helper and a method are not tests", "", true, testSrc("func TestMain(m *testing.M) {}", "func BenchmarkA(b *testing.B) {}", "func helper(t *testing.T) {}", "func (s S) TestA(t *testing.T) {}"), nil, ""},
		{"a name is Test or Test and a non-lowercase letter, taking one *testing.T", "", true, testSrc("func Test(t *testing.T) {}", "func Test_foo(t *testing.T) {}", "func Testify(t *testing.T) {}", "func TestA() {}", "func TestB(t *testing.T, u *testing.T) {}", "func TestC(t *T) {}"), []string{"Test", "Test_foo"}, ""},
		{"an integration-tagged file gives no names", "", true, "//go:build integration\n\n" + testSrc("func TestA(t *testing.T) {}"), nil, ""},
		{"a file that does not parse at HEAD is an error naming it", "", true, "package x\nfunc (\n", nil, "x_test.go"},
		{"a file that does not parse at the merge base is an error naming it", "package x\nfunc (\n", false, testSrc("func TestA(t *testing.T) {}"), nil, "x_test.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var base []byte
			if !tt.newAtHead {
				base = []byte(tt.base)
			}
			got, err := ChangedTests("x_test.go", base, []byte(tt.head))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want one naming %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ChangedTests: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("ChangedTests = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestChangedTestsReportsAPrintFailure: a body the printer cannot print fails the check, naming the test, rather than reading as an unchanged body (gt-13dre6).
func TestChangedTestsReportsAPrintFailure(t *testing.T) {
	t.Parallel()
	_, err := changedTests("x_test.go", nil, []byte(testSrc("func TestA(t *testing.T) {}")), func(io.Writer, *ast.BlockStmt) error {
		return errors.New("printer failed")
	})
	if err == nil || !strings.Contains(err.Error(), "printing TestA") {
		t.Errorf("error = %v, want one naming TestA", err)
	}
}
