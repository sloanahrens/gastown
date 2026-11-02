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
	"time"

	"github.com/steveyegge/gastown/internal/config"
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
			config.SecretNamePattern.MatchString(name) || refs[name] {
			continue
		}
		out[name] = value
	}
	return out
}

// settingsFS is the filesystem the secret-reference scan reads through; a test
// stands in its own.
type settingsFS struct {
	readDir func(string) ([]os.DirEntry, error)
	read    func(string) ([]byte, error)
	eval    func(string) (string, error)
}

// containerEnvFor loads the town's daemon.env and reads every file under its
// settings directory, subdirectories included, through fsys, and returns the
// container's environment. The scan covers that directory only (the other
// directories the compose binds are not scanned). A skipped file is an
// unscanned file, so nothing is skipped: a link is resolved and its target read
// when the target stays under the town, and a link that points outside the
// town, cannot be resolved, or an entry that is neither a file, a directory nor
// a link, refuses the whole compose. A town without a settings directory has no
// references to protect.
func containerEnvFor(townRoot string, load func(string) (map[string]string, error), fsys settingsFS) (map[string]string, error) {
	env, err := load(townRoot)
	if err != nil {
		return nil, err
	}
	town, err := fsys.eval(townRoot)
	if err != nil {
		return nil, fmt.Errorf("the town root %s cannot be resolved: %w", townRoot, err)
	}
	var files [][]byte
	var walk func(dir string, top bool) error
	walk = func(dir string, top bool) error {
		entries, err := fsys.readDir(dir)
		if top && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("list %s to find the secrets the settings reference: %w", dir, err)
		}
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			switch {
			case top && e.Name() == "daemon.env":
			case e.Type()&fs.ModeSymlink != 0:
				target, err := fsys.eval(path)
				if err != nil {
					return fmt.Errorf("the setting %s is a link that cannot be resolved, so it cannot be scanned: %w", path, err)
				}
				if !pathWithin(target, town) {
					return fmt.Errorf("the setting %s is a link to %s, outside the town %s, so it cannot be scanned or bound", path, target, town)
				}
				data, err := fsys.read(target)
				if err != nil {
					return fmt.Errorf("read the setting %s (a link to %s): %w", path, target, err)
				}
				files = append(files, data)
			case e.IsDir():
				if err := walk(path, false); err != nil {
					return err
				}
			case e.Type().IsRegular():
				data, err := fsys.read(path)
				if err != nil {
					return fmt.Errorf("read the setting %s to find the secrets it references: %w", path, err)
				}
				files = append(files, data)
			default:
				return fmt.Errorf("the setting %s is neither a file, a directory nor a link, so it cannot be scanned", path)
			}
		}
		return nil
	}
	if err := walk(filepath.Join(townRoot, "settings"), true); err != nil {
		return nil, err
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
// directory, because the derived file lives in another one; one outside the
// town is refused, because the container binds only the town.
func containerKubeconfig(original []byte, dir, townRoot string) ([]byte, error) {
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
		if caFile != "" && !pathWithin(caFile, townRoot) {
			return nil, fmt.Errorf("cluster %q names the certificate authority file %s, outside the town %s, which the container does not bind", c.Name, caFile, townRoot)
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

// containerZone is the IANA zone name the container's TZ carries: the zone
// polecat_pool.timezone names when set, else the zone the host's localtime link
// (localtime, /etc/localtime on a host) points into. A name only, never zone
// data: the gt binary embeds the database (gt-yrxhs). A name that does not load
// as a zone is refused here, as is a host whose zone cannot be read when the
// town names none: a container without TZ runs in UTC and its 04:30 and 03:00
// windows move by hours, so there is no silent default.
func containerZone(townRoot string, load func(path string) (*config.TownSettings, error), readlink func(string) (string, error), localtime string) (string, error) {
	settings, err := load(config.TownSettingsPath(townRoot))
	if err != nil {
		return "", fmt.Errorf("read the town's settings for polecat_pool.timezone: %w", err)
	}
	if settings != nil && settings.PolecatPool != nil && settings.PolecatPool.Timezone != "" {
		return checkedZone(settings.PolecatPool.Timezone, "polecat_pool.timezone")
	}
	target, err := readlink(localtime)
	if err != nil {
		return "", fmt.Errorf("the town sets no polecat_pool.timezone and the host zone cannot be read from %s (%v); set polecat_pool.timezone so the container's clock matches the host's", localtime, err)
	}
	_, name, ok := strings.Cut(target, "zoneinfo/")
	if !ok {
		return "", fmt.Errorf("the town sets no polecat_pool.timezone and %s points at %q, which is not under a zoneinfo directory; set polecat_pool.timezone so the container's clock matches the host's", localtime, target)
	}
	return checkedZone(name, localtime)
}

// checkedZone returns name when it loads as a zone, and refuses it when it does
// not (Go would otherwise fall back to UTC without a word).
func checkedZone(name, source string) (string, error) {
	if _, err := time.LoadLocation(name); err != nil {
		return "", fmt.Errorf("the zone %q from %s does not load: %w", name, source, err)
	}
	return name, nil
}
