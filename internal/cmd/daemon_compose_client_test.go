package cmd

import (
	"errors"
	"github.com/steveyegge/gastown/internal/config"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	_ "time/tzdata" // the zone tests load zones by name; carry the database so they do not depend on the host

	"gopkg.in/yaml.v3"
)

// hostKubeconfig is a kubeconfig shaped like the kind cluster's, with fake
// certificate and key bytes.
const hostKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: Q0FEQVRB
    server: https://127.0.0.1:59174
  name: kind-gt-sbx
contexts:
- context: {cluster: kind-gt-sbx, user: kind-gt-sbx}
  name: kind-gt-sbx
current-context: kind-gt-sbx
users:
- name: kind-gt-sbx
  user:
    client-certificate-data: Q0VSVERBVEE=
    client-key-data: U0VDUkVUS0VZ
`

func TestContainerKubeconfigPointsAtHostDockerInternalWithTLSServerName(t *testing.T) {
	t.Parallel()
	out, err := containerKubeconfig([]byte(hostKubeconfig), "/towns/t/.runtime/sbx-k8s", "/towns/t")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Clusters []struct {
			Name    string            `yaml:"name"`
			Cluster map[string]string `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	if err := yaml.Unmarshal(out, &got); err != nil || len(got.Clusters) != 1 {
		t.Fatalf("derived kubeconfig = %q, %v", out, err)
	}
	c := got.Clusters[0]
	if c.Name != "kind-gt-sbx" || c.Cluster["server"] != "https://host.docker.internal:59174" ||
		c.Cluster["tls-server-name"] != "localhost" || c.Cluster["certificate-authority-data"] != "Q0FEQVRB" {
		t.Errorf("cluster = %+v, want the new server, tls-server-name localhost and the original CA", c)
	}
	for _, secret := range []string{"U0VDUkVUS0VZ", "Q0VSVERBVEE=", "client-key", "users:"} {
		if strings.Contains(string(out), secret) {
			t.Errorf("the derived kubeconfig carries %q; credentials stay in the original, by reference", secret)
		}
	}
}

func TestContainerKubeconfigRefusesWhatItCannotDerive(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ in, want string }{
		"not yaml":   {"clusters: [", "does not parse"},
		"no cluster": {"apiVersion: v1\nkind: Config\n", "names no cluster"},
		"no host":    {"clusters:\n- name: a\n  cluster: {server: 'not a url'}\n", "not a URL with a host"},
	} {
		if _, err := containerKubeconfig([]byte(tc.in), "/d", "/"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want one containing %q", name, err, tc.want)
		}
	}
	out, err := containerKubeconfig([]byte("clusters:\n- name: a\n  cluster: {server: 'https://api.example'}\n"), "/d", "/")
	if err != nil || !strings.Contains(string(out), "server: https://host.docker.internal\n") {
		t.Errorf("a server without a port: %q, %v", out, err)
	}
}

func TestContainerEnvFromDaemonEnvDropsHostOnlyKeys(t *testing.T) {
	t.Parallel()
	in := map[string]string{
		"PATH": "/opt/homebrew/bin", "TMPDIR": "/x", "GOTMPDIR": "/y", "SDKROOT": "/s", "DEVELOPER_DIR": "/d",
		"CMUX_CLAUDE_HOOKS_DISABLED": "1", "BEADS_DOLT_POOL_READ_TIMEOUT": "30s",
	}
	got := containerEnv(in, nil)
	if len(got) != 1 || got["BEADS_DOLT_POOL_READ_TIMEOUT"] != "30s" {
		t.Errorf("container env = %v, want only BEADS_DOLT_POOL_READ_TIMEOUT", got)
	}
}

