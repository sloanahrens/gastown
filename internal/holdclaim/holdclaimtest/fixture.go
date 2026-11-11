package holdclaimtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The daemon.env keys of the pod setting. They are the names holdclaim reads
// (holdclaim.ContextEnv and holdclaim.NamespaceEnv), spelled out here because
// this package cannot import holdclaim.
const (
	contextKey   = "GT_DAEMON_POD_CONTEXT"
	namespaceKey = "GT_DAEMON_POD_NAMESPACE"
)

// WritePodSetting writes settings/daemon.env under townRoot with the pod setting:
// the kubectl context and namespace the daemon Deployment is reached through. An
// empty value leaves its key out, so a test can write a setting given in part,
// and writing both empty writes a town with no pod setting.
func WritePodSetting(t testing.TB, townRoot, kubeContext, namespace string) {
	t.Helper()
	var body strings.Builder
	if kubeContext != "" {
		body.WriteString(contextKey + "=" + kubeContext + "\n")
	}
	if namespace != "" {
		body.WriteString(namespaceKey + "=" + namespace + "\n")
	}
	dir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "daemon.env"), []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
