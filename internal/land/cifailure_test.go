package land

import (
	"slices"
	"testing"
)

func TestParseTestFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tail string
		want []TestFailure
	}{
		{
			name: "one test in one package",
			tail: "=== RUN   TestAlpha\n    a_test.go:9: want 2, got 3\n--- FAIL: TestAlpha (0.00s)\nFAIL\nFAIL\tgithub.com/x/a\t0.1s\n",
			want: []TestFailure{{Package: "github.com/x/a", Test: "TestAlpha"}},
		},
		{
			name: "each package takes the tests above its summary line",
			tail: "--- FAIL: TestAlpha (0.00s)\nFAIL\nFAIL\tgithub.com/x/a\t0.1s\n" +
				"--- FAIL: TestBeta (0.01s)\nFAIL\nFAIL\tgithub.com/x/b\t0.2s\n",
			want: []TestFailure{
				{Package: "github.com/x/a", Test: "TestAlpha"},
				{Package: "github.com/x/b", Test: "TestBeta"},
			},
		},
		{
			name: "a subtest is its top-level test, once",
			tail: "--- FAIL: TestGamma (0.00s)\n    --- FAIL: TestGamma/one (0.00s)\n    --- FAIL: TestGamma/two (0.00s)\nFAIL\nFAIL\tgithub.com/x/c\t0.3s\n",
			want: []TestFailure{{Package: "github.com/x/c", Test: "TestGamma"}},
		},
		{
			name: "a build failure names no test",
			tail: "# github.com/x/d\n./d.go:3:2: undefined: zzz\nFAIL\tgithub.com/x/d [build failed]\n",
			want: nil,
		},
		{
			name: "a passing package does not pair with the next failure",
			tail: "ok  \tgithub.com/x/e\t0.1s\n--- FAIL: TestDelta (0.00s)\nFAIL\nFAIL\tgithub.com/x/f\t0.2s\n",
			want: []TestFailure{{Package: "github.com/x/f", Test: "TestDelta"}},
		},
		{
			name: "a test with no package summary is unparsed",
			tail: "--- FAIL: TestEpsilon (0.00s)\n",
			want: nil,
		},
		{
			name: "an unfamiliar tail is unparsed",
			tail: "make: *** [Makefile:290: gate] Error 2\n",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ParseTestFailures(tt.tail)
			if !slices.Equal(got, tt.want) {
				t.Errorf("ParseTestFailures(%q) = %v, want %v", tt.tail, got, tt.want)
			}
		})
	}
}
