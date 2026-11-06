package templates_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
)

// secretConfigDir makes a config directory holding the named files, each with
// the same placeholder bytes and the manifest's usual mode, so a test can prove
// no render carried the bytes.
func secretConfigDir(t *testing.T, names ...string) (dir, marker string) {
	t.Helper()
	dir = t.TempDir()
	marker = "placeholder-contents-never-read"
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(marker+"\n"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	return dir, marker
}

// secretEntry is one manifest entry for the renderer, the shape the command
// passes from the manifest it parsed.
func secretEntry(name string, required bool) templates.SecretFile {
	return templates.SecretFile{Name: name, Consumer: "internal/daemon (test)", Mode: "0600", Required: required}
}

// secretFSFake is the spec's SecretFS seam over the host's filesystem, with an
// Lstat or Stat that can be made to fail for one file and a ReadFile that always
// does, so the branches no real file reaches are driven and a content read is
// caught rather than performed.
type secretFSFake struct {
	// failName is the file whose call fails; "" fails none, so the config
	// directory's own Stat still succeeds.
	failName          string
	lstatErr, statErr error
	calls             []string
}

func (f *secretFSFake) Lstat(name string) (fs.FileInfo, error) {
	f.calls = append(f.calls, "Lstat "+name)
	if f.lstatErr != nil && filepath.Base(name) == f.failName {
		return nil, f.lstatErr
	}
	return os.Lstat(name)
}

func (f *secretFSFake) Stat(name string) (fs.FileInfo, error) {
	f.calls = append(f.calls, "Stat "+name)
	if f.statErr != nil && filepath.Base(name) == f.failName {
		return nil, f.statErr
	}
	return os.Stat(name)
}

func (f *secretFSFake) ReadFile(name string) ([]byte, error) {
	f.calls = append(f.calls, "ReadFile "+name)
	return nil, errors.New("the fake refuses every content read")
}

// With a secrets manifest the daemon service mounts each listed file read-only
// at its own path and at the container's XDG path, and mounts no directory of
// the config tree. A file in that directory the manifest does not list stays on
// the host.
func TestDaemonComposeSecretsMountsOnlyListedFiles(t *testing.T) {
	t.Parallel()
	const listed, second, other = "forgejo-landing.env", "jev.env", "unlisted.env"
	dir, marker := secretConfigDir(t, listed, second, other)
	if err := os.Mkdir(filepath.Join(dir, "topics"), 0o700); err != nil {
		t.Fatalf("making a directory in the config dir: %v", err)
	}

	s := liveSpec()
	s.ConfigDir = dir
	s.Secrets = []templates.SecretFile{secretEntry(listed, true), secretEntry(second, false)}
	out, f := render(t, s)

	wantTargets := map[string]bool{}
	var want []composeVolume
	for _, name := range []string{listed, second} {
		wantTargets[filepath.Join(dir, name)] = true
		wantTargets[filepath.Join(templates.LiveConfigDirPath, name)] = true
		want = append(want,
			composeVolume{Type: "bind", Source: filepath.Join(dir, name), Target: filepath.Join(dir, name), ReadOnly: true},
			composeVolume{Type: "bind", Source: filepath.Join(dir, name), Target: filepath.Join(templates.LiveConfigDirPath, name), ReadOnly: true},
		)
	}
	for _, wantMount := range want {
		found := false
		for _, v := range f.Services["daemon"].Volumes {
			if v == wantMount {
				found = true
			}
		}
		if !found {
			t.Errorf("daemon volumes = %+v, want the read-only mount %+v", f.Services["daemon"].Volumes, wantMount)
		}
	}
	for _, v := range f.Services["daemon"].Volumes {
		if v.Source == dir {
			t.Errorf("the daemon mounts the config directory %s, want only the files the manifest lists", dir)
		}
		if inside(dir, v.Source) && v.Source != filepath.Join(dir, listed) && v.Source != filepath.Join(dir, second) {
			t.Errorf("the daemon mounts %s, which no manifest entry lists", v.Source)
		}
		if inside(dir, v.Source) && !wantTargets[v.Target] {
			t.Errorf("%s is mounted at %s, want its own path or the XDG path", v.Source, v.Target)
		}
	}
	if !strings.Contains(out, `XDG_CONFIG_HOME: "/run/gt/xdg"`) {
		t.Errorf("the compose lacks XDG_CONFIG_HOME, so a setting naming the XDG path finds nothing:\n%s", out)
	}
	if strings.Contains(out, marker) {
		t.Error("the rendered compose carries a manifest file's contents")
	}
}

// A required file that is not present is refused at render time, naming the
// entry, before any file is written. An optional one is skipped with a note in
// the file and no mount. An entry that names a directory is refused rather than
// mounted as one, and so is an entry the seam cannot stat.
func TestDaemonComposeSecretsRefusesAMissingRequiredFile(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(configDir, "topics"), 0o700); err != nil {
		t.Fatalf("making the directory entry: %v", err)
	}

	t.Run("a required file that is not present", func(t *testing.T) {
		t.Parallel()
		s := liveSpec()
		s.ConfigDir = configDir
		s.Secrets = []templates.SecretFile{secretEntry("absent.env", true)}

		out, err := templates.RenderDaemonCompose(s)
		if err == nil {
			t.Fatalf("RenderDaemonCompose with a missing required file = a render, want a refusal:\n%s", out)
		}
		for _, want := range []string{`"absent.env"`, "required", filepath.Join(configDir, "absent.env")} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to name %q", err, want)
			}
		}
	})

	t.Run("an optional file that is not present is skipped with a note", func(t *testing.T) {
		t.Parallel()
		s := liveSpec()
		s.ConfigDir = configDir
		s.Secrets = []templates.SecretFile{secretEntry("absent.env", false)}

		out, f := render(t, s)
		if !strings.Contains(out, `# secrets manifest: optional file "absent.env" is not present, so it is not mounted`) {
			t.Errorf("the render carries no note that the optional file was skipped:\n%s", out)
		}
		for _, v := range f.Services["daemon"].Volumes {
			if v.Source == filepath.Join(configDir, "absent.env") {
				t.Errorf("the daemon mounts the absent optional file %s", v.Source)
			}
		}
		if len(f.Services["daemon"].Volumes) == 0 {
			t.Error("the render carries no volumes at all")
		}
	})

	t.Run("an entry that names a directory", func(t *testing.T) {
		t.Parallel()
		s := liveSpec()
		s.ConfigDir = configDir
		s.Secrets = []templates.SecretFile{secretEntry("topics", false)}

		out, err := templates.RenderDaemonCompose(s)
		if err == nil {
			t.Fatalf("RenderDaemonCompose with a directory entry = a render, want a refusal:\n%s", out)
		}
		for _, want := range []string{`"topics"`, "directory"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to say %q", err, want)
			}
		}
	})

	t.Run("a config directory that cannot be read", func(t *testing.T) {
		t.Parallel()
		s := liveSpec()
		s.ConfigDir = filepath.Join(configDir, "gone")
		s.Secrets = []templates.SecretFile{secretEntry("absent.env", false)}

		out, err := templates.RenderDaemonCompose(s)
		if err == nil {
			t.Fatalf("RenderDaemonCompose over a missing config directory = a render, want a refusal:\n%s", out)
		}
		for _, want := range []string{"config directory", s.ConfigDir} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to name %q", err, want)
			}
		}
	})

	t.Run("an entry the seam cannot lstat", func(t *testing.T) {
		t.Parallel()
		dir, _ := secretConfigDir(t, "present.env")
		s := liveSpec()
		s.ConfigDir = dir
		s.SecretsFS = &secretFSFake{failName: "present.env", lstatErr: fs.ErrPermission}
		s.Secrets = []templates.SecretFile{secretEntry("present.env", false)}

		out, err := templates.RenderDaemonCompose(s)
		if err == nil {
			t.Fatalf("RenderDaemonCompose with an entry the seam cannot lstat = a render, want a refusal:\n%s", out)
		}
		for _, want := range []string{`"present.env"`, "cannot be read"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to say %q", err, want)
			}
		}
	})

	t.Run("an entry the seam can lstat but not stat", func(t *testing.T) {
		t.Parallel()
		dir, _ := secretConfigDir(t, "present.env")
		s := liveSpec()
		s.ConfigDir = dir
		s.SecretsFS = &secretFSFake{failName: "present.env", statErr: fs.ErrPermission}
		s.Secrets = []templates.SecretFile{secretEntry("present.env", false)}

		out, err := templates.RenderDaemonCompose(s)
		if err == nil {
			t.Fatalf("RenderDaemonCompose with an entry the seam cannot stat = a render, want a refusal:\n%s", out)
		}
		for _, want := range []string{`"present.env"`, "cannot be read"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to say %q", err, want)
			}
		}
	})
}

