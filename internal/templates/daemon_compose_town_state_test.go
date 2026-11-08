package templates_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/townstate"
)

// townStateVolume is the external volume the town-state render mounts at the
// town root. The name is the test's, not the renderer's: the flag carries it.
const townStateVolume = "gt-live-town-state"

// townStateSpec is the live spec with the volume in place of the Mac's bind, and
// the real manifest behind the render's session-owned seam.
func townStateSpec() templates.ComposeSpec {
	s := liveSpec()
	s.TownStateVolume = townStateVolume
	s.SessionOwned = func(rel string) (string, bool) {
		entry, ok := townstate.Default().SessionOwned(rel)
		return entry.Path, ok
	}
	return s
}

// The town-state render is a contract with the rehearsal's compose project, so
// it is compared byte for byte to its own golden: the volume is mounted at the
// town root, declared external, and no bind of the Mac's town root is left
// (gt-yjzepk.6).
func TestDaemonComposeTownStateVolumeGolden(t *testing.T) {
	t.Parallel()
	out, f := render(t, townStateSpec())

	golden, err := os.ReadFile(filepath.Join("testdata", "daemon-compose-town-state.golden.yml"))
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if out != string(golden) {
		t.Errorf("rendered file differs from testdata/daemon-compose-town-state.golden.yml\ngot:\n%s\nwant:\n%s", out, golden)
	}
	// Rendering twice is the same file: nothing in the render is a timestamp,
	// a map order or a path the host happened to hold.
	if again, _ := render(t, townStateSpec()); again != out {
		t.Errorf("the second render differs from the first\ngot:\n%s\nwant:\n%s", again, out)
	}

	townRoot := townStateSpec().TownRoot
	var mount composeVolume
	found := false
	for _, v := range f.Services["daemon"].Volumes {
		if v.Type == "bind" && v.Source == townRoot {
			t.Errorf("the daemon still binds the Mac's town root: %+v", v)
		}
		if v.Target == townRoot {
			mount, found = v, true
		}
	}
	if !found {
		t.Fatalf("the daemon mounts nothing at the town root %q: %+v", townRoot, f.Services["daemon"].Volumes)
	}
	if mount.Type != "volume" || mount.Source != townStateVolume {
		t.Errorf("the town root mount = %+v, want the volume %q", mount, townStateVolume)
	}
	decl, ok := f.Volumes[townStateVolume]
	if !ok {
		t.Fatalf("volumes = %v, want %q declared", f.Volumes, townStateVolume)
	}
	if !decl.External {
		t.Errorf("volume %q is not external; a compose down would remove the daemon's state", townStateVolume)
	}
}

// Without the flag the render is today's file, byte for byte: that is the
// rollback to the Mac's bind mount (gt-yjzepk.6).
func TestDaemonComposeWithoutTheFlagIsUnchanged(t *testing.T) {
	t.Parallel()
	out, _ := render(t, liveSpec())

	golden, err := os.ReadFile(filepath.Join("testdata", "daemon-compose.golden.yml"))
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if out != string(golden) {
		t.Errorf("the render without --town-state-volume differs from the golden\ngot:\n%s\nwant:\n%s", out, golden)
	}
}

// The volume name is the file's top-level key, so a name docker or compose
// would refuse, a scratch spec, and the engine's own volume are refused before
// anything is written (gt-yjzepk.6).
func TestDaemonComposeTownStateRefusesTheVolume(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		spec func() templates.ComposeSpec
		want string
	}{
		{
			name: "a name with a space",
			spec: func() templates.ComposeSpec {
				s := townStateSpec()
				s.TownStateVolume = "gt live state"
				return s
			},
			want: "--town-state-volume",
		},
		{
			name: "a name starting with a dot",
			spec: func() templates.ComposeSpec {
				s := townStateSpec()
				s.TownStateVolume = ".hidden"
				return s
			},
			want: "--town-state-volume",
		},
		{
			name: "a single-character name",
			spec: func() templates.ComposeSpec {
				s := townStateSpec()
				s.TownStateVolume = "v"
				return s
			},
			want: "--town-state-volume",
		},
		{
			name: "a scratch spec",
			spec: func() templates.ComposeSpec {
				s := townStateSpec()
				s.Scratch = true
				return s
			},
			want: "scratch spec",
		},
		{
			name: "the engine's data volume",
			spec: func() templates.ComposeSpec {
				s := townStateSpec()
				s.TownStateVolume = engineVolume
				return s
			},
			want: "engine's data volume",
		},
		{
			name: "a volume with no manifest behind the seam",
			spec: func() templates.ComposeSpec {
				s := townStateSpec()
				s.SessionOwned = nil
				return s
			},
			want: "without SessionOwned",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := templates.RenderDaemonCompose(tc.spec()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A bind under the town root is kept when the manifest gives its path to the
// daemon: the daemon's own state lives in the volume, and a daemon-owned mount
// beside it is still the daemon's (gt-yjzepk.6).
func TestDaemonComposeTownStateKeepsDaemonOwnedMounts(t *testing.T) {
	t.Parallel()
	s := townStateSpec()
	s.OMTokenMetaFile = s.TownRoot + "/.runtime/usage/om-token-meta.json"
	s.KubeconfigFiles = []string{s.TownRoot + "/.runtime/container/kubeconfig"}

	out, f := render(t, s)
	// KUBECONFIG names the file through the town, which the volume now carries;
	// the token meta file is a bind of its own beside it.
	for _, want := range []string{s.OMTokenMetaFile, s.KubeconfigFiles[0]} {
		if !strings.Contains(out, want) {
			t.Errorf("the render does not carry the daemon-owned path %s:\n%s", want, out)
		}
	}
	if len(f.Services["daemon"].Volumes) != 2 {
		t.Errorf("daemon volumes = %+v, want the town volume and the token meta file", f.Services["daemon"].Volumes)
	}
}

// A bind under a session-owned entry is a refusal, not a silent mount over the
// volume. The check runs on resolved paths, so a link whose target is a
// session's directory is refused even when its own name looks daemon-owned
// (gt-yjzepk.6).
func TestDaemonComposeTownStateRefusesSessionOwnedMounts(t *testing.T) {
	t.Parallel()
	t.Run("a rig's polecats tree", func(t *testing.T) {
		t.Parallel()
		s := townStateSpec()
		s.OMTokenMetaFile = s.TownRoot + "/gastown/polecats/wren/om-token-meta.json"
		_, err := templates.RenderDaemonCompose(s)
		if err == nil || !strings.Contains(err.Error(), s.OMTokenMetaFile) {
			t.Fatalf("error = %v, want the mount path %s named", err, s.OMTokenMetaFile)
		}
		if err != nil && !strings.Contains(err.Error(), "<rig>/polecats") {
			t.Errorf("error = %v, want the session-owned entry named", err)
		}
	})
}

// A town root or a mount that cannot be resolved is refused: the check never
// reads "cannot compare" as "outside the town", because the path a link hides
// could be a session's (gt-yjzepk.6).
func TestDaemonComposeTownStateRefusesAnUnresolvablePath(t *testing.T) {
	t.Parallel()
	link := filepath.Join(t.TempDir(), "dangling")
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), link); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		spec func() templates.ComposeSpec
		want string
	}{
		{
			name: "the town root",
			spec: func() templates.ComposeSpec {
				s := townStateSpec()
				s.TownRoot = link
				s.OMTokenMetaFile = "/run/gt/om-token-meta.json"
				return s
			},
			want: link,
		},
		{
			name: "a mount",
			spec: func() templates.ComposeSpec {
				s := townStateSpec()
				s.OMTokenMetaFile = link
				return s
			},
			want: link,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := templates.RenderDaemonCompose(tc.spec())
			if err == nil || !strings.Contains(err.Error(), "cannot be resolved") {
				t.Fatalf("error = %v, want a path that cannot be resolved refused", err)
			}
		})
	}
}

