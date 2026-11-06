package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/templates"

	"gopkg.in/yaml.v3"
)

// secretFSProbe is the spec's SecretFS seam: every call is recorded, the two
// metadata calls reach the host's filesystem, and a content read is refused, so
// a test sees what the render asked for and no file's bytes are read.
type secretFSProbe struct {
	calls []string
}

func (p *secretFSProbe) Lstat(name string) (fs.FileInfo, error) {
	p.calls = append(p.calls, "Lstat "+name)
	return os.Lstat(name)
}

func (p *secretFSProbe) Stat(name string) (fs.FileInfo, error) {
	p.calls = append(p.calls, "Stat "+name)
	return os.Stat(name)
}

func (p *secretFSProbe) ReadFile(name string) ([]byte, error) {
	p.calls = append(p.calls, "ReadFile "+name)
	return nil, errors.New("the probe refuses every content read")
}

// TestDaemonComposeSecretsManifestParses: the shipped deploy/daemon/secrets.yaml
// parses and names a consumer and mode for every file, the notification scripts
// are recorded as unclaimed rather than mounted, and the command refuses a
// malformed manifest naming the entry, before it writes anything
// (gt-gg9hv.66).
func TestDaemonComposeSecretsManifestParses(t *testing.T) {
	t.Parallel()

	t.Run("the shipped manifest", func(t *testing.T) {
		t.Parallel()
		data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "daemon", "secrets.yaml"))
		if err != nil {
			t.Fatalf("reading the shipped manifest: %v", err)
		}
		manifest, err := templates.ParseSecretsManifest(data)
		if err != nil {
			t.Fatalf("the shipped manifest does not parse: %v", err)
		}
		listed := map[string]templates.SecretFile{}
		for _, f := range manifest.Files {
			listed[f.Name] = f
		}
		for _, name := range []string{"forgejo-landing.env", "forgejo-viewer.env", "promote-gastown.key", "om-daemon-token", "om-daemon-token.meta", "jev.env"} {
			f, ok := listed[name]
			if !ok {
				t.Errorf("the shipped manifest lists no %s, a file the daemon reads", name)
				continue
			}
			if strings.TrimSpace(f.Consumer) == "" {
				t.Errorf("%s has no consumer", name)
			}
			if !strings.HasPrefix(f.Mode, "0") || len(f.Mode) != 4 {
				t.Errorf("%s mode = %q, want four octal digits", name, f.Mode)
			}
		}
		unclaimed := map[string]bool{}
		for _, u := range manifest.Unclaimed {
			if strings.TrimSpace(u.Search) == "" {
				t.Errorf("unclaimed %s has no search", u.Name)
			}
			unclaimed[u.Name] = true
		}
		// No daemon code in this checkout opens ntfy-push.sh, so mounting it
		// would be a guess (gt-gg9hv.66 rework).
		if listed["ntfy-push.sh"].Name != "" {
			t.Error("the shipped manifest mounts ntfy-push.sh, which no daemon code in this checkout opens")
		}
		if !unclaimed["ntfy-push.sh"] {
			t.Error("the shipped manifest records no search for ntfy-push.sh")
		}
	})

	// Each row is a manifest the command refuses before it renders, naming the
	// entry the parser found wrong.
	for _, tc := range []struct {
		name     string
		manifest string
		want     string
	}{
		{name: "no consumer", want: `"a.env"`,
			manifest: "files:\n  - name: a.env\n    mode: \"0600\"\n"},
		{name: "an absolute name", want: `"/etc/passwd"`,
			manifest: "files:\n  - name: /etc/passwd\n    consumer: test\n    mode: \"0600\"\n"},
		{name: "a .. name", want: `"../escape"`,
			manifest: "files:\n  - name: ../escape\n    consumer: test\n    mode: \"0600\"\n"},
		{name: "a path name", want: `"sub/a.env"`,
			manifest: "files:\n  - name: sub/a.env\n    consumer: test\n    mode: \"0600\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			manifestPath := filepath.Join(t.TempDir(), "secrets.yaml")
			if err := os.WriteFile(manifestPath, []byte(tc.manifest), 0o600); err != nil {
				t.Fatalf("writing the manifest: %v", err)
			}
			r := newComposeRig(t)
			r.flags.configDir = t.TempDir()
			r.flags.secretsManifest = manifestPath
			r.flags.secretsManifestSet = true
			r.deps.readFile = func(path string) ([]byte, error) {
				if path == manifestPath {
					return []byte(tc.manifest), nil
				}
				return nil, fs.ErrNotExist
			}

			err := runDaemonCompose(r.flags, r.deps)
			if err == nil {
				t.Fatal("runDaemonCompose = no error, want the manifest refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
			if len(r.wrote) != 0 {
				t.Errorf("a refusal wrote %v", r.wrote)
			}
		})
	}

	t.Run("an entry that is a directory in the config directory", func(t *testing.T) {
		t.Parallel()
		configDir := t.TempDir()
		if err := os.Mkdir(filepath.Join(configDir, "topics"), 0o700); err != nil {
			t.Fatalf("making the directory entry: %v", err)
		}
		const manifest = "files:\n  - name: topics\n    consumer: test\n    mode: \"0700\"\n"
		manifestPath := filepath.Join(t.TempDir(), "secrets.yaml")
		if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
			t.Fatalf("writing the manifest: %v", err)
		}
		r := newComposeRig(t)
		r.flags.configDir = configDir
		r.flags.secretsManifest = manifestPath
		r.flags.secretsManifestSet = true
		r.deps.readFile = func(path string) ([]byte, error) {
			if path == manifestPath {
				return []byte(manifest), nil
			}
			return nil, fs.ErrNotExist
		}

		err := runDaemonCompose(r.flags, r.deps)
		if err == nil {
			t.Fatal("runDaemonCompose with a directory entry = no error, want a refusal")
		}
		for _, want := range []string{`"topics"`, "directory"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to say %q", err, want)
			}
		}
		if len(r.wrote) != 0 {
			t.Errorf("a refusal wrote %v", r.wrote)
		}
	})
}

