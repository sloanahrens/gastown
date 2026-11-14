package daemon

import (
	"bytes"
	"testing"
)

// TestEnvVarEmptyLiteralIsRendered: an environment entry with a literal value
// keeps it in the manifest even when the value is empty, so the container reads
// a variable set to "" rather than one it never set; an entry sourced from a
// Secret carries no value at all. Each case compares the whole container manifest
// byte for byte, so a literal with a value and a Secret entry render as they did
// before the empty-literal case was added.
func TestEnvVarEmptyLiteralIsRendered(t *testing.T) {
	t.Parallel()
	const head = "name: c\nimage: i\nenv:\n"
	cases := []struct {
		name string
		env  EnvVar
		want string
	}{
		{
			name: "literal set to the empty string",
			env:  EnvVar{Name: "GT_EMPTY", Value: ""},
			want: head + "  - name: GT_EMPTY\n    value: \"\"\n",
		},
		{
			name: "literal with a value",
			env:  EnvVar{Name: "GT_SET", Value: "v"},
			want: head + "  - name: GT_SET\n    value: v\n",
		},
		{
			name: "Secret-backed entry",
			env:  EnvVar{Name: "GT_TOKEN", ValueFrom: &EnvVarSource{SecretKeyRef: &SecretKeySelector{Name: "s", Key: "k"}}},
			want: head + "  - name: GT_TOKEN\n    valueFrom:\n      secretKeyRef:\n        name: s\n        key: k\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := YAML(&buf, Container{Name: "c", Image: "i", Env: []EnvVar{tc.env}}); err != nil {
				t.Fatalf("encoding the container: %v", err)
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("container manifest = %q, want %q", got, tc.want)
			}
		})
	}
}