// A link whose target is a session's directory is refused even when its own
// name looks daemon-owned (gt-yjzepk.6).
func TestDaemonComposeTownStateRefusesASessionLink(t *testing.T) {
	t.Parallel()
	t.Run("a link into a session's directory", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		session := filepath.Join(town, "gastown", "crew", "dipper")
		if err := os.MkdirAll(session, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "daemon-state")
		if err := os.Symlink(session, link); err != nil {
			t.Fatal(err)
		}
		s := townStateSpec()
		s.TownRoot = town
		s.OMTokenMetaFile = link
		_, err := templates.RenderDaemonCompose(s)
		if err == nil || !strings.Contains(err.Error(), link) {
			t.Fatalf("error = %v, want the link %s named", err, link)
		}
	})
}

// A mount that resolves to the town root is refused: the volume is mounted
// there, so a bind of the same path would mount over it. The check runs on
// resolved paths, so a link to the town root is refused too (gt-i0h1x6).
func TestDaemonComposeTownStateRefusesAMountAtTheTownRoot(t *testing.T) {
	t.Parallel()
	t.Run("the town root itself", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		s := townStateSpec()
		s.TownRoot = town
		s.OMTokenMetaFile = town
		_, err := templates.RenderDaemonCompose(s)
		if err == nil || !strings.Contains(err.Error(), town) {
			t.Fatalf("error = %v, want the mount %s refused", err, town)
		}
		if err != nil && !strings.Contains(err.Error(), "town root") {
			t.Errorf("error = %v, want the town root named", err)
		}
	})
	t.Run("a link to the town root", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		link := filepath.Join(t.TempDir(), "town-link")
		if err := os.Symlink(town, link); err != nil {
			t.Fatal(err)
		}
		s := townStateSpec()
		s.TownRoot = town
		s.OMTokenMetaFile = link
		_, err := templates.RenderDaemonCompose(s)
		if err == nil || !strings.Contains(err.Error(), link) {
			t.Fatalf("error = %v, want the link %s refused", err, link)
		}
	})
}

// A volume name that is a bare YAML number or bool is still written as the
// string the host's volume has: the file names it quoted, in the mount and in
// the volumes section, and it reads back as that name (gt-i0h1x6).
func TestDaemonComposeTownStateQuotesTheVolumeName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"123", "true"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := townStateSpec()
			s.TownStateVolume = name
			out, f := render(t, s)
			if want := "source: \"" + name + "\"\n"; !strings.Contains(out, want) {
				t.Errorf("the render does not mount the volume as the string %q:\n%s", name, out)
			}
			if want := "  \"" + name + "\":\n    external: true\n"; !strings.Contains(out, want) {
				t.Errorf("the volumes section does not declare %q as a string:\n%s", name, out)
			}
			if decl, ok := f.Volumes[name]; !ok || !decl.External {
				t.Errorf("volumes = %v, want %q declared external", f.Volumes, name)
			}
			var found bool
			for _, v := range f.Services["daemon"].Volumes {
				if v.Target != s.TownRoot {
					continue
				}
				found = true
				if v.Type != "volume" || v.Source != name {
					t.Errorf("the town root mount = %+v, want the volume %q", v, name)
				}
			}
			if !found {
				t.Errorf("the daemon mounts nothing at the town root:\n%s", out)
			}
		})
	}
}
