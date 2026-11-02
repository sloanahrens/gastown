// Command gaterunner prints the landing-gate Forgejo runner's Kubernetes
// manifests to stdout, built from flags, so an operator can review and
// kubectl apply them:
//
//	go run ./cmd/gaterunner -label gate:docker://data.forgejo.org/oci/gastown-runner:go1.27-node20 \
//	    -forgejo-host-ip 192.168.5.2 > runner.yaml
//
// It renders deploy/k8s/forgejo-runner's Values and prints the package's
// multi-document stream: both claims, the ConfigMap and the Deployment. It
// never contacts a cluster, reads a token or writes a file. It exits 0 after
// writing, 2 for a wrong command line or a Values the package refuses, and 1
// when the stream cannot be written.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	forgejorunner "github.com/steveyegge/gastown/deploy/k8s/forgejo-runner"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run renders the manifests from args and writes them to stdout. It returns
// the process exit code: 0 on success, 2 for a refused command line or Values,
// and 1 when stdout cannot be written.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gaterunner", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var labels labelList
	fs.Var(&labels, "label", "runner label as name:docker://image; repeat for each label (at least one required)")
	forgejoHostIP := fs.String("forgejo-host-ip", "", "IPv4 address the name forgejo resolves to in the pod and its job containers (required)")
	capacity := fs.Int("capacity", 0, "most jobs the runner runs at once; the package default when unset")
	namespace := fs.String("namespace", "", "namespace every object lands in; the package default when unset")
	dindStorage := fs.String("dind-storage", "", "engine claim size, such as 40Gi; the package default when unset")
	runnerStorage := fs.String("runner-storage", "", "runner data claim size, such as 1Gi; the package default when unset")
	runnerCPU := fs.String("runner-cpu", "", "runner container CPU share; the package default when unset")
	runnerMemory := fs.String("runner-memory", "", "runner container memory share; the package default when unset")
	dindCPU := fs.String("dind-cpu", "", "engine sidecar CPU share; the package default when unset")
	dindMemory := fs.String("dind-memory", "", "engine sidecar memory share; the package default when unset")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "gaterunner: unexpected argument %q: every input is a flag\n", fs.Arg(0))
		return 2
	}
	if len(labels) == 0 {
		fmt.Fprintln(stderr, "gaterunner: -label is required: name at least one runner label, as name:docker://image")
		return 2
	}
	if *forgejoHostIP == "" {
		fmt.Fprintln(stderr, "gaterunner: -forgejo-host-ip is required: name the host gateway address the forgejo alias resolves to")
		return 2
	}

	manifests, err := forgejorunner.Build(forgejorunner.Values{
		Namespace:     *namespace,
		Labels:        labels,
		Capacity:      *capacity,
		ForgejoHostIP: *forgejoHostIP,
		DindStorage:   *dindStorage,
		RunnerStorage: *runnerStorage,
		RunnerResources: forgejorunner.Share{
			CPU:    *runnerCPU,
			Memory: *runnerMemory,
		},
		DindResources: forgejorunner.Share{
			CPU:    *dindCPU,
			Memory: *dindMemory,
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "gaterunner: %v\n", err)
		return 2
	}
	if err := forgejorunner.Render(stdout, manifests); err != nil {
		fmt.Fprintf(stderr, "gaterunner: writing the manifests: %v\n", err)
		return 1
	}
	return 0
}

// labelList collects a repeatable -label flag, keeping the order given.
type labelList []string

func (l *labelList) String() string { return strings.Join(*l, ",") }

func (l *labelList) Set(value string) error {
	*l = append(*l, value)
	return nil
}
