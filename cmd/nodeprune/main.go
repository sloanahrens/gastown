// Command nodeprune removes old gastown-polecat images from the sandbox cluster's
// kind nodes after a rebuild (gt-e14j6). The choice of images is in
// internal/polecat/imageprune; this runs crictl on each node through docker exec.
// A failed removal is logged and the run goes on; the image stays on the node.
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
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("nodeprune", flag.ContinueOnError)
	context := fs.String("context", "colima-gt-sbx", "the docker context of the VM the kind nodes run in")
	prefix := fs.String("nodes", "gt-sbx-", "name prefix of the kind node containers")
	keep := fs.Int("keep", 3, "newest gastown-polecat images to keep on each node (POLECAT_IMAGE_KEEP)")
	dry := fs.Bool("dry-run", false, "print what would be removed and remove nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	names, err := docker(*context, "ps", "--filter", "name="+*prefix, "--format", "{{.Names}}")
	if err != nil {
		fmt.Fprintf(os.Stderr, "nodeprune: list nodes: %v\n", err)
		return 1
	}
	for _, node := range strings.Fields(string(names)) {
		pruneNode(*context, node, *keep, *dry)
	}
	return 0
}

func pruneNode(context, node string, keep int, dry bool) {
	raw, err := crictl(context, node, "images", "-o", "json")
	if err != nil {
		fmt.Printf("nodeprune: %s: list images: %v\n", node, err)
		return
	}
	images, err := imageprune.ParseImages(raw)
	if err != nil {
		fmt.Printf("nodeprune: %s: %v\n", node, err)
		return
	}
	for i := range images {
		insp, err := crictl(context, node, "inspecti", "-o", "json", images[i].ID)
		if err != nil {
			fmt.Printf("nodeprune: %s: inspect %s: %v (kept)\n", node, images[i].ID, err)
			images[i].Unknown = true
			continue
		}
		if images[i].Created, err = imageprune.ParseCreated(insp); err != nil {
			fmt.Printf("nodeprune: %s: %s: %v (kept)\n", node, images[i].ID, err)
			images[i].Unknown = true
		}
	}
	ps, err := crictl(context, node, "ps", "-a", "-o", "json")
	if err != nil {
		fmt.Printf("nodeprune: %s: list containers: %v (nothing removed)\n", node, err)
		return
	}
	inUse, err := imageprune.InUse(ps)
	if err != nil {
		fmt.Printf("nodeprune: %s: %v (nothing removed)\n", node, err)
		return
	}
	plan := imageprune.Plan(images, inUse, keep)
	if dry {
		for _, im := range plan {
			fmt.Printf("nodeprune: %s: would remove %s %v\n", node, im.ID, im.Digests)
		}
		return
	}
	removed, failed := imageprune.RemoveAll(plan, func(id string) error {
		_, err := crictl(context, node, "rmi", id)
		return err
	})
	for _, id := range removed {
		fmt.Printf("nodeprune: %s: removed %s\n", node, id)
	}
	for id, err := range failed {
		fmt.Printf("nodeprune: %s: remove %s: %v (kept)\n", node, id, err)
	}
}

func crictl(context, node string, args ...string) ([]byte, error) {
	return docker(context, append([]string{"exec", node, "crictl"}, args...)...)
}

func docker(context string, args ...string) ([]byte, error) {
	cmd := exec.Command("docker", append([]string{"--context", context}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}
