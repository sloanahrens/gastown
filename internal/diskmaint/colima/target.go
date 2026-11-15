// Package colima removes dangling images from the gt-sbx Colima docker daemon
// as a diskmaint target. It names each image by id rather than running docker's
// own prune, so a dry run lists exactly what goes and the ledger records it.
//
// The target is slice 3 of gt-z6qre; gt maint wires it in with the other targets.
package colima

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/dockerimg"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
)

// planTarget is the Target name a plan and a ledger entry carry.
const planTarget = "colima"

// actionRemove is the only Action kind this target's plans carry; Apply refuses
// anything else.
const actionRemove = "remove-image"

// noneField is how docker prints a repository or tag an image does not have.
const noneField = "<none>"

// Kept rules. They name the rule and not the listing or its output.
const (
	ruleTagged  = "tagged; only dangling images are removed"
	ruleInUse   = "dangling but used by a container; left alone"
	rulePartial = "repository or tag is missing from only one side; left alone"
)

// removeFailed is the Failure reason for an rmi that did not succeed. It names
// the command rather than quoting its output.
const removeFailed = "docker rmi failed; the image is still present"

var (
	// errNoLedger reports a target built without a ledger. Every apply must
	// leave a record, so a run that cannot write one does not start.
	errNoLedger = errors.New("colima apply: no ledger configured")
	// errBadRef reports a plan action whose ref is not sha256:<64 hex>.
	errBadRef = errors.New("ref is not sha256:<64 hex>")
	// errPlanRefused reports an apply of a refused plan. Nothing is removed. The
	// refusal is an error here as well as a ledger line, so a direct caller does
	// not read it as success.
	errPlanRefused = errors.New("colima apply: the plan is refused; nothing was removed")
)

// Compile-time check that the colima target satisfies the diskmaint contract.
var _ diskmaint.Target = (*Target)(nil)

// Config is what the colima target needs to measure, plan and apply.
type Config struct {
	// DockerHost is the daemon socket, passed to every docker call as
	// DOCKER_HOST, e.g. unix://<home>/.colima/gt-sbx/docker.sock.
	DockerHost string
	// Run runs docker on the daemon named by DockerHost.
	Run diskmaint.Runner
	// Ledger records every apply; Apply refuses to run without one.
	Ledger *diskmaint.Ledger
}

// Target prunes dangling images from the colima daemon.
type Target struct {
	cfg Config
}

// NewTarget returns a Target that reads and prunes the daemon named by cfg.
func NewTarget(cfg Config) *Target {
	return &Target{cfg: cfg}
}

// Name is the target's name in plans and ledger entries.
func (t *Target) Name() string { return planTarget }

// Measure reports the bytes every image on the daemon holds, tagged or not. An
// unreadable image list is an error, never a zero.
func (t *Target) Measure(ctx context.Context) (diskmaint.Measurement, error) {
	images, err := t.client().Images(ctx)
	if err != nil {
		return diskmaint.Measurement{}, fmt.Errorf("measure: %w", err)
	}
	// docker prints one row per repo:tag, each with the same id and size, so an
	// image with several tags is counted once.
	seen := make(map[string]bool, len(images))
	var used int64
	for _, im := range images {
		if seen[im.ID] {
			continue
		}
		seen[im.ID] = true
		used += im.Bytes()
	}
	return diskmaint.Measurement{
		Used:   used,
		Detail: fmt.Sprintf("%d images on the colima daemon", len(seen)),
	}, nil
}

