package sweeprunner

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// envInClusterHost is the variable the kubelet sets in every pod, the standard
// client-go signal that a process runs in a cluster.
const envInClusterHost = "KUBERNETES_SERVICE_HOST"

// InClusterWithoutContext reports whether the kubectl steps run on the pod's
// service account instead of a named context: the process runs in a cluster
// (environ holds KUBERNETES_SERVICE_HOST) and no kubeconfig names context. A
// kubeconfig that does name it wins, so the Mac, and a pod given a kubeconfig,
// keep the context they were told to use (gt-2by0ke). The caller sets
// Spec.InCluster from it once, when it builds the Spec.
func InClusterWithoutContext(environ []string, context string, hasContext func(name string) bool) bool {
	host := ""
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == envInClusterHost {
			host = v
		}
	}
	return host != "" && !hasContext(context)
}

// KubeconfigHasContext returns a lookup that reads the kubeconfig files environ
// names (KUBECONFIG, a path list, else HOME/.kube/config) with read, and says
// whether any of them names a context. A file that cannot be read or parsed
// names none.
func KubeconfigHasContext(environ []string, read func(path string) ([]byte, error)) func(name string) bool {
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	var paths []string
	if list := env["KUBECONFIG"]; list != "" {
		paths = filepath.SplitList(list)
	} else if home := env["HOME"]; home != "" {
		paths = []string{filepath.Join(home, ".kube", "config")}
	}
	return func(name string) bool {
		for _, p := range paths {
			data, err := read(p)
			if err != nil {
				continue
			}
			var cfg struct {
				Contexts []struct {
					Name string `yaml:"name"`
				} `yaml:"contexts"`
			}
			if yaml.Unmarshal(data, &cfg) != nil {
				continue
			}
			for _, c := range cfg.Contexts {
				if c.Name == name {
					return true
				}
			}
		}
		return false
	}
}

// HostInClusterWithoutContext is InClusterWithoutContext for this process: its
// environment and the kubeconfig files on its disk.
func HostInClusterWithoutContext(context string) bool {
	environ := os.Environ()
	return InClusterWithoutContext(environ, context, KubeconfigHasContext(environ, os.ReadFile))
}