// A secret that any settings file references, in either syntax, or that a
// daemon.env value references, or whose name is secret-like, is never rendered.
func TestContainerEnvNeverEmitsASecretReference(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		files [][]byte
		extra map[string]string // more daemon.env entries
	}{
		"braced reference in one file":           {[][]byte{[]byte(`{"a":"${REF_A}"}`)}, nil},
		"bare reference in one file":             {[][]byte{[]byte(`{"a":"$REF_A"}`)}, nil},
		"reference in a later file":              {[][]byte{[]byte(`{}`), []byte(`{"escalation":{"x":"${REF_A}"}}`)}, nil},
		"bare reference in a later file":         {[][]byte{[]byte(`{}`), []byte("cmd: curl -H $REF_A")}, nil},
		"reference from a daemon.env value":      {nil, map[string]string{"OTHER": "Bearer ${REF_A}"}},
		"bare reference from a daemon.env value": {nil, map[string]string{"OTHER": "Bearer $REF_A"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := map[string]string{"REF_A": "v", "PLAIN": "v"}
			for k, v := range tc.extra {
				in[k] = v
			}
			got := containerEnv(in, tc.files)
			if _, ok := got["REF_A"]; ok || got["PLAIN"] != "v" {
				t.Errorf("container env = %v, want PLAIN and not the referenced REF_A", got)
			}
		})
	}
	// A secret-named entry is dropped even when nothing references it.
	got := containerEnv(map[string]string{"DEEPSEEK_API_KEY": "k", "GITHUB_TOKEN": "t", "X_PASSWORD": "p", "OK": "v"}, nil)
	if len(got) != 1 || got["OK"] != "v" {
		t.Errorf("container env = %v, want OK only", got)
	}
}

type fakeSettingsEntry struct {
	name string
	mode fs.FileMode
}

func (e fakeSettingsEntry) Name() string               { return e.name }
func (e fakeSettingsEntry) IsDir() bool                { return e.mode.IsDir() }
func (e fakeSettingsEntry) Type() fs.FileMode          { return e.mode.Type() }
func (e fakeSettingsEntry) Info() (fs.FileInfo, error) { return nil, errors.New("unused") }

// tree is a fake settings directory: directory path -> entries, file path -> content,
// link path -> target.
type tree struct {
	dirs  map[string][]os.DirEntry
	files map[string]string
	links map[string]string
	boom  map[string]error // path -> error from readDir, read or eval
}

func (tr tree) fsys() settingsFS {
	return settingsFS{
		readDir: func(p string) ([]os.DirEntry, error) {
			if err := tr.boom["dir:"+p]; err != nil {
				return nil, err
			}
			e, ok := tr.dirs[p]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return e, nil
		},
		read: func(p string) ([]byte, error) {
			if err := tr.boom["read:"+p]; err != nil {
				return nil, err
			}
			c, ok := tr.files[p]
			if !ok {
				return nil, errors.New("read " + p + ": not a file")
			}
			return []byte(c), nil
		},
		eval: func(p string) (string, error) {
			if err := tr.boom["eval:"+p]; err != nil {
				return "", err
			}
			if t, ok := tr.links[p]; ok {
				return t, nil
			}
			return p, nil
		},
	}
}

func reg(name string) os.DirEntry  { return fakeSettingsEntry{name, 0} }
func dir(name string) os.DirEntry  { return fakeSettingsEntry{name, fs.ModeDir} }
func link(name string) os.DirEntry { return fakeSettingsEntry{name, fs.ModeSymlink} }

func envWithRefs() func(string) (map[string]string, error) {
	return func(string) (map[string]string, error) {
		return map[string]string{"REF_A": "v", "REF_B": "v", "REF_C": "v", "PLAIN": "v"}, nil
	}
}

// Every file under settings/ is read for references, subdirectories and links
// that stay under the town included; daemon.env itself is not.
func TestContainerEnvForReadsEverySettingsFile(t *testing.T) {
	t.Parallel()
	tr := tree{
		dirs: map[string][]os.DirEntry{
			"/t/settings":        {reg("config.json"), reg("daemon.env"), dir("themes"), link("shared.json")},
			"/t/settings/themes": {reg("x.json")},
		},
		files: map[string]string{
			"/t/settings/config.json":   `{"a":"${REF_A}"}`,
			"/t/settings/themes/x.json": `{"b":"$REF_B"}`,
			"/t/shared/real.json":       `{"c":"${REF_C}"}`,
		},
		links: map[string]string{"/t/settings/shared.json": "/t/shared/real.json"},
	}
	got, err := containerEnvFor("/t", envWithRefs(), tr.fsys())
	if err != nil || len(got) != 1 || got["PLAIN"] != "v" {
		t.Fatalf("env = %v, %v; want PLAIN only (references in a file, a subdirectory and a link target honoured)", got, err)
	}
}

