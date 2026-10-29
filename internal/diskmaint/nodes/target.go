// Package nodes prunes old gastown-polecat images from the sandbox cluster's
// kind nodes as a diskmaint target. Which images to remove is
// internal/polecat/imageprune's decision; this package supplies the docker and
// crictl commands, turns that decision into a plan, and removes exactly the
// images the plan named.
//
// The target is the fold of cmd/nodeprune into gt maint (gt-z6qre.6, slice 3).
package nodes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
	"github.com/steveyegge/gastown/internal/polecat/imageprune"
)

// planTarget is the Target name a plan and a ledger entry carry.
const planTarget = "nodes"

// actionRemove is the only Action kind this target's plans carry; Apply
// refuses anything else.
const actionRemove = "remove-node-image"

// Defaults for the docker context and the kind node name prefix, the same ones
// cmd/nodeprune uses.
const (
	defaultContext    = "colima-gt-sbx"
	defaultNodePrefix = "gt-sbx-"
)

// ruleUnreadable is the Kept rule for a node whose images could not be read.
// It names the rule and not the read's error, which carries the command's
// output.
const ruleUnreadable = "node unreadable; left alone"

// removeFailed is the Failure reason for an rmi that did not succeed. It names
// the command rather than quoting its output.
const removeFailed = "crictl rmi failed; the image is still on the node"

var (
	// errNoLedger reports a target built without a ledger. Every apply must
	// leave a record, so a run that cannot write one does not start.
	errNoLedger = errors.New("nodes apply: no ledger configured")
	// errNoRunner reports a target with no docker runner; nothing can be read
	// or removed, so a plan refuses and an apply fails rather than reporting a
	// quiet run.
	errNoRunner = errors.New("no docker runner configured")
	// errListNodes reports a failed node listing. The runner's error carries the
	// command's stderr, so it is dropped in favor of this fixed reason.
	errListNodes = errors.New("list kind nodes failed")
	// errBadRef reports a plan action whose ref is not <node>/<image digest>.
	errBadRef = errors.New("ref is not <node>/sha256:<64 hex>")
)

// Ledger is what Apply records a run in. diskmaint.Ledger is the production
// implementation; a test substitutes one that fails, since a full disk cannot
// be arranged hermetically.
type Ledger interface {
	Append(diskmaint.LedgerEntry) error
}

// Compile-time check that the nodes target satisfies the diskmaint contract.
var _ diskmaint.Target = (*Target)(nil)

// Config is what the nodes target needs to measure, plan and apply.
type Config struct {
	// Context is the docker context of the VM the kind nodes run in; blank uses
	// colima-gt-sbx.
	Context string
	// NodePrefix is the name prefix of the kind node containers; blank uses
	// gt-sbx-.
	NodePrefix string
	// Run runs docker and, through it, crictl on each node.
	Run diskmaint.Runner
	// Ledger records every apply; Apply refuses to run without one.
	Ledger Ledger
	// Now is the clock a ledger entry is stamped with; nil uses the system
	// clock.
	Now func() time.Time
}

// Target prunes old gastown-polecat images from the sandbox cluster's nodes.
type Target struct {
	cfg Config
}

