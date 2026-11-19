package beads

import (
	"slices"
	"testing"
)

// WithBondRootOnly adds --root-only to bd mol bond's argv; without it Bond
// runs the argv it has always run.
func TestBondRootOnlyArgv(t *testing.T) {
	t.Parallel()
	const answer = `{"id":"gt-1"}`
	tests := []struct {
		name string
		opts []Option
		want string
	}{
		{"default", nil, "mol bond mol-x gt-1 --json --ephemeral"},
		{"root only", []Option{WithBondRootOnly()}, "mol bond mol-x gt-1 --json --ephemeral --root-only"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRecorder(func([]string) reply { return reply{stdout: answer} })
			b := newBeads(applyOptions(beadsFields{
				workDir: t.TempDir(), isolated: true, noRoute: true, exec: r.exec,
			}, tc.opts))

			out, err := b.Bond("mol-x", "gt-1", nil)
			if err != nil {
				t.Fatalf("Bond: %v", err)
			}
			if string(out) != answer {
				t.Errorf("Bond = %q, want bd's stdout unchanged", out)
			}
			if got, want := r.argvs(), []string{tc.want}; !slices.Equal(got, want) {
				t.Errorf("argv = %v, want %v", got, want)
			}
		})
	}
}