// A skipped file is an unscanned file: a link that leaves the town or cannot be
// resolved, an unreadable entry, and an entry that is not a file, directory or
// link each refuse the compose.
func TestContainerEnvForRefusesWhatItCannotScan(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	base := func() tree {
		return tree{
			dirs:  map[string][]os.DirEntry{"/t/settings": {reg("config.json")}},
			files: map[string]string{"/t/settings/config.json": `{}`},
		}
	}
	for name, tc := range map[string]struct {
		set  func(*tree)
		want string
	}{
		"link out of the town": {func(tr *tree) {
			tr.dirs["/t/settings"] = []os.DirEntry{link("x.json")}
			tr.links = map[string]string{"/t/settings/x.json": "/etc/secrets.json"}
		}, "outside the town"},
		"link into a sibling prefix": {func(tr *tree) {
			tr.dirs["/t/settings"] = []os.DirEntry{link("x.json")}
			tr.links = map[string]string{"/t/settings/x.json": "/tother/x.json"}
		}, "outside the town"},
		"link unresolvable": {func(tr *tree) {
			tr.dirs["/t/settings"] = []os.DirEntry{link("x.json")}
			tr.boom = map[string]error{"eval:/t/settings/x.json": boom}
		}, "cannot be resolved"},
		"link target unreadable": {func(tr *tree) {
			tr.dirs["/t/settings"] = []os.DirEntry{link("x.json")}
			tr.links = map[string]string{"/t/settings/x.json": "/t/shared/gone.json"}
		}, "a link to"},
		"irregular entry": {func(tr *tree) {
			tr.dirs["/t/settings"] = []os.DirEntry{fakeSettingsEntry{"sock", fs.ModeSocket}}
		}, "neither a file, a directory nor a link"},
		"file unreadable":  {func(tr *tree) { tr.boom = map[string]error{"read:/t/settings/config.json": boom} }, "read the setting"},
		"top dir unlisted": {func(tr *tree) { tr.boom = map[string]error{"dir:/t/settings": boom} }, "list /t/settings"},
		"subdir unlisted": {func(tr *tree) {
			tr.dirs["/t/settings"] = []os.DirEntry{dir("sub")}
			tr.boom = map[string]error{"dir:/t/settings/sub": boom}
		}, "list /t/settings/sub"},
		"town unresolvable": {func(tr *tree) { tr.boom = map[string]error{"eval:/t": boom} }, "town root"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tr := base()
			tc.set(&tr)
			_, err := containerEnvFor("/t", envWithRefs(), tr.fsys())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
	if _, err := containerEnvFor("/t", func(string) (map[string]string, error) { return nil, boom }, base().fsys()); !errors.Is(err, boom) {
		t.Errorf("load error = %v", err)
	}
	// A town without a settings directory has no references to protect.
	none := tree{}
	if got, err := containerEnvFor("/t", envWithRefs(), none.fsys()); err != nil || len(got) != 4 {
		t.Errorf("a town without settings: %v, %v", got, err)
	}
}

func TestContainerKubeconfigMakesARelativeCAFileAbsolute(t *testing.T) {
	t.Parallel()
	out, err := containerKubeconfig([]byte("clusters:\n- name: a\n  cluster: {server: 'https://127.0.0.1:1', certificate-authority: ca.crt}\n"), "/towns/t/.runtime/sbx-k8s", "/towns/t")
	if err != nil || !strings.Contains(string(out), "certificate-authority: /towns/t/.runtime/sbx-k8s/ca.crt") {
		t.Errorf("derived = %q, %v; want the CA file resolved against the original's directory", out, err)
	}
}

func TestComposeWriteRefusesAKubeconfigOutsideTheTown(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for name, tc := range map[string]struct {
		kubeconfig string
		eval       func(string) (string, error)
		want       string
	}{
		"outside": {"/elsewhere/kubeconfig", nil, "not an absolute path under the town"},
		"a link out": {composeTestTown + "/.runtime/link", func(p string) (string, error) {
			if p == composeTestTown {
				return p, nil
			}
			return "/elsewhere/real", nil
		}, "resolves to /elsewhere/real"},
		"relative": {"rel/kubeconfig", nil, "not an absolute path under the town"},
		"unresolvable": {composeTestTown + "/.runtime/k", func(p string) (string, error) {
			if p == composeTestTown {
				return p, nil
			}
			return "", boom
		}, "cannot be resolved"},
		"town unresolvable": {composeTestTown + "/.runtime/k", func(string) (string, error) { return "", boom }, "town root"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newComposeRig(t)
			r.flags.kubeconfig = tc.kubeconfig
			if tc.eval != nil {
				r.deps.evalSymlinks = tc.eval
			}
			r.deps.readFile = func(string) ([]byte, error) { return []byte(hostKubeconfig), nil }
			err := runDaemonCompose(r.flags, r.deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(r.wrote) != 0 {
				t.Fatalf("err=%v wrote=%v, want %q and no write", err, r.wrote, tc.want)
			}
		})
	}
}

func TestComposeWriteCarriesTheClientConfiguration(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	r.flags.kubeconfig = composeTestTown + "/.runtime/sbx-k8s/kubeconfig"
	r.flags.openShellConfigDir = "/host/home/.config/openshell"
	r.deps.containerEnv = func(string) (map[string]string, error) {
		return map[string]string{"BEADS_DOLT_POOL_READ_TIMEOUT": "30s"}, nil
	}
	r.deps.readFile = func(string) ([]byte, error) { return []byte(hostKubeconfig), nil }
	if err := runDaemonCompose(r.flags, r.deps); err != nil {
		t.Fatal(err)
	}
	kube := containerKubeconfigPath(r.town)
	if len(r.wrote) != 2 || r.wrote[0] != kube || r.wrote[1] != r.composePath() {
		t.Fatalf("wrote %v, want the derived kubeconfig then the compose file", r.wrote)
	}
	compose := string(r.disk[r.composePath()])
	for _, want := range []string{"KUBECONFIG: \"" + kube + ":" + r.flags.kubeconfig + "\"", "XDG_CONFIG_HOME", "BEADS_DOLT_POOL_READ_TIMEOUT: \"30s\"", "/host/home/.config/openshell"} {
		if !strings.Contains(compose, want) {
			t.Errorf("the compose file lacks %q:\n%s", want, compose)
		}
	}
	if strings.Contains(compose, "U0VDUkVUS0VZ") {
		t.Error("the compose file carries credential bytes")
	}
}

func TestComposeWriteRefusesBeforeWritingWhenTheClientConfigurationFails(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for name, tc := range map[string]struct {
		set  func(*composeRig)
		want string
	}{
		"env": {func(r *composeRig) {
			r.deps.containerEnv = func(string) (map[string]string, error) { return nil, boom }
		}, "daemon.env"},
		"read": {func(r *composeRig) { r.deps.readFile = func(string) ([]byte, error) { return nil, boom } }, "reading the kubeconfig"},
		"derive": {func(r *composeRig) {
			r.deps.readFile = func(string) ([]byte, error) { return []byte("kind: Config\n"), nil }
		}, "deriving the container kubeconfig"},
		"overlay write": {func(r *composeRig) {
			r.deps.readFile = func(string) ([]byte, error) { return []byte(hostKubeconfig), nil }
			r.writeErr = boom
		}, "container kubeconfig"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newComposeRig(t)
			r.flags.kubeconfig = composeTestTown + "/.runtime/k"
			tc.set(r)
			err := runDaemonCompose(r.flags, r.deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
			if _, ok := r.disk[r.composePath()]; ok {
				t.Error("a compose file was written despite the refusal")
			}
		})
	}
}

func TestContainerKubeconfigRefusesACAFileOutsideTheTown(t *testing.T) {
	t.Parallel()
	in := []byte("clusters:\n- name: a\n  cluster: {server: 'https://127.0.0.1:1', certificate-authority: ../../../../etc/ca.crt}\n")
	if _, err := containerKubeconfig(in, "/towns/t/.runtime/sbx-k8s", "/towns/t"); err == nil || !strings.Contains(err.Error(), "outside the town") {
		t.Errorf("error = %v, want the outside-the-town refusal", err)
	}
}

func TestSecretNamePatternCoversCommonCredentialNames(t *testing.T) {
	t.Parallel()
	for _, n := range []string{"GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY", "DB_PASSWORD", "SSH_PRIVATE_KEY", "STRIPE_KEY", "Authorization", "SESSION_COOKIE", "BEARER", "API_KEY", "ACCESS_KEY_ID", "KEY"} {
		if !config.SecretNamePattern.MatchString(n) {
			t.Errorf("%s is not matched as a credential name", n)
		}
	}
	for _, n := range []string{"BEADS_DOLT_POOL_READ_TIMEOUT", "TZ", "PATH", "KEYBOARD_LAYOUT_X"} {
		if config.SecretNamePattern.MatchString(n) && n != "KEYBOARD_LAYOUT_X" {
			t.Errorf("%s is matched as a credential name", n)
		}
	}
}

func TestContainerZoneIsTheTownsThenTheHostsAndNeverSilentUTC(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	none := func(string) (*config.TownSettings, error) { return nil, nil }
	named := func(zone string) func(string) (*config.TownSettings, error) {
		return func(string) (*config.TownSettings, error) {
			return &config.TownSettings{PolecatPool: &config.PolecatPool{Timezone: zone}}, nil
		}
	}
	link := func(target string, err error) func(string) (string, error) {
		return func(string) (string, error) { return target, err }
	}
	if z, err := containerZone("/t", named("America/Denver"), link("/zoneinfo/Europe/Paris", nil), "/etc/localtime"); z != "America/Denver" || err != nil {
		t.Errorf("a town naming a zone = %q, %v", z, err)
	}
	if z, err := containerZone("/t", none, link("/var/db/timezone/zoneinfo/America/Chicago", nil), "/etc/localtime"); z != "America/Chicago" || err != nil {
		t.Errorf("the host's zone = %q, %v", z, err)
	}
	for name, tc := range map[string]struct {
		load func(string) (*config.TownSettings, error)
		rl   func(string) (string, error)
		want string
	}{
		"unreadable host link":      {none, link("", boom), "cannot be read"},
		"link not under zoneinfo":   {none, link("/etc/other", nil), "not under a zoneinfo directory"},
		"a misspelt town zone":      {named("America/Chicgo"), link("", nil), "does not load"},
		"a host link to nonsense":   {none, link("/zoneinfo/Not/AZone", nil), "does not load"},
		"a link ending at zoneinfo": {none, link("/usr/share/zoneinfo/", nil), "not an IANA zone name"},
		"a town zone Local":         {named("Local"), link("", nil), "not an IANA zone name"},
		"unreadable settings":       {func(string) (*config.TownSettings, error) { return nil, boom }, link("", nil), "polecat_pool.timezone"},
	} {
		if z, err := containerZone("/t", tc.load, tc.rl, "/etc/localtime"); err == nil || z != "" || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s = %q, %v; want a refusal containing %q", name, z, err, tc.want)
		}
	}
}

// The production seam reads a real link: a temporary symlink into a fake
// zoneinfo tree stands in for /etc/localtime, and the real deps carry the
// zone function.
func TestContainerZoneReadsARealLinkAndTheDepsCarryIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	link := filepath.Join(dir, "localtime")
	if err := os.Symlink("/usr/share/zoneinfo/America/Chicago", link); err != nil {
		t.Fatal(err)
	}
	z, err := containerZone("/t", func(string) (*config.TownSettings, error) { return nil, nil }, os.Readlink, link)
	if err != nil || z != "America/Chicago" {
		t.Errorf("zone from a real link = %q, %v", z, err)
	}
	if realDaemonComposeDeps(io.Discard).townZone == nil {
		t.Error("the real deps carry no townZone")
	}
}

func TestComposeWriteCarriesTheZoneSidecarAndWorkRoot(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	r.deps.townZone = func(string) (string, error) { return "America/Chicago", nil }
	r.deps.daemonConfig = func(string) (*config.DaemonPatrolConfig, error) {
		return &config.DaemonPatrolConfig{
			ContainerNameSetting: composeTestContainer, ComposeProjectSetting: composeTestProject,
			OMTokenMetaSetting: "/h/om.meta",
			Patrols:            &config.PatrolsConfig{LandingWorker: &config.LandingWorkerConfig{WorkRoot: "/h/landing"}},
		}, nil
	}
	if err := runDaemonCompose(r.flags, r.deps); err != nil {
		t.Fatal(err)
	}
	compose := string(r.disk[r.composePath()])
	for _, want := range []string{`TZ: "America/Chicago"`, `source: "/h/om.meta"`, `source: "/h/landing"`} {
		if !strings.Contains(compose, want) {
			t.Errorf("the compose lacks %q:\n%s", want, compose)
		}
	}
	r.deps.townZone = func(string) (string, error) { return "", errors.New("boom") }
	if err := runDaemonCompose(r.flags, r.deps); err == nil {
		t.Error("an unreadable zone must refuse")
	}
}
