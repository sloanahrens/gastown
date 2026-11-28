package presubmit

import (
	"reflect"
	"testing"
)

func TestRedBaseGoArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		dir   string
		names []string
		want  []string
	}{
		{"two names give the anchored alternation and the package path", "internal/presubmit", []string{"TestA", "TestB"}, []string{"test", "-json", "-run", "^(TestA|TestB)$", "-count=1", "./internal/presubmit"}},
		{"the root directory is .", ".", []string{"TestA"}, []string{"test", "-json", "-run", "^(TestA)$", "-count=1", "."}},
		{"no names gives nil", "internal/presubmit", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RedBaseGoArgs(tt.dir, tt.names); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("RedBaseGoArgs(%q, %v) = %v, want %v", tt.dir, tt.names, got, tt.want)
			}
		})
	}
}
