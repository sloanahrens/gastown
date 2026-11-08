package cmd

import (
	"bytes"
	"io"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/templates"
)

const composeTestTownStateVolume = "gt-live-town-state"

// --town-state-volume reaches the render: the daemon mounts the external volume
// at the town root instead of the Mac's bind, and the command prints the
// rollback line after writing (gt-yjzepk.6).
func TestDaemonComposeTownStateWritesTheVolumeAndRollback(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	r.flags.townStateVolume = composeTestTownStateVolume
	if err := runDaemonCompose(r.flags, r.deps); err != nil {
		t.Fatalf("runDaemonCompose: %v", err)
	}
	got, ok := r.disk[r.composePath()]
	if !ok {
		t.Fatalf("no compose file written at %s", r.composePath())
	}
	for _, want := range []string{
		"      - type: volume\n        source: " + composeTestTownStateVolume + "\n        target: \"" + composeTestTown + "\"\n",
		"  " + composeTestTownStateVolume + ":\n    external: true\n",
	} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("compose file does not carry %q:\n%s", want, got)
		}
	}
	if bad := "      - type: bind\n        source: \"" + composeTestTown + "\"\n        target: \"" + composeTestTown + "\"\n"; bytes.Contains(got, []byte(bad)) {
		t.Errorf("the compose file still binds the Mac's town root:\n%s", got)
	}
	if out := r.out.String(); !strings.Contains(out, "render again without --town-state-volume and recreate the service") {
		t.Errorf("output %q does not carry the one-line rollback", out)
	}
}

// --print prints the render alone: the rollback line follows a write, so it is
// not part of the file on stdout.
func TestDaemonComposeTownStatePrintCarriesNoRollback(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	r.flags.print = true
	r.flags.townStateVolume = composeTestTownStateVolume
	if err := runDaemonCompose(r.flags, r.deps); err != nil {
		t.Fatalf("runDaemonCompose: %v", err)
	}
	out := r.out.String()
	if strings.Contains(out, "Rollback:") {
		t.Errorf("--print output carries the rollback line:\n%s", out)
	}
	if !strings.Contains(out, "source: "+composeTestTownStateVolume) {
		t.Errorf("--print output does not carry the volume:\n%s", out)
	}
	if _, ok := r.disk[r.composePath()]; ok {
		t.Errorf("--print left a file at %s", r.composePath())
	}
}

// The volume name and a session-owned mount are each refused before any file is
// written, naming the flag or the path (gt-yjzepk.6).
func TestDaemonComposeTownStateRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(r *composeRig)
		want   string
	}{
		{
			name:   "a name that is not a volume name",
			mutate: func(r *composeRig) { r.flags.townStateVolume = "gt live state" },
			want:   "--town-state-volume",
		},
		{
			name: "a mount under a session-owned town-state entry",
			mutate: func(r *composeRig) {
				r.flags.townStateVolume = composeTestTownStateVolume
				meta := composeTestTown + "/gastown/polecats/cheedo/om-token-meta.json"
				r.deps.daemonConfig = func(string) (*config.DaemonPatrolConfig, error) {
					return &config.DaemonPatrolConfig{
						ContainerNameSetting:  composeTestContainer,
						ComposeProjectSetting: composeTestProject,
						OMTokenMetaSetting:    meta,
					}, nil
				}
			},
			want: composeTestTown + "/gastown/polecats/cheedo/om-token-meta.json",
		},
		{
			name: "the flag given blank",
			mutate: func(r *composeRig) {
				r.flags.townStateVolume, r.flags.townStateVolumeSet = "  ", true
			},
			want: "was given blank",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newComposeRig(t)
			tc.mutate(r)
			err := runDaemonCompose(r.flags, r.deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
			if len(r.wrote) != 0 {
				t.Errorf("a refusal wrote %v", r.wrote)
			}
		})
	}
}

// The command replaces only a file it wrote. A file without the generated
// header is the operator's and is left alone without --force, an unreadable
// file is refused because it cannot be told apart from theirs, and a file with
// the header, or no file at all, is written.
func TestDaemonComposeWriteOverwritesOnlyItsOwnFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		existing  []byte
		readErr   error
		force     bool
		wantErr   string
		wantWrote bool
	}{
		{
			name:      "no file at the path",
			readErr:   fs.ErrNotExist,
			wantWrote: true,
		},
		{
			name:      "a file gt wrote",
			existing:  []byte(templates.DaemonComposeHeader + "services:\n"),
			wantWrote: true,
		},
		{
			name:     "a hand-written file",
			existing: []byte("services: {}\n"),
			wantErr:  "was not written by gt",
		},
		{
			name:      "a hand-written file with --force",
			existing:  []byte("services: {}\n"),
			force:     true,
			wantWrote: true,
		},
		{
			name:    "a file that cannot be read",
			readErr: fs.ErrPermission,
			wantErr: "cannot read the existing compose file",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newComposeRig(t)
			r.flags.force = tc.force
			r.deps.files.read = func(string) ([]byte, error) {
				if tc.readErr != nil {
					return nil, tc.readErr
				}
				return tc.existing, nil
			}
			err := runDaemonCompose(r.flags, r.deps)
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				if len(r.wrote) != 0 || len(r.privateWrote) != 0 {
					t.Errorf("a refusal wrote %v (private %v); the derived configs wait for the compose file", r.wrote, r.privateWrote)
				}
				if !strings.Contains(err.Error(), "--force") {
					t.Errorf("error = %v, want it to name --force", err)
				}
			case err != nil:
				t.Fatalf("runDaemonCompose: %v", err)
			}
			if tc.wantWrote && !slices.Contains(r.wrote, r.composePath()) {
				t.Errorf("wrote %v, want the compose file %s", r.wrote, r.composePath())
			}
		})
	}
}

// The flag is parsed by the command itself, and its Changed wiring is what marks
// a blank value as given rather than as the flag's default.
func TestDaemonComposeTownStateFlagIsWired(t *testing.T) {
	t.Parallel()
	var got daemonComposeFlags
	c := newDaemonComposeCmd(func(_ io.Writer, f daemonComposeFlags) error {
		got = f
		return nil
	})
	c.SetArgs([]string{"write", "--image", daemonimage.ImageTag(composeTestRev), "--town-state-volume", composeTestTownStateVolume})
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got.townStateVolume != composeTestTownStateVolume || !got.townStateVolumeSet {
		t.Errorf("parsed %q (set %v), want %q and set", got.townStateVolume, got.townStateVolumeSet, composeTestTownStateVolume)
	}
}
