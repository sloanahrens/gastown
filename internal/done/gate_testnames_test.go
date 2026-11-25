package done

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// TestTestFuncNamesKeepsOnlyTestShapedFunctions: names come back in file then
// declaration order, and only the top-level functions go test would list.
func TestTestFuncNamesKeepsOnlyTestShapedFunctions(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"b_test.go": {Data: []byte("package x\n\nfunc TestB(t *testing.T) {}\nfunc Test1(t *testing.T) {}\n")},
		"a_test.go": {Data: []byte(`package x

import "testing"
type suite struct{}

func TestA(t *testing.T) {}
func BenchmarkA(b *testing.B) {}
func FuzzA(f *testing.F) {}
func ExampleA() {}
func Test_x(t *testing.T) {}
func Testfoo(t *testing.T) {}
func TestMain(m *testing.M) {}
func helper() {}
func (suite) TestX(t *testing.T) {}
func Outer() { _ = func(t *testing.T) {} }
`)},
	}
	got, err := testFuncNames(fsys, []string{"b_test.go", "a_test.go"})
	if err != nil {
		t.Fatalf("testFuncNames() error = %v", err)
	}
	want := []string{"TestB", "Test1", "TestA", "BenchmarkA", "FuzzA", "ExampleA", "Test_x"}
	if !slices.Equal(got, want) {
		t.Errorf("testFuncNames() = %q, want %q", got, want)
	}
}

// TestTestFuncNamesErrors: a file that does not parse and a file that cannot
// be read are refused, each error naming the file; an empty list names nothing.
func TestTestFuncNamesErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		fsys fstest.MapFS
		file string
	}{
		{"unparseable", fstest.MapFS{"bad_test.go": {Data: []byte("package x\nfunc TestA( {\n")}}, "bad_test.go"},
		{"unreadable", fstest.MapFS{}, "missing_test.go"},
	} {
		_, err := testFuncNames(tc.fsys, []string{tc.file})
		if err == nil || !strings.Contains(err.Error(), tc.file) {
			t.Errorf("%s file: testFuncNames() error = %v, want an error naming %s", tc.name, err, tc.file)
		}
	}
	if got, err := testFuncNames(fstest.MapFS{}, nil); err != nil || len(got) != 0 {
		t.Errorf("testFuncNames(nil) = %q, %v; want no names and no error", got, err)
	}
}