// The manifest's mode is a maximum and the render enforces it: a file readable
// by group or other, one carrying a bit beyond the entry's mode, and a symlink
// are each refused naming the entry and the mode on disk, while a file with
// fewer bits than the maximum is mounted (gt-gg9hv.66).
func TestDaemonComposeSecretsEnforcesTheMode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		mode fs.FileMode
		want []string
	}{
		{name: "readable by group", mode: 0o640, want: []string{`"a.env"`, "0640", "group or other"}},
		{name: "readable by other", mode: 0o604, want: []string{`"a.env"`, "0604", "group or other"}},
		{name: "a user bit beyond the maximum", mode: 0o700, want: []string{`"a.env"`, "0700", "0600", "maximum"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, _ := secretConfigDir(t, "a.env")
			if err := os.Chmod(filepath.Join(dir, "a.env"), tc.mode); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			s := liveSpec()
			s.ConfigDir = dir
			s.Secrets = []templates.SecretFile{secretEntry("a.env", true)}

			out, err := templates.RenderDaemonCompose(s)
			if err == nil {
				t.Fatalf("RenderDaemonCompose over a %04o file = a render, want a refusal:\n%s", tc.mode, out)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to name %q", err, want)
				}
			}
		})
	}

	t.Run("an entry that is not a regular file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := syscall.Mkfifo(filepath.Join(dir, "a.env"), 0o600); err != nil {
			t.Fatalf("making the fifo: %v", err)
		}
		s := liveSpec()
		s.ConfigDir = dir
		s.Secrets = []templates.SecretFile{secretEntry("a.env", true)}

		out, err := templates.RenderDaemonCompose(s)
		if err == nil {
			t.Fatalf("RenderDaemonCompose over a fifo = a render, want a refusal:\n%s", out)
		}
		for _, want := range []string{`"a.env"`, "not a regular file"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to name %q", err, want)
			}
		}
	})

	t.Run("a symlink is refused", func(t *testing.T) {
		t.Parallel()
		dir, _ := secretConfigDir(t, "real.env")
		if err := os.Symlink(filepath.Join(dir, "real.env"), filepath.Join(dir, "a.env")); err != nil {
			t.Fatalf("making the symlink: %v", err)
		}
		s := liveSpec()
		s.ConfigDir = dir
		s.Secrets = []templates.SecretFile{secretEntry("a.env", true)}

		out, err := templates.RenderDaemonCompose(s)
		if err == nil {
			t.Fatalf("RenderDaemonCompose over a symlink = a render, want a refusal:\n%s", out)
		}
		for _, want := range []string{`"a.env"`, "symlink"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to name %q", err, want)
			}
		}
	})

	t.Run("fewer bits than the maximum is mounted", func(t *testing.T) {
		t.Parallel()
		dir, _ := secretConfigDir(t, "a.env")
		if err := os.Chmod(filepath.Join(dir, "a.env"), 0o400); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		s := liveSpec()
		s.ConfigDir = dir
		s.Secrets = []templates.SecretFile{secretEntry("a.env", true)}

		_, f := render(t, s)
		want := composeVolume{Type: "bind", Source: filepath.Join(dir, "a.env"), Target: filepath.Join(dir, "a.env"), ReadOnly: true}
		for _, v := range f.Services["daemon"].Volumes {
			if v == want {
				return
			}
		}
		t.Errorf("daemon volumes = %+v, want the read-only mount %+v", f.Services["daemon"].Volumes, want)
	})
}

