package cmd

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	out, err := containerKubeconfig([]byte(hostKubeconfig), "/towns/t/.runtime/sbx-k8s")
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
		if _, err := containerKubeconfig([]byte(tc.in), "/d"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want one containing %q", name, err, tc.want)
		}
	}
	out, err := containerKubeconfig([]byte("clusters:\n- name: a\n  cluster: {server: 'https://api.example'}\n"), "/d")
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

// Every regular settings file is read for references, not config.json alone; the
// directories and daemon.env itself are not.
func TestContainerEnvForReadsEverySettingsFile(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	load := func(string) (map[string]string, error) {
		return map[string]string{"REF_A": "v", "REF_B": "v", "PLAIN": "v"}, nil
	}
	dir := func(string) ([]os.DirEntry, error) {
		return []os.DirEntry{fakeSettingsEntry{"config.json", 0}, fakeSettingsEntry{"escalation.json", 0}, fakeSettingsEntry{"daemon.env", 0}, fakeSettingsEntry{"sub", fs.ModeDir}}, nil
	}
	read := func(p string) ([]byte, error) {
		switch filepath.Base(p) {
		case "config.json":
			return []byte(`{"a":"${REF_A}"}`), nil
		case "escalation.json":
			return []byte(`{"b":"$REF_B"}`), nil
		}
		return nil, errors.New("read " + p + " but it is not a settings file to scan")
	}
	got, err := containerEnvFor("/t", load, dir, read)
	if err != nil || len(got) != 1 || got["PLAIN"] != "v" {
		t.Fatalf("env = %v, %v; want PLAIN only (both files' references honoured)", got, err)
	}
	if _, err := containerEnvFor("/t", func(string) (map[string]string, error) { return nil, boom }, dir, read); !errors.Is(err, boom) {
		t.Errorf("load error = %v", err)
	}
	if _, err := containerEnvFor("/t", load, func(string) ([]os.DirEntry, error) { return nil, boom }, read); !errors.Is(err, boom) {
		t.Errorf("list error = %v", err)
	}
	if _, err := containerEnvFor("/t", load, dir, func(string) ([]byte, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Errorf("read error = %v", err)
	}
	got, err = containerEnvFor("/t", load, func(string) ([]os.DirEntry, error) { return nil, fs.ErrNotExist }, read)
	if err != nil || got["PLAIN"] != "v" || len(got) != 3 {
		t.Errorf("a town without a settings directory: %v, %v", got, err)
	}
}

func TestContainerKubeconfigMakesARelativeCAFileAbsolute(t *testing.T) {
	t.Parallel()
	out, err := containerKubeconfig([]byte("clusters:\n- name: a\n  cluster: {server: 'https://127.0.0.1:1', certificate-authority: ca.crt}\n"), "/towns/t/.runtime/sbx-k8s")
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
