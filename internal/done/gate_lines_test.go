package done

import (
	"slices"
	"strings"
	"testing"
)

// TestGateLinesReturnsGoTestRunLines is gt-pzgyl8's table: each Gate line of
// the go test -run form yields its line, packages and pattern, and the lines
// the branch check cannot read yield none. A want row reads
// "line|packages|pattern", packages joined by a space.
func TestGateLinesReturnsGoTestRunLines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		gate string
		want []string
	}{
		{"single-quoted pattern", "go test ./internal/done -run 'TestGateLines' -count=1",
			[]string{"go test ./internal/done -run 'TestGateLines' -count=1|./internal/done|TestGateLines"}},
		{"double-quoted pattern", `go test ./internal/done -run "TestGateLines"`,
			[]string{`go test ./internal/done -run "TestGateLines"|./internal/done|TestGateLines`}},
		{"unquoted pattern", "go test ./internal/done -run TestGateLines",
			[]string{"go test ./internal/done -run TestGateLines|./internal/done|TestGateLines"}},
		{"two packages", "go test ./internal/done ./internal/hygiene -run 'TestGateLines'",
			[]string{"go test ./internal/done ./internal/hygiene -run 'TestGateLines'|./internal/done ./internal/hygiene|TestGateLines"}},
		{"the -run= spelling", "go test ./internal/done -run=TestGateLines",
			[]string{"go test ./internal/done -run=TestGateLines|./internal/done|TestGateLines"}},
		{"the -run= spelling with a quoted pattern", "go test ./internal/done -run='TestGateLines|TestGateRunLines'",
			[]string{"go test ./internal/done -run='TestGateLines|TestGateRunLines'|./internal/done|TestGateLines|TestGateRunLines"}},
		{"a flag value is not a package", "go test ./internal/done -count 1 -run 'TestGateLines'",
			[]string{"go test ./internal/done -count 1 -run 'TestGateLines'|./internal/done|TestGateLines"}},
		{"a flag before the packages", "go test -count=1 ./internal/done -run 'TestGateLines'",
			[]string{"go test -count=1 ./internal/done -run 'TestGateLines'|./internal/done|TestGateLines"}},
		{"a flag value before the packages", "go test -count 1 ./internal/done -run 'TestGateLines'",
			[]string{"go test -count 1 ./internal/done -run 'TestGateLines'|./internal/done|TestGateLines"}},
		{"alternatives kept whole", "go test ./internal/done -run 'TestGateLines|TestGateRunLines'",
			[]string{"go test ./internal/done -run 'TestGateLines|TestGateRunLines'|./internal/done|TestGateLines|TestGateRunLines"}},
		{"an anchored pattern", "go test ./internal/done -run '^TestGateLines$' -count=1",
			[]string{"go test ./internal/done -run '^TestGateLines$' -count=1|./internal/done|^TestGateLines$"}},
		{"anchored alternatives and a subtest level", "go test ./internal/done -run '^(TestA|TestB)$|^TestC$/row$'",
			[]string{"go test ./internal/done -run '^(TestA|TestB)$|^TestC$/row$'|./internal/done|^(TestA|TestB)$|^TestC$/row$"}},
		{"an unquoted anchor ends its word", "go test ./internal/done -run TestGateLines$ -count=1",
			[]string{"go test ./internal/done -run TestGateLines$ -count=1|./internal/done|TestGateLines$"}},
		{"a variable in the pattern", "go test ./internal/done -run 'Test$HOME'", nil},
		{"a braced variable in the pattern", "go test ./internal/done -run 'Test${HOME}'", nil},
		{"an unquoted variable", "go test ./internal/done -run Test$HOME", nil},
		{"a variable in another flag", "go test ./internal/done -run TestGateLines -count=$N", nil},
		{"line without -run", "go test ./internal/hygiene ./internal/testpolicy -count=1", nil},
		{"go vet", "go vet ./internal/done", nil},
		{"a ./... package", "go test ./... -run 'TestGateLines'", nil},
		{"a metacharacter fused to the -run value", "go test ./internal/done -run TestA; go vet ./internal/done", nil},
		{"an unquoted alternative is a pipe", "go test ./internal/done -run TestGateLines|TestGateRunLines", nil},
		{"a redirect after the command", "go test ./internal/done -run 'TestGateLines' > out.txt", nil},
		{"a package outside the safe set", "go test ./internal/done@v1 -run 'TestGateLines'", nil},
		{"a pattern outside the safe set", "go test ./internal/done -run 'Test$(id)'", nil},
		{"a flag where the pattern belongs", "go test ./internal/done -run -v", nil},
		{"an unterminated quote", "go test ./internal/done -run 'TestGateLines", nil},
		{"an unterminated quote after the pattern", "go test ./internal/done -run 'TestGateLines' -count='1", nil},
		{"lines the check cannot read", "go test ./internal/done -run\ngo test -run 'TestGateLines'\ngo test\ngo test ./internal/done -run 'TestGateLines' && go vet ./internal/done", nil},
		{"two lines keep their order", "go test ./internal/a -run 'TestA'\ngo vet ./internal/a\ngo test ./internal/b -run 'TestB'\nmkdir -p out",
			[]string{
				"go test ./internal/a -run 'TestA'|./internal/a|TestA",
				"go test ./internal/b -run 'TestB'|./internal/b|TestB",
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, run := range gateRunLines(tc.gate) {
				got = append(got, run.Line+"|"+strings.Join(run.Packages, " ")+"|"+run.Pattern)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("gateRunLines(%q) = %q; want %q", tc.gate, got, tc.want)
			}
		})
	}
}

// TestGateRunReadsTags is gt-mwzlyk's table: a Gate line's -tags list is kept
// as written, in either spelling, when it holds only letters, digits, comma,
// underscore and dot. A list with any other text reads as no tags, and the
// line is still read. A want row lists the Tags of each run the line yields.
func TestGateRunReadsTags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		gate string
		want []string
	}{
		{"the -tags spelling", "go test -tags integration ./internal/done -run 'TestGateRun'", []string{"integration"}},
		{"the -tags= spelling", "go test -tags=integration ./internal/done -run 'TestGateRun'", []string{"integration"}},
		{"a comma list", "go test -tags integration,unit ./internal/done -run TestGateRun", []string{"integration,unit"}},
		{"dot and underscore", "go test -tags=go_1.22 ./internal/done -run TestGateRun", []string{"go_1.22"}},
		{"a quoted tag list", "go test -tags 'integration' ./internal/done -run 'TestGateRun'", []string{"integration"}},
		{"tags after the pattern", "go test ./internal/done -run 'TestGateRun' -tags integration", []string{"integration"}},
		{"no tags", "go test ./internal/done -run 'TestGateRun' -count=1", []string{""}},
		{"a plus in the list", "go test -tags=integration+race ./internal/done -run 'TestGateRun'", []string{""}},
		{"a space inside quotes", "go test -tags 'integ ration' ./internal/done -run 'TestGateRun'", []string{""}},
		{"-tags with no value", "go test ./internal/done -run 'TestGateRun' -tags", []string{""}},
		{"a flag taken as the tag value", "go test -tags -run 'TestGateRun' ./internal/done", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, run := range gateRunLines(tc.gate) {
				got = append(got, run.Tags)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("gateRunLines(%q) tags = %q; want %q", tc.gate, got, tc.want)
			}
		})
	}
}
