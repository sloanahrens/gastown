package tiersweep

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/iotest"
)

// parseFixture runs ParseTestJSON over one fixture under testdata/. A fixture
// that cannot be read fails the test, so a missing file never looks like an
// empty stream.
func parseFixture(t *testing.T, name string) (string, []TestTime) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	text, tests, err := ParseTestJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseTestJSON(%s): %v", name, err)
	}
	return text, tests
}

// TestParseTestJSONPlainText: the text of a passing package is the ok line and
// the PASS line in stream order, and the text of a failing test keeps its
// --- FAIL line, its panic text and the FAIL and package FAIL lines.
func TestParseTestJSONPlainText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		fixture string
		want    string
	}{
		{
			name:    "passing package",
			fixture: "testjson_pass.jsonl",
			want:    "PASS\nok  \texample.com/fixture/a\t1.2s\n",
		},
		{
			name:    "failing test with a panic",
			fixture: "testjson_fail.jsonl",
			want: "=== RUN   TestBoom\n" +
				"--- FAIL: TestBoom (0.01s)\n" +
				"panic: boom\n" +
				"\n" +
				"goroutine 7 [running]:\n" +
				"\tb_test.go:8 +0x2c\n" +
				"FAIL\n" +
				"FAIL\texample.com/fixture/b\t0.02s\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			text, _ := parseFixture(t, tc.fixture)
			if text != tc.want {
				t.Errorf("text = %q, want %q", text, tc.want)
			}
		})
	}
}

// TestParseTestJSONTimes: each top-level test gets its own elapsed seconds and
// outcome, in the order its terminal event comes in the stream; a skipped test
// is reported as skip; a subtest is not listed on its own.
func TestParseTestJSONTimes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		fixture string
		want    []TestTime
	}{
		{
			name:    "passing test",
			fixture: "testjson_pass.jsonl",
			want: []TestTime{
				{Test: "TestAdd", Package: "example.com/fixture/a", Elapsed: 0, Action: "pass"},
			},
		},
		{
			name:    "failing test",
			fixture: "testjson_fail.jsonl",
			want: []TestTime{
				{Test: "TestBoom", Package: "example.com/fixture/b", Elapsed: 0.01, Action: "fail"},
			},
		},
		{
			name:    "interleaved tests with a subtest and a skip",
			fixture: "testjson_interleave.jsonl",
			want: []TestTime{
				{Test: "TestFast", Package: "example.com/fixture/c", Elapsed: 0.1, Action: "pass"},
				{Test: "TestSkipped", Package: "example.com/fixture/c", Elapsed: 0, Action: "skip"},
				{Test: "TestSlow", Package: "example.com/fixture/c", Elapsed: 2.5, Action: "pass"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, tests := parseFixture(t, tc.fixture)
			if !reflect.DeepEqual(tests, tc.want) {
				t.Errorf("tests = %+v, want %+v", tests, tc.want)
			}
		})
	}
}

// TestParseTestJSONBuildFailureAndNoise: a package that failed to build returns
// its output and no tests, and a non-JSON line is kept in the text in place
// and does not stop the parse of the events after it.
func TestParseTestJSONBuildFailureAndNoise(t *testing.T) {
	t.Parallel()
	t.Run("build failure", func(t *testing.T) {
		t.Parallel()
		text, tests := parseFixture(t, "testjson_build.jsonl")
		want := "# example.com/fixture/e\n" +
			"./e.go:3:2: undefined: missing\n" +
			"FAIL\texample.com/fixture/e [build failed]\n" +
			"FAIL\n"
		if text != want {
			t.Errorf("text = %q, want %q", text, want)
		}
		if tests != nil {
			t.Errorf("tests = %+v, want none", tests)
		}
	})
	t.Run("non-JSON line in the middle", func(t *testing.T) {
		t.Parallel()
		text, tests := parseFixture(t, "testjson_noise.jsonl")
		want := "=== RUN   TestAfterNoise\n" +
			"ld: warning: stray tool output between two events\n" +
			"PASS\n" +
			"ok  \texample.com/fixture/d\t0.05s\n"
		if text != want {
			t.Errorf("text = %q, want %q", text, want)
		}
		wantTests := []TestTime{
			{Test: "TestAfterNoise", Package: "example.com/fixture/d", Elapsed: 0.01, Action: "pass"},
		}
		if !reflect.DeepEqual(tests, wantTests) {
			t.Errorf("tests = %+v, want %+v", tests, wantTests)
		}
	})
}

// TestParseTestJSONReadError: a reader that fails returns that error with no
// text and no tests, so a broken stream never reads as a clean run.
func TestParseTestJSONReadError(t *testing.T) {
	t.Parallel()
	boom := errors.New("disk gone")
	text, tests, err := ParseTestJSON(iotest.ErrReader(boom))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if text != "" || tests != nil {
		t.Errorf("text = %q, tests = %+v, want none", text, tests)
	}
}
