// Command nodeprune removes old gastown-polecat images from the sandbox cluster's
// kind nodes after a rebuild (gt-e14j6). The choice of images and the fail-closed
// rules are in internal/polecat/imageprune; this command runs crictl on each node
// through docker exec and reports every node.
//
// Defaults: the docker context is the Colima profile this town's sandbox cluster
// runs in (colima-gt-sbx), the node prefix is gt-sbx-, and -keep is 3 (the plugin's
// POLECAT_IMAGE_KEEP). -keep 0 keeps only images in use and the dev tag. The exit
// status is 0 only when every node was read and every removal succeeded; a node
// that could not be read is left alone and reported.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/polecat/imageprune"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, out, errw *os.File) int {
	fs := flag.NewFlagSet("nodeprune", flag.ContinueOnError)
	fs.SetOutput(errw)
	context := fs.String("context", "colima-gt-sbx", "the docker context of the VM the kind nodes run in")
	prefix := fs.String("nodes", "gt-sbx-", "name prefix of the kind node containers")
	keep := fs.Int("keep", 3, "newest gastown-polecat images to keep on each node; 0 keeps only images in use and the dev tag")
	dry := fs.Bool("dry-run", false, "print what would be removed and remove nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *keep < 0 {
		fmt.Fprintf(errw, "nodeprune: -keep must be 0 or more, got %d\n", *keep)
		return 2
	}
	names, err := docker(*context, "ps", "--filter", "name="+*prefix, "--format", "{{.Names}}")
	if err != nil {
		fmt.Fprintf(errw, "nodeprune: list nodes: %v\n", err)
		return 1
	}
	nodes := strings.Fields(string(names))
	results, err := imageprune.PruneAll(nodes, func(node string) imageprune.Crictl {
		return func(args ...string) ([]byte, error) {
			return docker(*context, append([]string{"exec", node, "crictl"}, args...)...)
		}
	}, *keep, *dry)
	if err != nil {
		fmt.Fprintf(errw, "nodeprune: %v (matching %q under context %s)\n", err, *prefix, *context)
		return 1
	}
	report(out, errw, results, *dry)
	if !imageprune.AllOk(results) {
		return 1
	}
	return 0
}

// report prints one block per node: what it planned, what it removed, and any
// failure named with its node, so no failure reads as success.
func report(out, errw *os.File, results []imageprune.NodeResult, dry bool) {
	for _, r := range results {
		switch {
		case r.Err != nil:
			fmt.Fprintf(errw, "nodeprune: %s: NOT PRUNED, could not be read: %v\n", r.Node, r.Err)
			continue
		case dry:
			for _, im := range r.Planned {
				fmt.Fprintf(out, "nodeprune: %s: would remove %s %v\n", r.Node, im.ID, im.Digests)
			}
		default:
			for _, id := range r.Removed {
				fmt.Fprintf(out, "nodeprune: %s: removed %s\n", r.Node, id)
			}
		}
		for id, err := range r.Failed {
			fmt.Fprintf(errw, "nodeprune: %s: FAILED to remove %s: %v (kept)\n", r.Node, id, err)
		}
	}
}

func docker(context string, args ...string) ([]byte, error) {
	cmd := exec.Command("docker", append([]string{"--context", context}, args...)...) //nolint:gosec // G204: argv is docker --context <operator flag> then fixed verbs and args, with node names from docker ps; no shell, and no bead or user text reaches it
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}