// The spec-side checks of a secrets manifest: the files cannot be resolved
// without a config directory, a scratch spec carries none, an entry is checked
// rather than trusted, and a BindRO directory that holds the config tree is
// refused because it would mount the tree the manifest replaces.
func TestDaemonComposeSecretsRefusesWhatTheSpecCannotCarry(t *testing.T) {
	t.Parallel()
	bindRoot := t.TempDir()
	bindConfig := filepath.Join(bindRoot, "gt")
	if err := os.MkdirAll(bindConfig, 0o700); err != nil {
		t.Fatalf("making the config directory fixture: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*templates.ComposeSpec)
		want   string
	}{
		{name: "no config directory", want: "ConfigDir is empty",
			mutate: func(s *templates.ComposeSpec) {
				s.ConfigDir = ""
				s.Secrets = []templates.SecretFile{secretEntry("a.env", true)}
			}},
		{name: "an absolute entry", want: `"/etc/passwd"`,
			mutate: func(s *templates.ComposeSpec) { s.Secrets = []templates.SecretFile{secretEntry("/etc/passwd", true)} }},
		{name: "an entry under dot-dot", want: `"../escape"`,
			mutate: func(s *templates.ComposeSpec) { s.Secrets = []templates.SecretFile{secretEntry("../escape", true)} }},
		{name: "an entry with no consumer", want: `"a.env"`,
			mutate: func(s *templates.ComposeSpec) {
				s.Secrets = []templates.SecretFile{{Name: "a.env", Mode: "0600", Required: true}}
			}},
		{name: "an entry with a group-readable mode", want: `"a.env"`,
			mutate: func(s *templates.ComposeSpec) {
				s.Secrets = []templates.SecretFile{{Name: "a.env", Consumer: "test", Mode: "0644"}}
			}},
		{name: "on a scratch spec", want: "scratch spec",
			mutate: func(s *templates.ComposeSpec) {
				*s = scratchSpec()
				s.Secrets = []templates.SecretFile{secretEntry("a.env", true)}
			}},
		{name: "a bind-ro directory holding the config tree", want: "config directory",
			mutate: func(s *templates.ComposeSpec) {
				s.ConfigDir = bindConfig
				s.BindRO = []string{bindRoot}
				s.Secrets = []templates.SecretFile{secretEntry("a.env", true)}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := liveSpec()
			s.ConfigDir = t.TempDir()
			tc.mutate(&s)
			if _, err := templates.RenderDaemonCompose(s); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}

	t.Run("a config directory that is a file", func(t *testing.T) {
		t.Parallel()
		file := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("writing the fixture file: %v", err)
		}
		s := liveSpec()
		s.ConfigDir = file
		s.Secrets = []templates.SecretFile{secretEntry("a.env", true)}
		if _, err := templates.RenderDaemonCompose(s); err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("error = %v, want the config directory refused as not a directory", err)
		}
	})
}

// The manifest parser reads one entry per file with its name, consumer, mode and
// required flag and records the unclaimed files with their searches. Every
// malformed manifest is refused naming the entry or the key it cannot take, so a
// manifest a hand edit broke never reaches the render.
func TestDaemonComposeSecretsManifestParses(t *testing.T) {
	t.Parallel()

	t.Run("the entries and their fields", func(t *testing.T) {
		t.Parallel()
		manifest, err := templates.ParseSecretsManifest([]byte(`
files:
  - name: forgejo-landing.env
    consumer: the landing worker
    mode: "0600"
    required: true
  - name: om-daemon-token.meta
    consumer: the om token's expiry sidecar
    mode: "0600"
unclaimed:
  - name: hm-config.json
    search: "grep -rn hm-config internal cmd"
`))
		if err != nil {
			t.Fatalf("ParseSecretsManifest: %v", err)
		}
		want := []templates.SecretFile{
			{Name: "forgejo-landing.env", Consumer: "the landing worker", Mode: "0600", Required: true},
			{Name: "om-daemon-token.meta", Consumer: "the om token's expiry sidecar", Mode: "0600"},
		}
		if len(manifest.Files) != len(want) {
			t.Fatalf("files = %+v, want %+v", manifest.Files, want)
		}
		for i := range want {
			if manifest.Files[i] != want[i] {
				t.Errorf("files[%d] = %+v, want %+v", i, manifest.Files[i], want[i])
			}
		}
		if len(manifest.Unclaimed) != 1 || manifest.Unclaimed[0].Name != "hm-config.json" || manifest.Unclaimed[0].Search == "" {
			t.Errorf("unclaimed = %+v, want the unclaimed file with its search", manifest.Unclaimed)
		}
	})

	for _, tc := range []struct {
		name string
		data string
		want string
	}{
		{name: "malformed YAML", want: "secrets manifest",
			data: "files: [\n"},
		{name: "no consumer", want: `"a.env"`,
			data: "files:\n  - name: a.env\n    mode: \"0600\"\n"},
		{name: "an empty name", want: "files[0]",
			data: "files:\n  - name: \"\"\n    consumer: test\n    mode: \"0600\"\n"},
		{name: "an absolute name", want: `"/etc/passwd"`,
			data: "files:\n  - name: /etc/passwd\n    consumer: test\n    mode: \"0600\"\n"},
		{name: "a .. name", want: `"../escape"`,
			data: "files:\n  - name: ../escape\n    consumer: test\n    mode: \"0600\"\n"},
		{name: "a bare dot-dot name", want: `".."`,
			data: "files:\n  - name: ..\n    consumer: test\n    mode: \"0600\"\n"},
		{name: "a path name", want: `"sub/a.env"`,
			data: "files:\n  - name: sub/a.env\n    consumer: test\n    mode: \"0600\"\n"},
		{name: "a directory name", want: `"topics/"`,
			data: "files:\n  - name: topics/\n    consumer: test\n    mode: \"0600\"\n"},
		{name: "a control character in the name", want: "control character",
			data: "files:\n  - name: \"a\\x01b\"\n    consumer: test\n    mode: \"0600\"\n"},
		{name: "a three-digit mode", want: `"a.env"`,
			data: "files:\n  - name: a.env\n    consumer: test\n    mode: \"600\"\n"},
		{name: "a group-readable mode", want: `"a.env"`,
			data: "files:\n  - name: a.env\n    consumer: test\n    mode: \"0644\"\n"},
		{name: "a repeated name", want: `"a.env"`,
			data: "files:\n  - name: a.env\n    consumer: one\n    mode: \"0600\"\n  - name: a.env\n    consumer: two\n    mode: \"0600\"\n"},
		{name: "no files", want: "lists no files",
			data: "unclaimed:\n  - name: b.env\n    search: grep\n"},
		{name: "an unclaimed entry with a bad name", want: `"/b.env"`,
			data: "files:\n  - name: a.env\n    consumer: test\n    mode: \"0600\"\nunclaimed:\n  - name: /b.env\n    search: grep\n"},
		{name: "an unclaimed entry with no name", want: "unclaimed[0]",
			data: "files:\n  - name: a.env\n    consumer: test\n    mode: \"0600\"\nunclaimed:\n  - name: \"\"\n    search: grep\n"},
		{name: "an unclaimed entry with no search", want: `"b.env"`,
			data: "files:\n  - name: a.env\n    consumer: test\n    mode: \"0600\"\nunclaimed:\n  - name: b.env\n"},
		{name: "a repeated unclaimed name", want: `"b.env"`,
			data: "files:\n  - name: a.env\n    consumer: test\n    mode: \"0600\"\nunclaimed:\n  - name: b.env\n    search: one\n  - name: b.env\n    search: two\n"},
		{name: "a name in both lists", want: `"a.env"`,
			data: "files:\n  - name: a.env\n    consumer: test\n    mode: \"0600\"\nunclaimed:\n  - name: a.env\n    search: grep\n"},
		{name: "an unknown key", want: "owner",
			data: "files:\n  - name: a.env\n    consumer: test\n    mode: \"0600\"\n    owner: someone\n"},
		{name: "a second document", want: "more than one YAML document",
			data: "files:\n  - name: a.env\n    consumer: test\n    mode: \"0600\"\n---\nfiles: []\n"},
		{name: "a malformed second document", want: "secrets manifest",
			data: "files:\n  - name: a.env\n    consumer: test\n    mode: \"0600\"\n---\nbad: [\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := templates.ParseSecretsManifest([]byte(tc.data))
			if err == nil {
				t.Fatal("ParseSecretsManifest = a manifest, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// inside reports whether path is dir or lies under it, for the rendered paths a
// test compares without resolving them.
func inside(dir, path string) bool {
	return path == dir || strings.HasPrefix(path, dir+"/")
}
