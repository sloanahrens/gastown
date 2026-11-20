package presubmit

import "testing"

func TestIntegrationTagged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "the tag alone",
			src:  "//go:build integration\n\npackage e2e\n",
			want: true,
		},
		{
			name: "the tag negated",
			src:  "//go:build !integration\n\npackage unit\n",
			want: false,
		},
		{
			name: "the tag and a platform tag",
			src:  "//go:build integration && linux\n\npackage e2e\n",
			want: true,
		},
		{
			name: "the tag or a platform tag",
			src:  "//go:build linux || integration\n\npackage e2e\n",
			want: false,
		},
		{
			name: "no constraint",
			src:  "package e2e\n",
			want: false,
		},
		{
			name: "a constraint that does not parse",
			src:  "//go:build integration &&\n\npackage e2e\n",
			want: true,
		},
		{
			name: "a go:build line after the package clause",
			src:  "package e2e\n\n//go:build integration\n",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IntegrationTagged([]byte(tt.src)); got != tt.want {
				t.Errorf("IntegrationTagged(%q) = %v, want %v", tt.src, got, tt.want)
			}
		})
	}
}
