package cmd

import (
	"errors"
	"io/fs"
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
	out, err := containerKubeconfig([]byte(hostKubeconfig))
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
		if _, err := containerKubeconfig([]byte(tc.in)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want one containing %q", name, err, tc.want)
		}
	}
	out, err := containerKubeconfig([]byte("clusters:\n- name: a\n  cluster: {server: 'https://api.example'}\n"))
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

func TestContainerEnvNeverEmitsASecretReference(t *testing.T) {
	t.Parallel()
	in := map[string]string{"DEEPSEEK_TOKEN": "sk-secret", "PLAIN": "v"}
	settings := []byte(`{"agents":{"a":{"env":{"ANTHROPIC_AUTH_TOKEN":"${DEEPSEEK_TOKEN}"}}}}`)
	got := containerEnv(in, settings)
	if _, ok := got["DEEPSEEK_TOKEN"]; ok || got["PLAIN"] != "v" {
		t.Errorf("container env = %v, want PLAIN only (the referenced secret stays resolved by name)", got)
	}
}

func TestContainerEnvForReportsWhatItCannotRead(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	load := func(string) (map[string]string, error) { return map[string]string{"A": "1"}, nil }
	if _, err := containerEnvFor("/t", func(string) (map[string]string, error) { return nil, boom }, nil); !errors.Is(err, boom) {
		t.Errorf("load error = %v", err)
	}
	if _, err := containerEnvFor("/t", load, func(string) ([]byte, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Errorf("read error = %v", err)
	}
	got, err := containerEnvFor("/t", load, func(string) ([]byte, error) { return nil, fs.ErrNotExist })
	if err != nil || got["A"] != "1" {
		t.Errorf("a town without settings: %v, %v", got, err)
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
