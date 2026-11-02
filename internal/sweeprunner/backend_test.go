package sweeprunner

import "testing"

// TestStepMissingSource holds the one tolerance the plan has: a step that
// declares its source may be absent tolerates only a failure whose output
// names that absence, and a step that does not declare it tolerates nothing
// (gt-4lj2q).
func TestStepMissingSource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		step   Step
		output string
		want   bool
	}{
		{
			name:   "an absent source the step tolerates",
			step:   Step{MissingSourceOK: true},
			output: "error: tar: /out/logs: Cannot stat: No such file or directory\n",
			want:   true,
		},
		{
			name:   "another failure of a tolerant step",
			step:   Step{MissingSourceOK: true},
			output: "Error from server (Forbidden): pods \"reader\" is forbidden\n",
			want:   false,
		},
		{
			name:   "an absent source a step does not tolerate",
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
