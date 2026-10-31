package tiersweep

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// TestParseTestJSONUnfinishedTest: a top-level test that starts and prints but
// never reaches a terminal event is not listed, its output stays in the text,
// and a sibling that does finish is still listed.
func TestParseTestJSONUnfinishedTest(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`{"Action":"run","Package":"example.com/fixture/f","Test":"TestDone"}`,
		`{"Action":"pass","Package":"example.com/fixture/f","Test":"TestDone","Elapsed":0.2}`,
		`{"Action":"run","Package":"example.com/fixture/f","Test":"TestHang"}`,
		`{"Action":"output","Package":"example.com/fixture/f","Test":"TestHang","Output":"=== RUN   TestHang\n"}`,
		"",
	}, "\n")
	text, tests, err := ParseTestJSON(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("ParseTestJSON: %v", err)
	}
	if want := "=== RUN   TestHang\n"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	wantTests := []TestTime{
		{Test: "TestDone", Package: "example.com/fixture/f", Elapsed: 0.2, Action: "pass"},
	}
	if !reflect.DeepEqual(tests, wantTests) {
		t.Errorf("tests = %+v, want %+v", tests, wantTests)
	}
}

// TestParseTestJSONMalformedEvent: a line that is a valid JSON object but not a
// well-formed event fails the parse and names its line, so its text is never
// echoed raw and a missing terminal event never reads as a clean run.
func TestParseTestJSONMalformedEvent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
	}{
		{
			name: "field of the wrong type",
			line: `{"Action":"pass","Package":"example.com/fixture/g","Test":"TestX","Elapsed":"fast"}`,
		},
		{
			name: "object without an action",
			line: `{"Time":"2026-10-30T10:00:00.000Z","Package":"example.com/fixture/g"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			good := `{"Action":"output","Package":"example.com/fixture/g","Output":"PASS\n"}`
			text, tests, err := ParseTestJSON(strings.NewReader(good + "\n" + tc.line + "\n"))
			if err == nil {
				t.Fatalf("err = nil, want a malformed-event error; text = %q", text)
			}
			if !strings.Contains(err.Error(), "line 2") {
				t.Errorf("err = %q, want it to name line 2", err)
			}
			if text != "" || tests != nil {
				t.Errorf("text = %q, tests = %+v, want none", text, tests)
			}
		})
	}
}

// TestParseTestJSONBraceLineNotJSON: a line that starts with a brace but is not
// complete JSON is not an envelope, so it is kept in the text unchanged and does
// not fail the parse.
func TestParseTestJSONBraceLineNotJSON(t *testing.T) {
	t.Parallel()
	const brace = "{ build cache: miss"
	stream := `{"Action":"output","Package":"example.com/fixture/h","Output":"PASS\n"}` + "\n" +
		brace + "\n" +
		`{"Action":"output","Package":"example.com/fixture/h","Output":"ok  \texample.com/fixture/h\t0.01s\n"}` + "\n"
	text, _, err := ParseTestJSON(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("ParseTestJSON: %v", err)
	}
	if want := "PASS\n" + brace + "\n" + "ok  \texample.com/fixture/h\t0.01s\n"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
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
