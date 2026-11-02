package sweeprunner

import "testing"

// TestStepMissingSource holds the one tolerance the plan has: a step that names
// a source tolerates only a failure whose output says that source is not there,
// and a step that names none tolerates nothing (gt-4lj2q).
func TestStepMissingSource(t *testing.T) {
	t.Parallel()
	const logs = "/out/logs"
	for _, tc := range []struct {
		name   string
		step   Step
		output string
		want   bool
	}{
		{
			name:   "the declared source is absent",
			step:   Step{MissingSourcePath: logs},
			output: "error: tar: /out/logs: Cannot stat: No such file or directory\n",
			want:   true,
		},
		{
			name:   "another file is absent, not the declared source",
			step:   Step{MissingSourcePath: logs},
			output: "error: open /root/.kube/config: no such file or directory\n",
			want:   false,
		},
		{
			name:   "a path that starts with the declared source",
			step:   Step{MissingSourcePath: logs},
			output: "error: tar: /out/logs-old: Cannot stat: No such file or directory\n",
			want:   false,
		},
		{
			name:   "a path that ends with the declared source",
			step:   Step{MissingSourcePath: logs},
			output: "error: tar: /var/out/logs: Cannot stat: No such file or directory\n",
			want:   false,
		},
		{
			name:   "a path inside the declared source",
			step:   Step{MissingSourcePath: logs},
			output: "error: tar: /out/logs/integration: Cannot stat: No such file or directory\n",
			want:   false,
		},
		{
			name:   "another failure of a step that names a source",
			step:   Step{MissingSourcePath: logs},
			output: "Error from server (Forbidden): pods \"reader\" is forbidden\n",
			want:   false,
		},
		{
			name:   "an absent source a step does not name",
			step:   Step{},
			output: "error: /out/result.json: no such file or directory\n",
			want:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.step.MissingSource([]byte(tc.output)); got != tc.want {
				t.Errorf("MissingSource(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}
