package cmd

import (
	"testing"
)

// The include-path probe tests (TestIntegrationVerifyCgoIncludePath_*) live
// in cgo_probe_integration_test.go: the probe guards buildGT, which only the
// integration and e2e tiers call, and each probe runs go env and the C++
// compiler from the serial phase.

// TestShellSplit_PinsCgoQuoting pins the quoting rules the probe relies on:
// a quoted include path with spaces must stay one field, and a CC value with
// arguments must split into a binary plus its defaults.
func TestShellSplit_PinsCgoQuoting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{`-I /opt/icu4c/include`, []string{"-I", "/opt/icu4c/include"}},
		{`-I"/opt/icu 4c/include" -O2`, []string{"-I/opt/icu 4c/include", "-O2"}},
		{`"ccache clang" -I/a`, []string{"ccache clang", "-I/a"}},
		{`ccache clang -I/a`, []string{"ccache", "clang", "-I/a"}},
		{`-I'/opt/icu 4c/include'`, []string{"-I/opt/icu 4c/include"}},
		{"", nil},
		{"   ", nil},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := shellSplit(c.in)
			if !equalStrings(got, c.want) {
				t.Errorf("shellSplit(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
