package imageprune

import (
	"errors"
	"fmt"
	"sort"
)

// Crictl runs crictl on one node with the given arguments and returns its
// standard output. The command binds it to a node; tests pass a fake.
type Crictl func(args ...string) ([]byte, error)

// CrictlTimeout is the deadline every crictl call carries. Measured 2026-11-20
// on the control node (colima gt-control, k3s): with crictl's default 2 second
// deadline, reading its 208 images failed one run in three, each call taking
// 1.2 to 2.3 seconds.
const CrictlTimeout = "120s"

// Timed returns a Crictl that runs c with an explicit deadline. The flag goes
// before the call's own arguments, where crictl accepts a global option.
func Timed(c Crictl) Crictl {
	return func(args ...string) ([]byte, error) {
		return c(append([]string{"--timeout", CrictlTimeout}, args...)...)
	}
}

// NodeResult is what pruning one node did. Err is set when the node could not
// be read (its images, a creation time that was needed, or its containers): the
// node is then left alone, which is the fail-closed rule. Removed and Failed
// hold per-image outcomes once the node was read.
type NodeResult struct {
	Node    string
	Planned []Image
	Removed []string
	Failed  map[string]error
	Err     error
}

// Ok reports whether the node was read and every planned removal succeeded.
func (r NodeResult) Ok() bool {
	return r.Err == nil && len(r.Failed) == 0
}

// ErrNoNodes is returned when there are no nodes to prune: an empty node list
// is refused rather than treated as a clean run.
var ErrNoNodes = errors.New("no kind nodes found to prune")

// PruneNode plans and removes one node's old images. It reads the images, the
// creation time of each (an image whose time cannot be read is kept), and the
// containers in use; if any of the first two reads fails, nothing is removed.
func PruneNode(node string, crictl Crictl, keep int, dry bool) NodeResult {
	return pruneNode(node, crictl, dry, func(images []Image, inUse map[string]bool) []Image {
		return Plan(images, inUse, keep)
	})
}

// PruneNodeRepos is PruneNode over several repositories: each keeps its newest
// keep images (PlanRepos). Every crictl call goes through Timed, so each read
// carries the explicit deadline the 208-image node needs (gt-hhi6a5).
func PruneNodeRepos(node string, crictl Crictl, keep int, repos []string, dry bool) NodeResult {
	return pruneNode(node, Timed(crictl), dry, func(images []Image, inUse map[string]bool) []Image {
		return PlanRepos(images, inUse, keep, repos)
	})
}

// pruneNode is the read, plan and remove run that PruneNode and PruneNodeRepos
// share; plan chooses the images to remove from the reads.
func pruneNode(node string, crictl Crictl, dry bool, plan func([]Image, map[string]bool) []Image) NodeResult {
	res := NodeResult{Node: node, Failed: map[string]error{}}
	raw, err := crictl("images", "-o", "json")
	if err != nil {
		res.Err = fmt.Errorf("list images: %w", err)
		return res
	}
	images, err := ParseImages(raw)
	if err != nil {
		res.Err = err
		return res
	}
	for i := range images {
		insp, err := crictl("inspecti", "-o", "json", images[i].ID)
		if err != nil {
			images[i].Unknown = true
			continue
		}
		if images[i].Created, err = ParseCreated(insp); err != nil {
			images[i].Unknown = true
		}
	}
	ps, err := crictl("ps", "-a", "-o", "json")
	if err != nil {
		res.Err = fmt.Errorf("list containers: %w", err)
		return res
	}
	inUse, err := InUse(ps)
	if err != nil {
		res.Err = err
		return res
	}
	res.Planned = plan(images, inUse)
	if dry {
		return res
	}
	res.Removed, res.Failed = RemoveAll(res.Planned, func(id string) error {
		_, err := crictl("rmi", id)
		return err
	})
	return res
}

// PruneAll prunes each node in order and returns one result per node. A node
// that cannot be read does not stop the others. An empty node list is an error.
func PruneAll(nodes []string, crictlFor func(node string) Crictl, keep int, dry bool) ([]NodeResult, error) {
	if len(nodes) == 0 {
		return nil, ErrNoNodes
	}
	sorted := append([]string(nil), nodes...)
	sort.Strings(sorted)
	results := make([]NodeResult, 0, len(sorted))
	for _, n := range sorted {
		results = append(results, PruneNode(n, crictlFor(n), keep, dry))
	}
	return results, nil
}

// AllOk reports whether every node was read and every removal succeeded.
func AllOk(results []NodeResult) bool {
	for _, r := range results {
		if !r.Ok() {
			return false
		}
	}
	return true
}