// TestDaemonComposeSecretsNeverReadsContents: the command reads the manifest and
// opens none of the files it lists. The seam the render stats through records
// every call and refuses every content read, so a read would fail the render and
// a successful render proves the listing alone was used, while no file's bytes
// reach the compose file or the output (gt-gg9hv.66).
func TestDaemonComposeSecretsNeverReadsContents(t *testing.T) {
	t.Parallel()
	const marker = "placeholder-file-bytes-never-read"
	configDir := t.TempDir()
	for _, name := range []string{"forgejo-landing.env", "jev.env"} {
		if err := os.WriteFile(filepath.Join(configDir, name), []byte(marker+"\n"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	manifest := "files:\n" +
		"  - name: forgejo-landing.env\n    consumer: the landing worker\n    mode: \"0600\"\n    required: true\n" +
		"  - name: jev.env\n    consumer: the jev key\n    mode: \"0600\"\n"
	manifestPath := filepath.Join(t.TempDir(), "secrets.yaml")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatalf("writing the manifest: %v", err)
	}

	r := newComposeRig(t)
	r.flags.configDir = configDir
	r.flags.secretsManifest = manifestPath
	r.flags.secretsManifestSet = true
	var opened []string
	r.deps.readFile = func(path string) ([]byte, error) {
		opened = append(opened, path)
		if path == manifestPath {
			return []byte(manifest), nil
		}
		return nil, fs.ErrNotExist
	}
	probe := &secretFSProbe{}
	r.deps.secretsFS = probe

	if err := runDaemonCompose(r.flags, r.deps); err != nil {
		t.Fatalf("runDaemonCompose: %v", err)
	}
	if len(opened) != 1 || opened[0] != manifestPath {
		t.Errorf("the command opened %v, want only the manifest %s", opened, manifestPath)
	}
	for _, name := range []string{"forgejo-landing.env", "jev.env"} {
		host := filepath.Join(configDir, name)
		if !strings.Contains(strings.Join(probe.calls, "\n"), "Stat "+host) {
			t.Errorf("the seam saw no Stat of %s, so the render did not go through it; calls = %v", host, probe.calls)
		}
	}
	for _, call := range probe.calls {
		if strings.HasPrefix(call, "ReadFile ") {
			t.Errorf("the render read %s, want no content read at all", strings.TrimPrefix(call, "ReadFile "))
		}
	}
	got, ok := r.disk[r.composePath()]
	if !ok {
		t.Fatalf("no compose file written at %s", r.composePath())
	}
	if bytes.Contains(got, []byte(marker)) {
		t.Errorf("the compose file carries a listed file's contents:\n%s", got)
	}
	if strings.Contains(r.out.String(), marker) {
		t.Errorf("the output carries a listed file's contents: %q", r.out.String())
	}
	for _, name := range []string{"forgejo-landing.env", "jev.env"} {
		host := filepath.Join(configDir, name)
		want := "      - type: bind\n        source: \"" + host + "\"\n        target: \"" + host + "\"\n        read_only: true\n"
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("the compose file does not mount %s read-only at its own path:\n%s", name, got)
		}
	}
	if bytes.Contains(got, []byte(`source: "`+configDir+`"`)) {
		t.Errorf("the compose file mounts the config directory itself:\n%s", got)
	}
}

// TestDaemonComposeSecretsSaysRotationNeedsARecreate: a bind mount of one file
// pins its inode, so a rotated secret goes stale in the running container. Every
// manifest render says so, on the warning stream so --print's output stays the
// compose file, the shipped manifest's header says the same line, and the
// rendered file mounts no directory of the config tree (gt-gg9hv.66).
func TestDaemonComposeSecretsSaysRotationNeedsARecreate(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "jev.env"), []byte("placeholder\n"), 0o600); err != nil {
		t.Fatalf("writing jev.env: %v", err)
	}
	manifest := "files:\n  - name: jev.env\n    consumer: the jev key\n    mode: \"0600\"\n"
	manifestPath := filepath.Join(t.TempDir(), "secrets.yaml")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatalf("writing the manifest: %v", err)
	}

	newRig := func(t *testing.T) *composeRig {
		t.Helper()
		r := newComposeRig(t)
		r.flags.configDir = configDir
		r.flags.secretsManifest = manifestPath
		r.flags.secretsManifestSet = true
		r.deps.readFile = func(path string) ([]byte, error) {
			if path == manifestPath {
				return []byte(manifest), nil
			}
			return nil, fs.ErrNotExist
		}
		return r
	}

	t.Run("the header of the shipped manifest carries it", func(t *testing.T) {
		t.Parallel()
		data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "daemon", "secrets.yaml"))
		if err != nil {
			t.Fatalf("reading the shipped manifest: %v", err)
		}
		if !bytes.Contains(data, []byte(templates.SecretsRecreateNotice)) {
			t.Errorf("the manifest header lacks %q", templates.SecretsRecreateNotice)
		}
	})

	t.Run("a write prints it and mounts no directory", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		if err := runDaemonCompose(r.flags, r.deps); err != nil {
			t.Fatalf("runDaemonCompose: %v", err)
		}
		if !strings.Contains(r.errOut.String(), templates.SecretsRecreateNotice) {
			t.Errorf("the command printed no rotation notice; stderr = %q", r.errOut.String())
		}
		if strings.Contains(r.out.String(), templates.SecretsRecreateNotice) {
			t.Errorf("the notice went to the output beside the written path: %q", r.out.String())
		}
		got, ok := r.disk[r.composePath()]
		if !ok {
			t.Fatalf("no compose file written at %s", r.composePath())
		}
		var composed struct {
			Services map[string]struct {
				Volumes []struct {
					Source string `yaml:"source"`
					Target string `yaml:"target"`
				} `yaml:"volumes"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(got, &composed); err != nil {
			t.Fatalf("the compose file does not parse as YAML: %v\n%s", err, got)
		}
		for _, v := range composed.Services["daemon"].Volumes {
			if v.Source == configDir || v.Target == configDir {
				t.Errorf("the daemon mounts the config directory %s, want only the listed files", configDir)
			}
		}
	})

	t.Run("a print renders it and keeps the output the compose file", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		r.flags.print = true
		if err := runDaemonCompose(r.flags, r.deps); err != nil {
			t.Fatalf("runDaemonCompose: %v", err)
		}
		if !strings.Contains(r.errOut.String(), templates.SecretsRecreateNotice) {
			t.Errorf("--print printed no rotation notice; stderr = %q", r.errOut.String())
		}
		var composed map[string]any
		if err := yaml.Unmarshal(r.out.Bytes(), &composed); err != nil {
			t.Errorf("--print's output is not the compose file alone: %v\n%s", err, r.out.String())
		}
	})
}

// TestDaemonComposeSecretsOpensOnlyThroughTheSeam: the secrets code reaches the
// host only through templates.SecretFS. A direct os.Open, os.ReadFile,
// os.OpenFile or ioutil.ReadFile in the secrets files of internal/templates or
// internal/cmd fails, outside the one file that implements the seam, so a read
// added later cannot bypass the seam unobserved (gt-gg9hv.66).
func TestDaemonComposeSecretsOpensOnlyThroughTheSeam(t *testing.T) {
	t.Parallel()
	const seamFile = "daemon_compose_secrets_fs.go"

	var scanned []string
	for _, dir := range []string{filepath.Join("..", "templates"), "."} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.Contains(name, "secrets") || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			scanned = append(scanned, name)
			if name == seamFile {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			for _, site := range secretOpenSites(t, path, data) {
				t.Errorf("%s opens a file directly; the secrets code reaches the host through templates.SecretFS (%s)", site, seamFile)
			}
		}
	}
	// A scan that saw no files would pass everything; these two are the files
	// the check exists for.
	for _, want := range []string{"daemon_compose_secrets.go", seamFile} {
		if !slices.Contains(scanned, want) {
			t.Errorf("the scan did not see %s; scanned %v", want, scanned)
		}
	}

	t.Run("the scan finds a direct open", func(t *testing.T) {
		t.Parallel()
		src := []byte(`package p
import ("os"; "io/ioutil")
func a() { os.Open("a"); os.ReadFile("b"); os.OpenFile("c", 0, 0); ioutil.ReadFile("d") }
func b(f *os.File) { f.Write(nil) }
`)
		sites := secretOpenSites(t, "sample.go", src)
		for _, want := range []string{"os.Open", "os.ReadFile", "os.OpenFile", "ioutil.ReadFile"} {
			if !strings.Contains(strings.Join(sites, "\n"), want) {
				t.Errorf("the scan missed %s; found %v", want, sites)
			}
		}
	})
}

// secretOpenSites returns one site per direct open or read of a file in src:
// any reference to os.Open, os.ReadFile, os.OpenFile or ioutil.ReadFile, called
// or taken as a value, so an alias of the function is caught too. The parse is
// the scan's only judgment; a file that does not parse is a test failure, never
// an empty list.
func secretOpenSites(t *testing.T, path string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	banned := map[string]map[string]bool{
		"os":     {"Open": true, "ReadFile": true, "OpenFile": true},
		"ioutil": {"ReadFile": true},
	}
	var sites []string
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if banned[pkg.Name][sel.Sel.Name] {
			sites = append(sites, fmt.Sprintf("%s:%d %s.%s", path, fset.Position(sel.Pos()).Line, pkg.Name, sel.Sel.Name))
		}
		return true
	})
	return sites
}

// TestDaemonComposeSecretsFlagNeedsConfigDir: --secrets-manifest is refused
// blank and without --config-dir, writes nothing when either is refused, and the
// flag reaches the spec through the real command when both are given
// (gt-gg9hv.66).
func TestDaemonComposeSecretsFlagNeedsConfigDir(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "jev.env"), []byte("placeholder\n"), 0o600); err != nil {
		t.Fatalf("writing jev.env: %v", err)
	}
	manifestPath := filepath.Join(t.TempDir(), "secrets.yaml")
	manifest := "files:\n  - name: jev.env\n    consumer: the jev key\n    mode: \"0600\"\n"
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatalf("writing the manifest: %v", err)
	}

	t.Run("blank", func(t *testing.T) {
		t.Parallel()
		r := newComposeRig(t)
		r.flags.secretsManifest = "  "
		r.flags.secretsManifestSet = true
		err := runDaemonCompose(r.flags, r.deps)
		if err == nil || !strings.Contains(err.Error(), "was given blank") {
			t.Fatalf("error = %v, want the blank flag refused", err)
		}
		if len(r.wrote) != 0 {
			t.Errorf("a refusal wrote %v", r.wrote)
		}
	})

	t.Run("without a config directory", func(t *testing.T) {
		t.Parallel()
		r := newComposeRig(t)
		r.flags.secretsManifest = manifestPath
		r.flags.secretsManifestSet = true
		err := runDaemonCompose(r.flags, r.deps)
		if err == nil || !strings.Contains(err.Error(), "--config-dir") {
			t.Fatalf("error = %v, want --config-dir named as required", err)
		}
		if len(r.wrote) != 0 {
			t.Errorf("a refusal wrote %v", r.wrote)
		}
	})

	t.Run("an unreadable manifest", func(t *testing.T) {
		t.Parallel()
		r := newComposeRig(t)
		r.flags.configDir = configDir
		r.flags.secretsManifest = manifestPath
		r.flags.secretsManifestSet = true
		r.deps.readFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
		err := runDaemonCompose(r.flags, r.deps)
		if err == nil || !strings.Contains(err.Error(), "reading the secrets manifest") {
			t.Fatalf("error = %v, want the unreadable manifest refused", err)
		}
		if len(r.wrote) != 0 {
			t.Errorf("a refusal wrote %v", r.wrote)
		}
	})

	t.Run("through the command", func(t *testing.T) {
		t.Parallel()
		r := newComposeRig(t)
		r.deps.readFile = func(path string) ([]byte, error) {
			if path == manifestPath {
				return []byte(manifest), nil
			}
			return nil, fs.ErrNotExist
		}
		c := newDaemonComposeCmd(func(_ io.Writer, f daemonComposeFlags) error {
			return runDaemonCompose(f, r.deps)
		})
		c.SetArgs([]string{"write", "--image", daemonimage.ImageTag(composeTestRev),
			"--config-dir", configDir, "--secrets-manifest", manifestPath})
		c.SilenceUsage, c.SilenceErrors = true, true

		if err := c.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
		file, ok := r.disk[r.composePath()]
		if !ok {
			t.Fatalf("no compose file written at %s", r.composePath())
		}
		var composed struct {
			Services map[string]struct {
				Volumes []struct {
					Source string `yaml:"source"`
					Target string `yaml:"target"`
				} `yaml:"volumes"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(file, &composed); err != nil {
			t.Fatalf("the compose file does not parse as YAML: %v\n%s", err, file)
		}
		host := filepath.Join(configDir, "jev.env")
		targets := map[string]bool{}
		for _, v := range composed.Services["daemon"].Volumes {
			if v.Source == host {
				targets[v.Target] = true
			}
		}
		for _, target := range []string{host, filepath.Join(templates.LiveConfigDirPath, "jev.env")} {
			if !targets[target] {
				t.Errorf("jev.env is not mounted at %s; volumes = %+v", target, composed.Services["daemon"].Volumes)
			}
		}
		if targets[configDir] {
			t.Error("the compose file mounts the config directory itself")
		}
	})
}
