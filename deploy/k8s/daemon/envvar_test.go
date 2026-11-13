package daemon

import (
	"bytes"
	"strings"
	"testing"
)

// TestEnvVarEmptyLiteralIsRendered: an environment entry with a literal value
// keeps it in the manifest even when the value is empty, so the container reads
// a variable set to "" rather than one it never set; an entry sourced from a
// Secret carries no value at all.
func TestEnvVarEmptyLiteralIsRendered(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  EnvVar
		want string
	}{
		{
			name: "literal set to the empty string",
			env:  EnvVar{Name: "GT_EMPTY", Value: ""},
			want: "  - name: GT_EMPTY\n    value: \"\"\n",
		},
		{
			name: "literal with a value",
			env:  EnvVar{Name: "GT_SET", Value: "v"},
			want: "  - name: GT_SET\n    value: v\n",
		},
		{
			name: "Secret-backed entry",
			env:  EnvVar{Name: "GT_TOKEN", ValueFrom: &EnvVarSource{SecretKeyRef: &SecretKeySelector{Name: "s", Key: "k"}}},
			want: "  - name: GT_TOKEN\n    valueFrom:\n      secretKeyRef:\n        name: s\n        key: k\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := YAML(&buf, Container{Name: "c", Image: "i", Env: []EnvVar{tc.env}}); err != nil {
				t.Fatalf("encoding the container: %v", err)
			}
			if got := buf.String(); !strings.Contains(got, "env:\n"+tc.want) {
				t.Errorf("container env = %q, want it to hold %q", got, tc.want)
			}
		})
	}
}
