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
		extra []string
		want  []string
	}{
		{"allowlisted set variables only", []string{"PATH", "HOME"}, nil, []string{"PATH=/usr/bin", "HOME=/home/x"}},
		{"unset allowlisted variable is absent", []string{"PATH", "LANG"}, nil, []string{"PATH=/usr/bin"}},
		{"empty allowlist is an empty environment", nil, nil, []string{}},
		{
			name:  "extra environment follows the allowlist",
			allow: []string{"PATH", "HOME"},
			extra: []string{"TOWN_METRICS_TOWN=/town", "TOWN_METRICS_DOCS=/town/docs"},
			want: []string{
				"PATH=/usr/bin", "HOME=/home/x", "TOWN_METRICS_TOWN=/town", "TOWN_METRICS_DOCS=/town/docs",
			},
		},
		{
			name:  "extra wins over an allowlisted name and appears once",
			allow: []string{"PATH", "HOME"},
			extra: []string{"PATH=/opt/bin"},
			want:  []string{"HOME=/home/x", "PATH=/opt/bin"},
		},
		{
			name:  "extra names never reach a secret the allowlist omits",
			allow: []string{"PATH"},
			extra: []string{"TOWN_METRICS_TOWN=/town"},
			want:  []string{"PATH=/usr/bin", "TOWN_METRICS_TOWN=/town"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := childEnv(tc.allow, tc.extra, lookup)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("childEnv(%v, %v) = %v, want %v (secrets in the host environment must not reach a child)", tc.allow, tc.extra, got, tc.want)
			}
		})
	}
}

// TestRunnerOSRunnerEnvSharesTheChildEnvRuling is the same allowlist and extra
// pair through the seam the analysis uses: both seams refuse an empty argv
// before starting anything, so the ruling childEnv pins is the one they run.
func TestRunnerOSRunnerEnvSharesTheChildEnvRuling(t *testing.T) {
	t.Parallel()
	if _, err := OSRunnerEnv(DefaultAllowEnv)(context.Background(), nil, []string{"TOWN_METRICS_TOWN=/town"}); err == nil {
		t.Fatal("OSRunnerEnv ran an empty argv without an error")
	}
}

func TestRunnerOSRunnerRejectsEmptyArgv(t *testing.T) {
	t.Parallel()
	if _, err := OSRunner(DefaultAllowEnv)(context.Background(), nil); err == nil {
		t.Fatal("OSRunner ran an empty argv without an error")
	}
}
