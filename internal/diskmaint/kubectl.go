package diskmaint

// SandboxContext is the context the kind bootstrap generates for the sandbox
// cluster, cluster gt-sbx (deploy/k8s/kind, .runtime/sbx-k8s/kubeconfig).
//
// Every cluster call a maintenance target makes names it, so a call never
// depends on which context the kubeconfig file happens to make current: the
// container's derived kubeconfig carries clusters alone, and a kubectl that
// found no context fell back to localhost:8080 (gt-gg9hv.24.6).
const SandboxContext = "kind-gt-sbx"

// KubectlArgs is the prefix every maintenance cluster call starts with: the
// kubeconfig it reads and the context it names. The two callers that run
// cluster clients -- the daemon's cache trim and the registry target's in-use
// read -- build their argv from this one function, so a future call cannot
// carry one of the two flags without the other.
func KubectlArgs(kubeconfig, kubeContext string) []string {
	return []string{"--kubeconfig", kubeconfig, "--context", kubeContext}
}
