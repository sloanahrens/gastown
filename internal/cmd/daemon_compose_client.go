package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// The container daemon's client configuration (gt-yj0vh): the environment the
// launchd daemon gets from its plist, and a kubeconfig that reaches the kind API
// server through Docker Desktop's host.docker.internal.

// containerEnvDeniedNames are the daemon.env entries that name the host's own
// layout (its PATH, SDK and temporary directories); the container has its own.
var containerEnvDeniedNames = map[string]bool{
	"PATH": true, "SDKROOT": true, "DEVELOPER_DIR": true, "GOTMPDIR": true, "TMPDIR": true,
}

// containerEnvDeniedPrefix names the terminal multiplexer's per-session entries.
const containerEnvDeniedPrefix = "CMUX_"

// settingsEnvRef is a reference to an environment name in either syntax,
// ${NAME} or $NAME: a secret the settings resolve by name from daemon.env or the
// process, never one the compose file carries.
var settingsEnvRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// containerEnvSecretName matches the daemon.env names that hold a credential.
// Such an entry is never rendered, referenced or not.
var containerEnvSecretName = regexp.MustCompile(`(?i)token|secret|passw|credential|api[_-]?key|private`)

// containerEnv is the part of daemon.env the container's environment carries:
// every entry except the host-only ones (containerEnvDeniedNames and
// containerEnvDeniedPrefix are the one list), any whose name is secret-like, and
// any whose name some settings file, or another daemon.env value, references in
// either syntax. A referenced secret stays resolved by the daemon's own read of
// settings/daemon.env over the bind, so its value is never written to the
// compose file.
func containerEnv(daemonEnv map[string]string, settingsFiles [][]byte) map[string]string {
	refs := map[string]bool{}
	scan := func(text []byte) {
		for _, m := range settingsEnvRef.FindAllSubmatch(text, -1) {
			refs[string(m[1])+string(m[2])] = true
		}
	}
	for _, f := range settingsFiles {
		scan(f)
	}
	for _, v := range daemonEnv {
		scan([]byte(v))
	}
	out := map[string]string{}
	for name, value := range daemonEnv {
		if containerEnvDeniedNames[name] || strings.HasPrefix(name, containerEnvDeniedPrefix) ||
			containerEnvSecretName.MatchString(name) || refs[name] {
			continue
		}
		out[name] = value
	}
	return out
}

// containerEnvFor loads the town's daemon.env and every regular file in its
// settings directory through load, readDir and read, and returns the
// container's environment. A town without a settings directory has no
// references to protect.
func containerEnvFor(townRoot string, load func(string) (map[string]string, error),
	readDir func(string) ([]os.DirEntry, error), read func(string) ([]byte, error)) (map[string]string, error) {
	env, err := load(townRoot)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(townRoot, "settings")
	entries, err := readDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("list the town's settings to find the secrets they reference: %w", err)
	}
	var files [][]byte
	for _, e := range entries {
		if !e.Type().IsRegular() || e.Name() == "daemon.env" {
			continue
		}
		data, err := read(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read the setting %s to find the secrets it references: %w", e.Name(), err)
		}
		files = append(files, data)
	}
	return containerEnv(env, files), nil
}

// Container kubeconfig facts: the host name Docker Desktop gives the Mac, and
// the name the kind API server's certificate carries (it names localhost and
// 127.0.0.1, not host.docker.internal).
const (
	containerKubeHost     = "host.docker.internal"
	containerKubeTLSName  = "localhost"
	containerKubeconfigRP = ".runtime/container/kubeconfig.yaml"
)

// kubeconfigFile is the part of a kubeconfig the derived one reads. Credentials
// are not in it: a decode into this type never holds a client key.
type kubeconfigFile struct {
	Clusters []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server string `yaml:"server"`
			CAData string `yaml:"certificate-authority-data,omitempty"`
			CAFile string `yaml:"certificate-authority,omitempty"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
}

// containerKubeconfig derives, from the host's kubeconfig, a file holding each
// cluster with its server on host.docker.internal and tls-server-name
// localhost. It holds no user and no credential: kubectl merges a KUBECONFIG list
// by name, first file first, so this file is listed before the original, whose
// contexts and users (and their credentials, by the original's own file) apply
// unchanged. The certificate authority is public and is copied with its cluster;
// a certificate-authority file path is made absolute against dir, the original's
// directory, because the derived file lives in another one.
func containerKubeconfig(original []byte, dir string) ([]byte, error) {
	var in kubeconfigFile
	if err := yaml.Unmarshal(original, &in); err != nil {
		return nil, fmt.Errorf("the kubeconfig does not parse: %w", err)
	}
	if len(in.Clusters) == 0 {
		return nil, errors.New("the kubeconfig names no cluster")
	}
	type cluster struct {
		Server        string `yaml:"server"`
		TLSServerName string `yaml:"tls-server-name"`
		CAData        string `yaml:"certificate-authority-data,omitempty"`
		CAFile        string `yaml:"certificate-authority,omitempty"`
	}
	type entry struct {
		Name    string  `yaml:"name"`
		Cluster cluster `yaml:"cluster"`
	}
	out := struct {
		APIVersion string  `yaml:"apiVersion"`
		Kind       string  `yaml:"kind"`
		Clusters   []entry `yaml:"clusters"`
	}{APIVersion: "v1", Kind: "Config"}
	for _, c := range in.Clusters {
		u, err := url.Parse(c.Cluster.Server)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("cluster %q has server %q, which is not a URL with a host", c.Name, c.Cluster.Server)
		}
		u.Host = containerKubeHost + portSuffix(u.Port())
		caFile := c.Cluster.CAFile
		if caFile != "" && !filepath.IsAbs(caFile) {
			caFile = filepath.Join(dir, caFile)
		}
		out.Clusters = append(out.Clusters, entry{Name: c.Name, Cluster: cluster{
			Server: u.String(), TLSServerName: containerKubeTLSName, CAData: c.Cluster.CAData, CAFile: caFile,
		}})
	}
	// The value is plain strings and slices of them, which yaml.Marshal cannot
	// refuse.
	data, _ := yaml.Marshal(out)
	return data, nil
}

func portSuffix(port string) string {
	if port == "" {
		return ""
	}
	return ":" + port
}

// containerKubeconfigPath is where the derived kubeconfig is written: under the
// town's .runtime, which the town bind already carries into the container.
func containerKubeconfigPath(townRoot string) string {
	return filepath.Join(townRoot, filepath.FromSlash(containerKubeconfigRP))
}
