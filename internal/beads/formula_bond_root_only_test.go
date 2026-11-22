package beads

import (
	"os"
	"path/filepath"
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

// TestBondRootOnlySurvivesDerivedWrappers: each wrapper Beads derives from
// another — ActingAs, ForAgentBead and the two routed copies, pinnedToBeadsDir
// and forIssueID — carries the bondRootOnly option, so a caller that builds the
// wrapper with WithBondRootOnly and then derives still bonds root-only
// (gt-f2kekw).
func TestBondRootOnlySurvivesDerivedWrappers(t *testing.T) {
	t.Parallel()
	const answer = `{"id":"gt-1"}`

	// A routed wrapper is what ForAgentBead and forIssueID derive from: the
	// yy- route resolves to a rig directory that exists here.
	townRoot, rigDir := newTestTown(t)
	if err := os.MkdirAll(filepath.Join(townRoot, "checked-out-rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteRoutes(filepath.Join(townRoot, ".beads"), []Route{{Prefix: "yy-", Path: "checked-out-rig"}}); err != nil {
		t.Fatal(err)
	}

	derivations := []struct {
		name   string
		derive func(*Beads) *Beads
	}{
		{"ActingAs", func(b *Beads) *Beads { return b.ActingAs("capable") }},
		{"ForAgentBead", func(b *Beads) *Beads { return b.ForAgentBead() }},
		{"pinnedToBeadsDir", func(b *Beads) *Beads {
			return b.pinnedToBeadsDir(filepath.Join(townRoot, "checked-out-rig", ".beads"))
		}},
		{"forIssueID", func(b *Beads) *Beads { return b.forIssueID("yy-abc123") }},
	}
	options := []struct {
		name     string
		opts     []Option
		wantArgv string
	}{
		{"default", nil, "mol bond mol-x gt-1 --json --ephemeral"},
		{"root only", []Option{WithBondRootOnly()}, "mol bond mol-x gt-1 --json --ephemeral --root-only"},
	}
	for _, d := range derivations {
		for _, o := range options {
			t.Run(d.name+"/"+o.name, func(t *testing.T) {
				t.Parallel()
				r := newRecorder(func([]string) reply { return reply{stdout: answer} })
				b := newBeads(applyOptions(beadsFields{
					workDir: rigDir, isolated: true, exec: r.exec,
				}, o.opts))

				derived := d.derive(b)
				if derived == b {
					t.Fatalf("%s returned the wrapper unchanged, so the rows below prove nothing", d.name)
				}
				if _, err := derived.Bond("mol-x", "gt-1", nil); err != nil {
					t.Fatalf("Bond: %v", err)
				}
				if got, want := r.argvs(), []string{o.wantArgv}; !slices.Equal(got, want) {
					t.Errorf("after %s, Bond argv = %v, want %v", d.name, got, want)
				}
			})
		}
	}
}