// NewTarget returns a Target that reads the nodes named by cfg.
func NewTarget(cfg Config) *Target {
	if cfg.Context == "" {
		cfg.Context = defaultContext
	}
	if cfg.NodePrefix == "" {
		cfg.NodePrefix = defaultNodePrefix
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Target{cfg: cfg}
}

// Name is the target's name in plans and ledger entries.
func (t *Target) Name() string { return planTarget }

// Measure reports how many kind nodes the target would prune. crictl reports no
// image sizes, so Used stays 0 and the Detail says the bytes are not measured
// rather than claiming the nodes hold nothing.
func (t *Target) Measure(ctx context.Context) (diskmaint.Measurement, error) {
	names, err := t.nodes(ctx)
	if err != nil {
		return diskmaint.Measurement{}, fmt.Errorf("measure: %w", err)
	}
	return diskmaint.Measurement{
		Detail: fmt.Sprintf("%d kind nodes under docker context %s; image bytes are not measured", len(names), t.cfg.Context),
	}, nil
}

// Plan reads the kind nodes' images and returns imageprune's decision for them.
// Each planned image becomes a remove-node-image action whose Ref names its
// node; an image that is kept is not listed. A node that cannot be read is Kept
// as unreadable and gets no actions, and the other nodes are still planned. A
// failed listing, or no nodes at all, refuses the plan rather than letting it
// read as a clean run.
func (t *Target) Plan(ctx context.Context, p diskmaint.Policy) (diskmaint.Plan, error) {
	plan := diskmaint.Plan{Target: planTarget}
	names, err := t.nodes(ctx)
	if err != nil {
		plan.Refused = t.refusal(err)
		return plan, nil
	}
	results, err := imageprune.PruneAll(names, func(node string) imageprune.Crictl {
		return func(args ...string) ([]byte, error) { return t.exec(ctx, node, args) }
	}, p.Keep, true)
	if err != nil {
		// PruneAll's only error is imageprune.ErrNoNodes: the listing worked but
		// named no node, which is refused rather than treated as a clean run.
		plan.Refused = fmt.Sprintf("no kind nodes named %s* under docker context %s", t.cfg.NodePrefix, t.cfg.Context)
		return plan, nil
	}
	for _, r := range results {
		if r.Err != nil {
			plan.Kept = append(plan.Kept, diskmaint.Kept{Ref: r.Node, Rule: ruleUnreadable})
			continue
		}
		for _, im := range r.Planned {
			plan.Actions = append(plan.Actions, diskmaint.Action{
				Kind: actionRemove,
				Ref:  r.Node + "/" + im.ID,
				Why:  keepRule(p.Keep),
			})
		}
	}
	return plan, nil
}

// Apply removes exactly the images the plan named, node by node, and records
// the run as one ledger line. It does not re-plan: an image the live state
// would now select but the plan did not name is left alone, so what was shown
// in the dry run is what is removed.
//
// Every action is validated before any command runs for its node; an action
// whose ref does not validate becomes a Failure and touches nothing. One failed
// removal does not stop the others. A refused or empty plan has nothing to
// remove and is recorded as such.
func (t *Target) Apply(ctx context.Context, plan diskmaint.Plan) (diskmaint.Result, error) {
	result := diskmaint.Result{Target: planTarget}
	if t.cfg.Ledger == nil {
		return result, errNoLedger
	}
	var errs []string
	var err error
	if plan.Refused == "" && len(plan.Actions) > 0 {
		if t.cfg.Run == nil {
			errs = append(errs, errNoRunner.Error())
			err = errNoRunner
		} else {
			errs = t.removePlanned(ctx, plan.Actions, &result)
		}
	}
	if recErr := t.record(plan, result, errs); recErr != nil {
		err = errors.Join(err, recErr)
	}
	return result, err
}

// nodes lists the kind node containers in the docker context. The name filter
// is a substring match, so a name is accepted only when it starts with the
// prefix, and only when it is a container name that may become a command
// argument. A listing that cannot be read is errListNodes, never an empty list.
func (t *Target) nodes(ctx context.Context) ([]string, error) {
	if t.cfg.Run == nil {
		return nil, errNoRunner
	}
	out, err := t.docker(ctx, []string{"ps", "--filter", "name=" + t.cfg.NodePrefix, "--format", "{{.Names}}"})
	if err != nil {
		return nil, errListNodes
	}
	var names []string
	for _, name := range strings.Fields(string(out)) {
		if strings.HasPrefix(name, t.cfg.NodePrefix) && registry.ValidContainer(name) {
			names = append(names, name)
		}
	}
	return names, nil
}

// refusal is a plan's reason when the node listing failed.
func (t *Target) refusal(err error) string {
	if errors.Is(err, errNoRunner) {
		return errNoRunner.Error()
	}
	return fmt.Sprintf("list kind nodes failed: docker --context %s ps", t.cfg.Context)
}

// keepRule is why an image the plan removes is beyond what the policy keeps.
func keepRule(keep int) string {
	if keep <= 0 {
		return "not in use and not the dev tag; the policy keeps no image by age"
	}
	return fmt.Sprintf("older than the newest %d gastown-polecat images, not in use, not dev", keep)
}

// nodeGroup is the plan actions that name one node, in plan order.
type nodeGroup struct {
	node    string
	actions []diskmaint.Action
}

// groupByNode groups actions by the node their ref names, keeping the order the
// nodes first appear in the plan.
func groupByNode(actions []diskmaint.Action) []nodeGroup {
	index := map[string]int{}
	groups := make([]nodeGroup, 0, len(actions))
	for _, a := range actions {
		node, _, _ := strings.Cut(a.Ref, "/")
		i, ok := index[node]
		if !ok {
			i = len(groups)
			index[node] = i
			groups = append(groups, nodeGroup{node: node})
		}
		groups[i].actions = append(groups[i].actions, a)
	}
	return groups
}

// removePlanned removes the named images, one node at a time, and returns the
// reasons to record. Only the planned image ids reach crictl; a node the plan
// does not name is never contacted.
func (t *Target) removePlanned(ctx context.Context, actions []diskmaint.Action, result *diskmaint.Result) []string {
	var errs []string
	for _, group := range groupByNode(actions) {
		images := make([]imageprune.Image, 0, len(group.actions))
		byID := make(map[string]diskmaint.Action, len(group.actions))
		// node is the name the actions validated, so the command below runs
		// against a name that passed validation rather than the group key.
		var node string
		for _, a := range group.actions {
			n, im, err := actionImage(a)
			if err != nil {
				result.Failed = append(result.Failed, diskmaint.Failure{Action: a, Err: err.Error()})
				// A ref that does not parse is plan text nobody validated, so
				// the record names the node only when it is a real one.
				errs = append(errs, fmt.Sprintf("%s: %v", nodeLabel(group.node), err))
				continue
			}
			node = n
			images = append(images, im)
			byID[im.ID] = a
		}
		if len(images) == 0 {
			continue
		}
		removed, failed := imageprune.RemoveAll(images, func(id string) error {
			_, err := t.exec(ctx, node, []string{"rmi", id})
			return err
		})
		for _, id := range removed {
			result.Done = append(result.Done, byID[id])
		}
		for id := range failed {
			result.Failed = append(result.Failed, diskmaint.Failure{Action: byID[id], Err: removeFailed})
			errs = append(errs, fmt.Sprintf("%s: remove %s failed; the image is still on the node", group.node, id))
		}
	}
	return errs
}

// nodeLabel names a node in a record, or says it is not one: a ref that failed
// validation may carry anything in that field.
func nodeLabel(node string) string {
	if registry.ValidContainer(node) {
		return node
	}
	return "an unnamed node"
}

// actionImage validates one plan action and returns the node and image it
// names. The refusal does not quote the ref: it is plan text no validator has
// passed.
func actionImage(a diskmaint.Action) (string, imageprune.Image, error) {
	if a.Kind != actionRemove {
		return "", imageprune.Image{}, fmt.Errorf("action kind is not %s", actionRemove)
	}
	node, id, ok := strings.Cut(a.Ref, "/")
	if !ok || !registry.ValidContainer(node) || !registry.ValidDigest(id) {
		return "", imageprune.Image{}, errBadRef
	}
	return node, imageprune.Image{ID: id}, nil
}

// record appends the run's one ledger line.
func (t *Target) record(plan diskmaint.Plan, result diskmaint.Result, errs []string) error {
	err := t.cfg.Ledger.Append(diskmaint.LedgerEntry{
		TS:         t.cfg.Now().UTC(),
		Target:     planTarget,
		Mode:       "apply",
		Planned:    len(plan.Actions),
		Done:       len(result.Done),
		BytesFreed: result.BytesFreed,
		Refused:    plan.Refused,
		Deferred:   result.Deferred,
		Errors:     errs,
	})
	if err != nil {
		return fmt.Errorf("record the apply: %w", err)
	}
	return nil
}

// exec runs crictl on one node through docker exec.
func (t *Target) exec(ctx context.Context, node string, args []string) ([]byte, error) {
	argv := make([]string, 0, 3+len(args))
	argv = append(argv, "exec", node, "crictl")
	argv = append(argv, args...)
	return t.docker(ctx, argv)
}

// docker runs docker in the configured context with args.
func (t *Target) docker(ctx context.Context, args []string) ([]byte, error) {
	argv := make([]string, 0, 2+len(args))
	argv = append(argv, "--context", t.cfg.Context)
	argv = append(argv, args...)
	return t.cfg.Run(ctx, nil, "docker", argv...)
}
