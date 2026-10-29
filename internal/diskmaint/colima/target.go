// Package colima removes dangling images from the gt-sbx Colima docker daemon
// as a diskmaint target. It names each image by id rather than running docker's
// own prune, so a dry run lists exactly what goes and the ledger records it.
//
// The target is slice 3 of gt-z6qre; gt maint wires it in with the other targets.
package colima

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
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
	// errNoRunner reports a target with no docker runner.
	errNoRunner = errors.New("no docker runner configured")
	// errNoDockerHost reports a target that does not name the colima daemon.
	// Without it docker would talk to whatever daemon the environment picks.
	errNoDockerHost = errors.New("no docker host configured; the colima daemon is not named")
	// errListImages reports an unreadable image list. The runner's error carries
	// the command's stderr, so it is dropped in favor of this fixed reason.
	errListImages = errors.New("list docker images failed; the image list is unreadable")
	// errListContainers reports an unreadable container list.
	errListContainers = errors.New("list docker containers failed; the in-use set is unreadable")
	// errInspect reports an unreadable container image lookup.
	errInspect = errors.New("inspect container images failed; the in-use set is unreadable")
	// errParse reports a line of docker output that does not parse, so the
	// listing is refused rather than read partly.
	errParse = errors.New("docker output line does not parse; the listing is refused")
	// errBadRef reports a plan action whose ref is not sha256:<64 hex>.
	errBadRef = errors.New("ref is not sha256:<64 hex>")
)

var (
	// containerPattern matches the short or full container ids docker ps prints.
	containerPattern = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
	// sizeUnits are the units docker's human-readable sizes use.
	sizeUnits = map[string]float64{
		"B": 1, "kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40,
	}
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

// image is one line of docker image ls --format {{json .}}.
type image struct {
	Repository string `json:"Repository"`
	Tag        string `json:"Tag"`
	ID         string `json:"ID"`
	Size       string `json:"Size"`
}

// Measure reports the bytes every image on the daemon holds, tagged or not. An
// unreadable image list is an error, never a zero.
func (t *Target) Measure(ctx context.Context) (diskmaint.Measurement, error) {
	images, err := t.images(ctx)
	if err != nil {
		return diskmaint.Measurement{}, fmt.Errorf("measure: %w", err)
	}
	var used int64
	for _, im := range images {
		used += im.bytes()
	}
	return diskmaint.Measurement{
		Used:   used,
		Detail: fmt.Sprintf("%d images on the colima daemon", len(images)),
	}, nil
}

// Plan lists the dangling images no container uses as remove-image actions.
// Every other image is Kept with the rule that kept it. A listing that cannot be
// read, or a line that does not parse, refuses the plan. Plan runs no rmi: the
// caller applies only when its policy asks for it.
func (t *Target) Plan(ctx context.Context, _ diskmaint.Policy) (diskmaint.Plan, error) {
	plan := diskmaint.Plan{Target: planTarget}
	images, err := t.images(ctx)
	if err != nil {
		plan.Refused = err.Error()
		return plan, nil
	}
	inUse, err := t.inUse(ctx)
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
				Bytes: im.bytes(),
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
// ledger line.
func (t *Target) Apply(ctx context.Context, plan diskmaint.Plan) (diskmaint.Result, error) {
	result := diskmaint.Result{Target: planTarget}
	if t.cfg.Ledger == nil {
		return result, errNoLedger
	}
	var errs []string
	var err error
	if plan.Refused == "" && len(plan.Actions) > 0 {
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

// images reads the daemon's image list. Every line must parse and carry a
// valid image id; one that does not refuses the whole listing.
func (t *Target) images(ctx context.Context) ([]image, error) {
	out, err := t.docker(ctx, "image", "ls", "--no-trunc", "--format", "{{json .}}")
	if err != nil {
		return nil, t.listErr(err, errListImages)
	}
	var images []image
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var im image
		if err := json.Unmarshal([]byte(line), &im); err != nil || !registry.ValidDigest(im.ID) {
			return nil, errParse
		}
		images = append(images, im)
	}
	return images, nil
}

// inUse is the set of image ids a container, running or stopped, uses. Any
// container or image id that does not parse refuses the set, so a container
// the set misses can never make a dangling image look unused.
func (t *Target) inUse(ctx context.Context) (map[string]bool, error) {
	out, err := t.docker(ctx, "ps", "-a", "-q")
	if err != nil {
		return nil, t.listErr(err, errListContainers)
	}
	ids := strings.Fields(string(out))
	used := map[string]bool{}
	if len(ids) == 0 {
		return used, nil
	}
	for _, id := range ids {
		if !containerPattern.MatchString(id) {
			return nil, errParse
		}
	}
	args := append([]string{"inspect", "--format", "{{.Image}}"}, ids...)
	out, err = t.docker(ctx, args...)
	if err != nil {
		return nil, t.listErr(err, errInspect)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id := strings.TrimSpace(line)
		if !registry.ValidDigest(id) {
			return nil, errParse
		}
		used[id] = true
	}
	return used, nil
}

// listErr is the fixed reason for a failed docker call. A missing runner or host
// is named as such; any other failure is the given sentinel, since the runner's
// error carries the command's stderr.
func (t *Target) listErr(err, fallback error) error {
	if errors.Is(err, errNoRunner) || errors.Is(err, errNoDockerHost) {
		return err
	}
	return fallback
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
		if _, err := t.docker(ctx, "rmi", id); err != nil {
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

// ready reports whether the target can reach a daemon at all.
func (t *Target) ready() error {
	if t.cfg.Run == nil {
		return errNoRunner
	}
	if t.cfg.DockerHost == "" {
		return errNoDockerHost
	}
	return nil
}

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

// docker runs docker against the colima daemon with args.
func (t *Target) docker(ctx context.Context, args ...string) ([]byte, error) {
	if err := t.ready(); err != nil {
		return nil, err
	}
	return t.cfg.Run(ctx, []string{"DOCKER_HOST=" + t.cfg.DockerHost}, "docker", args...)
}

// bytes is the image's listed size, or 0 when docker printed one that does not
// parse: a size is best-effort and never a reason to refuse.
func (im image) bytes() int64 {
	i := strings.IndexFunc(im.Size, func(r rune) bool { return !(r >= '0' && r <= '9' || r == '.') })
	if i <= 0 {
		return 0
	}
	n, err := strconv.ParseFloat(im.Size[:i], 64)
	mult, ok := sizeUnits[im.Size[i:]]
	if err != nil || !ok || n < 0 {
		return 0
	}
	return int64(math.Round(n * mult))
}
