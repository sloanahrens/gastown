package metrics

import (
	"context"
	"reflect"
	"testing"
)

func TestRunnerChildEnvPassesOnlyAllowlisted(t *testing.T) {
	t.Parallel()
	host := map[string]string{"PATH": "/usr/bin", "HOME": "/home/x", "DEEPSEEK_API_KEY": "secret", "GT_TOKEN": "t"}
	lookup := func(name string) (string, bool) {
		v, ok := host[name]
		return v, ok
	}
	tests := []struct {
		name  string
		allow []string
		want  []string
	}{
		{"allowlisted set variables only", []string{"PATH", "HOME"}, []string{"PATH=/usr/bin", "HOME=/home/x"}},
		{"unset allowlisted variable is absent", []string{"PATH", "LANG"}, []string{"PATH=/usr/bin"}},
		{"empty allowlist is an empty environment", nil, []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := childEnv(tc.allow, lookup)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("childEnv(%v) = %v, want %v (secrets in the host environment must not reach a child)", tc.allow, got, tc.want)
			}
		})
	}
}

func TestRunnerOSRunnerRejectsEmptyArgv(t *testing.T) {
	t.Parallel()
	if _, err := OSRunner(DefaultAllowEnv)(context.Background(), nil); err == nil {
		t.Fatal("OSRunner ran an empty argv without an error")
	}
}