// Plan lists the dangling images no container uses as remove-image actions.
// Every other image is Kept with the rule that kept it. A listing that cannot be
// read, or a line that does not parse, refuses the plan. Plan runs no rmi: the
// caller applies only when its policy asks for it.
func (t *Target) Plan(ctx context.Context, _ diskmaint.Policy) (diskmaint.Plan, error) {
	plan := diskmaint.Plan{Target: planTarget}
	images, err := t.client().Images(ctx)
	if err != nil {
		plan.Refused = err.Error()
		return plan, nil
	}
	inUse, err := t.client().InUse(ctx)
	if err != nil {
		plan.Refused = err.Error()
		return plan, nil
	}
	seen := make(map[string]bool, len(images))
	for _, im := range images {
		repoNone, tagNone := im.Repository == noneField, im.Tag == noneField
		switch {
		case !repoNone && !tagNone:
			plan.Kept = append(plan.Kept, diskmaint.Kept{Ref: im.Repository + ":" + im.Tag, Rule: ruleTagged})
		case repoNone && tagNone:
			if seen[im.ID] {
				continue
			}
			seen[im.ID] = true
			if inUse[im.ID] {
				plan.Kept = append(plan.Kept, diskmaint.Kept{Ref: noneField + "@" + im.ID, Rule: ruleInUse})
				continue
			}
			plan.Actions = append(plan.Actions, diskmaint.Action{
				Kind:  actionRemove,
				Ref:   im.ID,
				Bytes: im.Bytes(),
				Why:   "dangling and no container uses it",
			})
		default:
			plan.Kept = append(plan.Kept, diskmaint.Kept{Ref: im.Repository + ":" + im.Tag, Rule: rulePartial})
		}
	}
	return plan, nil
}

// Apply runs docker rmi once per planned image id, never prune and never -f. It
// does not re-plan: an image the live state would now select but the plan did
// not name is left alone. One failed removal does not stop the others. Every
// action is validated before its command runs, and the run is recorded as one
// ledger line. A refused plan removes nothing and returns errPlanRefused.
func (t *Target) Apply(ctx context.Context, plan diskmaint.Plan) (diskmaint.Result, error) {
	result := diskmaint.Result{Target: planTarget}
	if t.cfg.Ledger == nil {
		return result, errNoLedger
	}
	var errs []string
	var err error
	if plan.Refused != "" {
		err = errPlanRefused
	} else if len(plan.Actions) > 0 {
		if err = t.ready(); err != nil {
			errs = append(errs, err.Error())
		} else {
			errs = t.removePlanned(ctx, plan.Actions, &result)
		}
	}
	if recErr := t.record(plan, result, errs); recErr != nil {
		err = errors.Join(err, recErr)
	}
	return result, err
}

// removePlanned removes each named image and returns the reasons to record. An
// action whose ref does not validate becomes a Failure before any command runs.
func (t *Target) removePlanned(ctx context.Context, actions []diskmaint.Action, result *diskmaint.Result) []string {
	var errs []string
	for _, a := range actions {
		id, err := actionID(a)
		if err != nil {
			result.Failed = append(result.Failed, diskmaint.Failure{Action: a, Err: err.Error()})
			errs = append(errs, err.Error())
			continue
		}
		if _, err := t.client().Docker(ctx, "rmi", id); err != nil {
			result.Failed = append(result.Failed, diskmaint.Failure{Action: a, Err: removeFailed})
			errs = append(errs, fmt.Sprintf("rmi %s failed; the image is still present", id))
			continue
		}
		result.Done = append(result.Done, a)
		result.BytesFreed += a.Bytes
	}
	return errs
}

// actionID validates one plan action and returns the image id it names. The
// refusal does not quote the ref: it is plan text no validator has passed.
func actionID(a diskmaint.Action) (string, error) {
	if a.Kind != actionRemove {
		return "", fmt.Errorf("action kind is not %s", actionRemove)
	}
	if !registry.ValidDigest(a.Ref) {
		return "", errBadRef
	}
	return a.Ref, nil
}

// client is the shared docker reader pointed at the target's daemon.
func (t *Target) client() dockerimg.Client {
	return dockerimg.Client{Host: t.cfg.DockerHost, Run: t.cfg.Run}
}

// ready reports whether the target can reach a daemon at all.
func (t *Target) ready() error { return t.client().Ready() }

// record appends the run's one ledger line.
func (t *Target) record(plan diskmaint.Plan, result diskmaint.Result, errs []string) error {
	err := t.cfg.Ledger.Append(diskmaint.LedgerEntry{
		TS:         time.Now().UTC(),
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
