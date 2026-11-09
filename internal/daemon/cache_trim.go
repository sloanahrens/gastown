package daemon

// cacheTrim applies the shared cache claim's trim from the disk_maintenance
// patrol (gt-rm4am): when its interval is due, in any window (gt-gg9hv.24.2),
// the pass reads the claim's listing, builds the trim Job with
// internal/polecat/cachevol, and in apply mode applies the manifest's documents
// in order, the shard ConfigMaps before the Job that mounts them.
//
// The patrol is the one applier, and dry run is its default as for every
// maintenance target: the manifest is built and the objects it would apply are
// logged, and nothing is written. A listing that cannot be read refuses the
// trim and says so, because a listing read as empty would plan a trim of a
// claim nothing looked at.
//
// The claim's namespace comes from the claim itself, not from a default or from
// a pod: pods of the same claim name in two namespaces are two claims, and each
// is judged by its own namespace's pods alone.
//
// The listing comes from the cluster, not from this host: the claim is mounted
// only by pods, so the pass reads the pods that mount it with kubectl -- the way
// the registry target's in-use provider reads pods -- counts the ones holding
// it as PlanTrim's sandbox count, and asks a running one of them to print the
// listing with the image's trim-list helper.
//
// A removal list past the shard ceiling is carried in the claim itself, and the
// applied line names the route that carried it. That route writes the list into
// the claim before its Job runs, so a list the claim cannot hold is refused
// before the apply: the pass reads the room from df in the pod the listing ran
// in, not from the image's helper, whose dispatcher runs a plugin run for any
// first argument it does not know. A room df cannot read leaves the fit check
// unknown and the trim still runs, deliberately: the writer refuses a list that
// will not fit (gt-gg9hv.41).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/polecat/cachevol"
	"github.com/steveyegge/gastown/internal/polecatimage"
)

const (
	// cacheTrimCapEnv is the size the claim is trimmed under: the knob the
	// polecat-image plugin already bounds each per-rig cache volume with, so a
	// town sets one cache cap rather than one per cache.
	cacheTrimCapEnv = "POLECAT_CACHE_CAP"

	// cacheTrimDefaultCap is the cap when the setting names none, the plugin's
	// own default.
	cacheTrimDefaultCap = "10GB"

	// cacheTrimWaitTimeout bounds the wait on a trim Job before its shard
	// ConfigMaps are deleted. The pass itself is bounded by
	// diskMaintRunTimeout, which the command inherits, so a Job still running
	// then is reported and its shards are left for the next run.
	cacheTrimWaitTimeout = 2 * time.Minute

	// cacheTrimDeleteTimeout bounds the foreground delete of an earlier run's
	// Job. kubectl returns once the Job and its pods are gone, so the name the
	// rerun applies next is not racing that delete.
	cacheTrimDeleteTimeout = 60 * time.Second

	// cacheTrimJobNameMax is the most a Kubernetes object name holds. A Job
	// name of a longer claim would be refused at apply, so it is shortened in
	// one place (cacheTrimJobName) rather than at every caller.
	cacheTrimJobNameMax = 63

	// cacheTrimJobNameDigest is how many hex characters of the claim's SHA-256
	// a shortened Job name ends with: enough that two long claims sharing a
	// prefix still get two names.
	cacheTrimJobNameDigest = 8

	// cacheTrimRefusalLogInterval is how long the same cache trim line is held
	// back. A trim that cannot start is retried every fifteen minutes, so one
	// line an hour says the refusal without repeating it four times.
	cacheTrimRefusalLogInterval = time.Hour

	// cacheTrimPreflightVerb is the preflight's kubectl arguments after the
	// kubeconfig and context prefix: it asks the file whether it names the
	// sandbox context at all.
	cacheTrimPreflightVerb = "config get-contexts " + diskMaintSandboxContext + " -o name"

	// cacheTrimNoOpFactor is how many times the cap a claim may still hold
	// after a completed trim before the trim is an attention item: a trim
	// that leaves it over this trimmed nothing, the trace a Job that ran the
	// wrong helper leaves (gt-gg9hv.46).
	cacheTrimNoOpFactor = 2

	// cacheTrimNoOpKeyPrefix is the item key's prefix; the claim's namespace
	// and name follow it, so one claim has one item however often it is
	// measured.
	cacheTrimNoOpKeyPrefix = "cache-trim-noop:"

	// cacheTrimNoOpOpenSandboxes is the item's closing sentence: the reading is
	// the claim's whole size from du, not only what the trim left, so a sandbox
	// that is open while the trim runs can explain a claim still over the
	// threshold without the Job having failed to remove anything.
	cacheTrimNoOpOpenSandboxes = "open sandbox paths can explain this; the claim is read whole, not just what the trim left"
)

// errCacheTrimNoContext is the preflight's refusal for a kubeconfig that parses
// but names no sandbox context.
var errCacheTrimNoContext = errors.New("the kubeconfig names no sandbox context")

// cacheTrimClaimRE is the DNS-1123 label form a PersistentVolumeClaim's name
// takes.
var cacheTrimClaimRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// cacheTrimClaim is one shared cache claim a pass trims, the image its sandbox
// profiles mount, and where that Job's pod runs: the placement those profiles
// run and sit on, since the claim's storage binds on that pool (gt-5paez). The
// pass runs the image a CI-built values file names when there is one, and this
// image otherwise.
type cacheTrimClaim struct {
	ClaimName    string
	Image        string
	NodeSelector map[string]string
	Tolerations  []cachevol.Toleration
}

// cacheTrimOwner is one sandbox profile that mounts a claim, by the name the
// settings give it, so a refusal can say which pair disagree.
type cacheTrimOwner struct {
	Name    string
	Profile agentconfig.SandboxProfile
}

// cacheTrimClaims returns the claims the sandbox profiles mount, one per claim,
// sorted by claim name, and the profiles that cannot be trimmed as they stand.
//
// Only a kubernetes profile mounts a claim at all. A claim two profiles name
// with different images or different pod placement is refused: the trim Job runs
// one image's helpers on one pool, and which one is the town's to say, not this
// pass's to pick.
func cacheTrimClaims(profiles map[string]agentconfig.SandboxProfile) ([]cacheTrimClaim, []error) {
	mounting := map[string][]cacheTrimOwner{}
	for _, name := range slices.Sorted(maps.Keys(profiles)) {
		profile := profiles[name]
		if profile.Driver != agentconfig.SandboxDriverKubernetes || profile.KubernetesCacheClaim == "" {
			continue
		}
		claim := profile.KubernetesCacheClaim
		mounting[claim] = append(mounting[claim], cacheTrimOwner{Name: name, Profile: profile})
	}

	var claims []cacheTrimClaim
	var errs []error
	for _, claim := range slices.Sorted(maps.Keys(mounting)) {
		if !validTrimClaim(claim) {
			errs = append(errs, fmt.Errorf("the cache claim %s: want a DNS-1123 label of at most 63 characters", claim))
			continue
		}
		owners := mounting[claim]
		var images []string
		for _, owner := range owners {
			if !slices.Contains(images, owner.Profile.Image) {
				images = append(images, owner.Profile.Image)
			}
		}
		slices.Sort(images)
		if len(images) != 1 {
			errs = append(errs, fmt.Errorf("the cache claim %s is mounted by sandbox profiles with %d images (%s); the trim Job runs one image, so none is chosen", claim, len(images), strings.Join(images, ", ")))
			continue
		}
		if a, b, differ := placementMismatch(owners); differ {
			errs = append(errs, fmt.Errorf("the cache claim %s is mounted by the sandbox profiles %s and %s, which place their pods differently; the trim Job runs on one pool, so none is chosen", claim, a, b))
			continue
		}
		first := owners[0].Profile
		claims = append(claims, cacheTrimClaim{
			ClaimName:    claim,
			Image:        images[0],
			NodeSelector: first.NodeSelector,
			Tolerations:  cacheTrimTolerations(first.Tolerations),
		})
	}
	return claims, errs
}

// placementMismatch is the first pair of owners that place a pod differently:
// on different node labels, or tolerating different taints. Two profiles
// mounting one claim run two sandboxes on one pool, so a Job placed for one is
// placed for both or for neither.
func placementMismatch(owners []cacheTrimOwner) (string, string, bool) {
	first := owners[0]
	for _, other := range owners[1:] {
		same := maps.Equal(first.Profile.NodeSelector, other.Profile.NodeSelector) &&
			slices.Equal(cacheTrimTolerations(first.Profile.Tolerations), cacheTrimTolerations(other.Profile.Tolerations))
		if !same {
			return first.Name, other.Name, true
		}
	}
	return "", "", false
}

// cacheTrimTolerations is a profile's tolerations as the Job's pod spec takes
// them. The settings validate their fields before they reach here, so nothing
// is checked again.
func cacheTrimTolerations(tolerations []agentconfig.SandboxToleration) []cachevol.Toleration {
	if len(tolerations) == 0 {
		return nil
	}
	out := make([]cachevol.Toleration, 0, len(tolerations))
	for _, t := range tolerations {
		out = append(out, cachevol.Toleration{Key: t.Key, Operator: t.Operator, Value: t.Value, Effect: t.Effect})
	}
	return out
}

// validTrimClaim reports whether claim is a name a PersistentVolumeClaim takes:
// a DNS-1123 label. The settings check the same form, and this pass checks it
// again rather than trust a config built in code, since the name reaches a
// kubectl argument.
//
// A claim failing this check is refused, never normalized: an uppercase letter
// or an underscore stops the trim rather than becoming a hyphen, so the pass
// never trims a claim other than the one the town named.
func validTrimClaim(claim string) bool {
	return len(claim) <= 63 && cacheTrimClaimRE.MatchString(claim)
}

// cacheTrimCap returns the size the claim is trimmed under. A setting that is
// not a size refuses the trim rather than reading as a cap near zero, which
// would trim everything.
func cacheTrimCap(getenv func(string) string) (int64, error) {
	value := getenv(cacheTrimCapEnv)
	if value == "" {
		value = cacheTrimDefaultCap
	}
	capBytes, ok := polecatimage.CapBytes(value)
	if !ok {
		return 0, fmt.Errorf("%s=%q is not a size such as 10GB", cacheTrimCapEnv, value)
	}
	return capBytes, nil
}

// cacheTrimJobName is a claim's trim Job name: the trim prefix and the claim
// when the two fit a Kubernetes object name, and otherwise the prefix, as much
// of the claim as fits, and eight hex characters of the claim's SHA-256, so two
// long claims sharing a prefix still get two names.
//
// The claim reaches here after validTrimClaim, so it is already a DNS-1123
// label; the shortening keeps that form because it cuts at a character
// boundary and drops any hyphen the cut left at the end.
func cacheTrimJobName(claim string) string {
	name := cachevol.DefaultTrimJobName + "-" + claim
	if len(name) <= cacheTrimJobNameMax {
		return name
	}
	sum := sha256.Sum256([]byte(claim))
	digest := hex.EncodeToString(sum[:])[:cacheTrimJobNameDigest]
	prefix := cachevol.DefaultTrimJobName + "-"
	keep := cacheTrimJobNameMax - len(prefix) - 1 - len(digest)
	base := strings.TrimRight(claim[:keep], "-")
	return prefix + base + "-" + digest
}

// cacheTrim is the pass's cache trim: each claim a kubernetes sandbox profile
// mounts, in each namespace that holds it, trimmed in the mode the patrol runs
// in.
//
// It reports whether the pass reached a verdict: trimmed, or nothing to trim.
// A pass that could not start -- unreadable profiles, values file or cap, a
// kubeconfig that names no sandbox context, an unreadable claim list -- or a
// claim whose trim failed for a reason the next run may clear reports false, and
// the patrol retries it soon rather than after the full interval. A refusal the
// config or the listing decides is a verdict: the next run judges it again at
// the full interval.
func (d *Daemon) cacheTrim(ctx context.Context, mode string) bool {
	s := d.diskMaintenanceSeams()
	profiles, err := s.sandboxes()
	if err != nil {
		d.cacheTrimLogf("disk_maintenance: cache trim: the sandbox profiles could not be read: %v; nothing trimmed", err)
		return false
	}
	// A claim no profile mounts is never reached by the loop below, so its item
	// is cleared here, from the profiles this pass read. A failed read returned
	// above and clears nothing.
	mounted := map[string]bool{}
	for _, profile := range profiles {
		if profile.Driver == agentconfig.SandboxDriverKubernetes && profile.KubernetesCacheClaim != "" {
			mounted[profile.KubernetesCacheClaim] = true
		}
	}
	d.cacheTrimNoOp.clearUnmounted(mounted)
	claims, errs := cacheTrimClaims(profiles)
	for _, err := range errs {
		d.cacheTrimLogf("disk_maintenance: cache trim: not applied: %v", err)
	}
	// The pass runs the polecat image the CI-built values file names, when that
	// file is there, so the trim's helper is the one built with the daemon's
	// commit rather than a moving tag a profile carries. The file is read once
	// for the pass, so every Job it applies runs the one image it records; with
	// no file each claim keeps its profile's image (gt-pm2y2m). It is read
	// before the empty-claims return below, so a malformed file is named in a
	// pass with nothing to trim rather than only in one that trims (gt-714vqw).
	valuesImage, fromValues, err := d.cacheTrimImage()
	if err != nil {
		d.cacheTrimLogf("disk_maintenance: cache trim: %v; nothing trimmed", err)
		return false
	}
	if len(claims) == 0 {
		return true
	}
	capBytes, err := cacheTrimCap(s.getenv)
	if err != nil {
		d.cacheTrimLogf("disk_maintenance: cache trim: not applied: %v", err)
		return false
	}
	kubeconfig := diskMaintKubeconfig(d.config.TownRoot, s.getenv)
	if err := d.cacheTrimPreflight(ctx, kubeconfig); err != nil {
		if errors.Is(err, errCacheTrimNoContext) {
			d.cacheTrimLogf("disk_maintenance: cache trim: kubeconfig %s names no context %q; nothing trimmed", kubeconfig, diskMaintSandboxContext)
		} else {
			d.cacheTrimLogf("disk_maintenance: cache trim: the kubeconfig could not be read: %v; nothing trimmed", err)
		}
		return false
	}
	namespaces, err := d.cacheTrimClaimNamespaces(ctx, kubeconfig)
	if err != nil {
		d.cacheTrimLogf("disk_maintenance: cache trim: the cluster's claims could not be read: %v; nothing trimmed", err)
		return false
	}
	// trimmed is the claim each namespace this pass trims for. A trim's labels
	// are the same for every claim, so two claims trimming in one namespace
	// would delete each other's objects; the second is refused.
	trimmed := map[string]string{}
	verdict := true
	for _, claim := range claims {
		found := namespaces[claim.ClaimName]
		if len(found) == 0 {
			// The listing names no claim of this name anywhere, so the claim a
			// completed trim measured was deleted, and its item describes
			// nothing this pass can judge. The collector reads the store
			// itself, so the next collection stops raising it.
			d.cacheTrimNoOp.clearClaim(claim.ClaimName)
			d.cacheTrimLogf("disk_maintenance: cache trim %s: no claim of that name is in the cluster; nothing trimmed", claim.ClaimName)
			continue
		}
		trimImage := claim.Image
		if fromValues {
			trimImage = valuesImage
		}
		for _, namespace := range found {
			if !d.cacheTrimClaim(ctx, kubeconfig, claim, trimImage, namespace, capBytes, mode == agentconfig.DiskMaintenanceModeApply, trimmed) {
				verdict = false
			}
		}
	}
	return verdict
}

// cacheTrimImage is the polecat image the CI-built values file records, and
// whether there is one at all: the Job that trims a claim runs the helper built
// with the daemon's commit, not a tag a sandbox profile names (gt-pm2y2m). The
// file is the one gt update images writes under the town's gastown directory. A
// pair not built yet answers false with no error; a file that is there and
// cannot be read or parsed is an error, never an absent pair.
func (d *Daemon) cacheTrimImage() (string, bool, error) {
	values, ok, err := imagevalues.LoadIn(filepath.Join(d.config.TownRoot, "gastown"))
	if err != nil {
		return "", false, err
	}
	return values.Polecat, ok, nil
}

// cacheTrimPreflight refuses a kubeconfig that does not resolve the sandbox
// context, before the pass makes any other cluster call. Every call names the
// context with --context, because a file that does not hold it falls back to
// its current-context, and to localhost:8080 when it has none (gt-gg9hv.24.6).
//
// The preflight is one call and the pass stops when it refuses: the caller logs
// the refusal and returns without a second cluster call.
func (d *Daemon) cacheTrimPreflight(ctx context.Context, kubeconfig string) error {
	out, err := d.cacheTrimKubectl(ctx, kubeconfig, "config", "get-contexts", diskMaintSandboxContext, "-o", "name")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) == "" {
		return errCacheTrimNoContext
	}
	return nil
}

// cacheTrimLogf logs one refusal or failure line, at most once an hour for the
// same line: a trim that cannot start is retried in fifteen minutes, and saying
// the same refusal on every retry would be four lines an hour for one fault. A
// success is not a repeat to hold back, so it is logged through d.logger.
func (d *Daemon) cacheTrimLogf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if d.diskMaintSchedule.allowLog(line, d.diskMaintenanceSeams().now(), cacheTrimRefusalLogInterval) {
		d.logger.Printf("%s", line)
	}
}

// cacheTrimLogKeyed logs one cache trim line at most once an hour for its own
// key. The fit refusal and the free-space-unknown line each have a key of their
// own (claim and namespace included), so one being held back never hides the
// other, and a changed figure in the text does not slip past the hold.
func (d *Daemon) cacheTrimLogKeyed(key, format string, args ...any) {
	if d.diskMaintSchedule.allowLog(key, d.diskMaintenanceSeams().now(), cacheTrimRefusalLogInterval) {
		d.logger.Printf(format, args...)
	}
}

// cacheTrimNoOpKey is the attention item key of one claim a completed trim left
// over the cap: the claim's namespace and name, since pods of the same claim
// name in two namespaces are two claims.
func cacheTrimNoOpKey(namespace, claim string) string {
	return cacheTrimNoOpKeyPrefix + namespace + "/" + claim
}

// cacheTrimNoOp is the claims a completed trim left over cacheTrimNoOpFactor
// times the cap, with the item each raises. The pass writes it on the goroutine
// a pass is dispatched to and the attention tick reads it on the heartbeat
// goroutine, so a mutex guards it; it is memory only, and the next pass's
// reading re-derives it after a restart.
type cacheTrimNoOp struct {
	mu    sync.Mutex
	items map[string]attention.Item
}

// raise stores it under its key, replacing an earlier measurement of the same
// claim: one claim is one item however often it is measured.
func (s *cacheTrimNoOp) raise(it attention.Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = map[string]attention.Item{}
	}
	s.items[it.Key] = it
}

// clear drops key's item: the claim was measured at or under the threshold.
func (s *cacheTrimNoOp) clear(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, key)
}

// clearClaim drops every item of a claim, in every namespace: the claim listing
// names no claim of that name, so each stored reading belongs to a claim that is
// gone -- deleted, or re-created with its pods gone -- and says nothing about
// whatever claim the name belongs to now.
//
// The key ends with the claim's name, so the suffix is the claim itself; the
// prefix keeps the CI cache trim's key out of the match.
func (s *cacheTrimNoOp) clearClaim(claim string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	suffix := "/" + claim
	for key := range s.items {
		if strings.HasPrefix(key, cacheTrimNoOpKeyPrefix) && strings.HasSuffix(key, suffix) {
			delete(s.items, key)
		}
	}
}

// clearUnmounted drops every item whose claim no sandbox profile mounts. The
// pass reaches a claim only through a profile, so the item of a claim the
// profiles dropped is never measured again and describes nothing this pass can
// judge.
//
// The key ends with the claim's name, so the claim is the text after its last
// slash; the prefix keeps the CI cache trim's key out of the sweep.
func (s *cacheTrimNoOp) clearUnmounted(mounted map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.items {
		if !strings.HasPrefix(key, cacheTrimNoOpKeyPrefix) {
			continue
		}
		if !mounted[key[strings.LastIndex(key, "/")+1:]] {
			delete(s.items, key)
		}
	}
}

// observed returns the items, copied for a reader and ordered by key so the
// collector's output does not depend on map order.
func (s *cacheTrimNoOp) observed() []attention.Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]attention.Item, 0, len(s.items))
	for _, it := range s.items {
		out = append(out, it)
	}
	slices.SortFunc(out, func(a, b attention.Item) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// cacheTrimOverTheCap reports whether used is more than cacheTrimNoOpFactor
// times capBytes. It divides rather than multiplying: a cap close to MaxInt64
// makes the product wrap negative, and `used > negative` would put every claim
// over the cap. The half-and-remainder pair is exact -- used == 2*cap is at the
// cap, not over it -- and no product is formed, so no cap can be too large.
func cacheTrimOverTheCap(used, capBytes int64) bool {
	half := used / cacheTrimNoOpFactor
	if half != capBytes {
		return half > capBytes
	}
	return used%cacheTrimNoOpFactor != 0
}

// cacheTrimClaim trims one claim in one namespace: it reads the listing through
// a pod that mounts the claim there, builds the trim, and in apply mode applies
// it. image is the image the Job runs, which the pass resolved for the whole
// pass; trimmed is the namespace each claim this pass already trims in, which
// the claim joins.
//
// It reports whether this claim reached a verdict. A read that failed, no
// running pod to read the listing through, a list that does not fit the claim,
// or an apply that failed reports false, so the pass records a retry instead of
// the full interval. A claim the config refuses, and a listing at or under the
// cap, are verdicts.
func (d *Daemon) cacheTrimClaim(ctx context.Context, kubeconfig string, claim cacheTrimClaim, image, namespace string, capBytes int64, apply bool, trimmed map[string]string) bool {
	sandboxes, sources, err := d.cacheTrimPods(ctx, kubeconfig, namespace, claim.ClaimName)
	if err != nil {
		d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: the pods that mount it could not be read: %v; nothing trimmed", claim.ClaimName, namespace, err)
		return false
	}
	if len(sources) == 0 {
		// No pod holds the claim, so nothing mounts the claim the completed
		// trim measured: it was re-created with its pods gone, or no sandbox
		// uses it now, and the item it raised describes nothing this pass can
		// judge. Pods that hold the claim without one running are a different
		// case: the claim is there, and a later trim can measure it, so its
		// item stays.
		if sandboxes == 0 {
			d.cacheTrimNoOp.clear(cacheTrimNoOpKey(namespace, claim.ClaimName))
		}
		d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: no running pod mounts it, so its listing cannot be read (%d pod(s) hold it); nothing trimmed", claim.ClaimName, namespace, sandboxes)
		return false
	}
	listing, err := d.cacheTrimListing(ctx, kubeconfig, sources[0])
	if err != nil {
		d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: the listing could not be read: %v; nothing trimmed", claim.ClaimName, namespace, err)
		return false
	}

	// The Job is named after the claim, in the claim's own namespace, so two
	// claims get two Jobs and a rerun of a trim overwrites its own rather than a
	// neighbour's.
	jobName := cacheTrimJobName(claim.ClaimName)
	// This refusal is the town's config, the same on every pass, so it is a
	// verdict: the claim is judged again at the full interval, not in fifteen
	// minutes.
	if other, taken := trimmed[namespace]; taken {
		d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: the claim %s is trimmed in the same namespace, and a trim's labels cannot tell the two sets of objects apart; nothing trimmed", claim.ClaimName, namespace, other)
		return true
	}
	trimmed[namespace] = claim.ClaimName
	// A listing at or under the cap has nothing to remove whatever the sandboxes
	// do, so that is this claim's verdict for this listing. A listing over the
	// cap with nothing removable, because a running sandbox pins the module
	// cache, is not: it clears when the sandbox stops, so it falls through to
	// the trim's refusal and is retried soon.
	if plan := cachevol.PlanTrim(listing, capBytes, sandboxes > 0); plan.Total <= capBytes {
		d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: the listing is %d bytes, at or under the cap of %d; nothing trimmed", claim.ClaimName, namespace, plan.Total, capBytes)
		return true
	}
	m, err := cachevol.BuildTrimManifest(cachevol.TrimJobInput{
		Name:             jobName,
		Namespace:        namespace,
		Image:            image,
		ClaimName:        claim.ClaimName,
		Listing:          listing,
		CapBytes:         capBytes,
		SandboxesRunning: sandboxes,
		NodeSelector:     claim.NodeSelector,
		Tolerations:      claim.Tolerations,
	})
	if err != nil {
		d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: %v; nothing trimmed", claim.ClaimName, namespace, err)
		return false
	}
	// A claim-carried list is written into the claim before the Job runs, so a
	// list the claim has no room for is refused here rather than part way
	// through the write (gt-gg9hv.41). The shard route carries no list in the
	// claim and is not judged by its room.
	//
	// The room is read with df, not with a helper subcommand: the image's
	// helper dispatches only its known subcommands, and an older image answers
	// any other first argument with a full plugin run. A df that cannot answer
	// leaves the room unknown and the trim still runs -- a DELIBERATE fail-open:
	// the Job's writer measures the free space again and refuses a list that
	// will not fit, so skipping this early check loses the clearer refusal, not
	// the guard. The unknown line is held to one an hour like a refusal.
	if m.Route() == cachevol.TrimRouteClaim {
		reading, err := d.cacheTrimDf(ctx, kubeconfig, sources[0])
		if err != nil {
			d.cacheTrimLogKeyed("cache_trim_free_unknown:"+namespace+"/"+claim.ClaimName,
				"disk_maintenance: cache trim %s in %s: free space unknown: %v; trimming the claim route without the fit check", claim.ClaimName, namespace, err)
		} else if err := cacheTrimClaimFits(listing, capBytes, sandboxes, reading.Available); err != nil {
			d.cacheTrimLogKeyed("cache_trim_no_fit:"+namespace+"/"+claim.ClaimName,
				"disk_maintenance: cache trim %s in %s: %v; nothing trimmed", claim.ClaimName, namespace, err)
			return false
		}
	}
	if !apply {
		d.logger.Printf("disk_maintenance: cache trim %s in %s dry_run: %s", claim.ClaimName, namespace, cacheTrimManifestLine(jobName, m))
		return true
	}
	finished, err := d.cacheTrimApply(ctx, kubeconfig, namespace, jobName, m)
	if err != nil {
		d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: the apply failed: %v", claim.ClaimName, namespace, err)
		return false
	}
	d.logger.Printf("disk_maintenance: cache trim %s in %s applied: %s %s", claim.ClaimName, namespace, cacheTrimRouteLine(m), cacheTrimManifestLine(jobName, m))
	// A trim is judged by what it left behind, not by its apply having been
	// accepted, and only once its Job has finished: a reading taken while the
	// Job runs is the usage the trim is about to remove (gt-gg9hv.46).
	if finished {
		d.cacheTrimMeasure(ctx, kubeconfig, sources[0], claim.ClaimName, capBytes)
	}
	return true
}

// cacheTrimMeasure reads the claim's own size after a completed trim with du
// and raises or clears its attention item: while the claim holds more than
// cacheTrimNoOpFactor times the cap the trim left it there, and the Job may have
// run the wrong helper. du is the claim's size, where df's Used on a local-path
// claim is the whole node filesystem, which a trim can never lower
// (gt-eyjgal). The item names open sandbox paths as another explanation. A
// reading at or under the threshold drops the item: the trim did its work.
//
// A du that cannot be read logs once an hour under a key of its own and raises
// nothing. This check never fails the trim: the trim reached a verdict, and a
// claim nobody could measure is not a claim that stayed full.
func (d *Daemon) cacheTrimMeasure(ctx context.Context, kubeconfig string, source cacheTrimSource, claim string, capBytes int64) {
	used, err := d.cacheTrimDu(ctx, kubeconfig, source)
	if err != nil {
		// An item an earlier trim raised stays in place: an unreadable claim is
		// not thereby under the cap, and clearing the item would read it as
		// clean. Failing safe here is what om rule 1 requires.
		d.cacheTrimLogKeyed("cache_trim_noop_unknown:"+source.Namespace+"/"+claim,
			"disk_maintenance: cache trim %s in %s: the claim could not be measured after the trim: %v; no attention item is raised", claim, source.Namespace, err)
		return
	}
	key := cacheTrimNoOpKey(source.Namespace, claim)
	if !cacheTrimOverTheCap(used, capBytes) {
		d.cacheTrimNoOp.clear(key)
		return
	}
	d.cacheTrimNoOp.raise(attention.Item{
		Key:      key,
		Kind:     attention.KindCacheTrimNoOp,
		Severity: attention.SeverityLow,
		Summary: fmt.Sprintf("cache trim of %s completed but the claim still holds %s against a cap of %s: the Job may have run the wrong helper. %s",
			claim, polecatimage.Human(used), polecatimage.Human(capBytes), cacheTrimNoOpOpenSandboxes),
	})
}

// collectCacheTrimNoOp returns the claims a completed trim left over twice the
// cap. The trim pass writes the records and the heartbeat only reads them, so an
// item appears at a trim and clears at a later pass that reads the claim under
// the threshold or finds it gone, and the queue holds the last reading in
// between.
func (s *attentionSources) collectCacheTrimNoOp(context.Context) ([]attention.Item, error) {
	if s.cacheTrimNoOp == nil {
		return nil, nil
	}
	return s.cacheTrimNoOp(), nil
}

// cacheTrimClaimFits refuses a claim-carried trim whose removal list the claim
// has no room for: the Job's init container writes the list into the claim, so a
// claim with less room than the WRITER needs would fail it. The writer needs
// polecatimage.TrimListRoom of the encoded list (the list and its digest, each on
// disk twice while the second is renamed), and that same function is compared
// here, not the list's length alone.
//
// The list's size is PlanTrim's removals encoded as the writer encodes them --
// json.Marshal of the path slice, internal/polecatimage/trimlist.go WriteTrimList
// -- measured from the listing this pass already has; the writer plans the same
// list again when the Job runs, so a claim that changed in between can still end
// under its cap. The writer's rule has no further margin: the filesystem's block
// rounding is the writer's own measurement to make, and it refuses again if the
// room shrank.
func cacheTrimClaimFits(listing []cachevol.Entry, capBytes int64, sandboxes int, free int64) error {
	plan := cachevol.PlanTrim(listing, capBytes, sandboxes > 0)
	paths := make([]string, 0, len(plan.Removals))
	for _, removal := range plan.Removals {
		paths = append(paths, removal.Path)
	}
	// A string slice's JSON encoding cannot fail, so the error is dropped.
	data, _ := json.Marshal(paths)
	if need := polecatimage.TrimListRoom(len(data)); need > free {
		return fmt.Errorf("the removal list (%d bytes) needs %d bytes of room to be written with its digest and does not fit the claim's free space (%d)", len(data), need, free)
	}
	return nil
}

// cacheTrimRouteLine names how the trim's removal list reached its Job: the
// shard ConfigMaps it mounts, or the claim its init container writes.
func cacheTrimRouteLine(m *cachevol.TrimManifest) string {
	if m.Route() == cachevol.TrimRouteClaim {
		return "route=claim"
	}
	return fmt.Sprintf("route=shards n=%d", len(m.Shards()))
}

// cacheTrimManifestLine names what a trim applies: the shard ConfigMaps the
// Job mounts, or the one Job that plans its own list in the claim.
func cacheTrimManifestLine(jobName string, m *cachevol.TrimManifest) string {
	if m.Route() == cachevol.TrimRouteClaim {
		return fmt.Sprintf("%s: one Job, which plans its list in the claim", jobName)
	}
	names := make([]string, 0, len(m.Shards()))
	for _, shard := range m.Shards() {
		names = append(names, shard.Metadata.Name)
	}
	return fmt.Sprintf("%s: %d shard ConfigMap(s) (%s), then the Job that mounts them", jobName, len(names), strings.Join(names, ", "))
}

// cacheTrimApply applies the manifest: it deletes what an earlier run left
// behind, applies the documents in order -- the shards before the Job that
// mounts them -- waits for the Job, and deletes the shards it read.
//
// It reports whether the Job finished within the wait. The shards a finished
// Job read are deleted and the claim is measured only then: a Job still
// running is still reading its shards, and its claim still holds what the trim
// is removing.
//
// The deletions are the applier's alone: nothing else may create or delete a
// shard, so a trim's leftovers go only through the selector the trim's own
// objects carry (gt-ej1yz).
func (d *Daemon) cacheTrimApply(ctx context.Context, kubeconfig, namespace, jobName string, m *cachevol.TrimManifest) (bool, error) {
	existing, running, err := d.cacheTrimExisting(ctx, kubeconfig, namespace)
	if err != nil {
		return false, err
	}
	stale := cachevol.TrimStale(m, existing)
	// An earlier Job that still runs is left alone, and this trim waits for the
	// next pass: deleting it would stop the trim its own pods are in the middle
	// of, and applying over it is refused because its template is immutable.
	for _, o := range stale {
		if o.Kind == "Job" && running[o.Name] {
			return false, fmt.Errorf("the earlier trim Job %s is still running; its pods mount the claim, so it is left to finish", o.Name)
		}
	}
	for _, object := range stale {
		kind := strings.ToLower(object.Kind)
		args := []string{"delete", kind, object.Name, "-n", namespace, "--ignore-not-found"}
		if kind == "job" {
			// Foreground and bounded: kubectl returns once the Job and its pods
			// are gone, so the same-named Job applied next does not race the
			// delete (gt-tjhfe).
			timeout := strconv.Itoa(int(cacheTrimDeleteTimeout.Seconds())) + "s"
			args = append(args, "--wait=true", "--timeout="+timeout)
		}
		if _, err := d.cacheTrimKubectl(ctx, kubeconfig, args...); err != nil {
			return false, fmt.Errorf("deleting the stale %s %s: %w", object.Kind, object.Name, err)
		}
		d.logger.Printf("disk_maintenance: cache trim %s: deleted the stale %s %s", jobName, object.Kind, object.Name)
	}

	docs := m.Documents()
	for i, doc := range docs {
		// -n names the namespace the document was built for; an apply that
		// relied on the document alone would run in the caller's default
		// namespace.
		if _, stderr, err := d.cacheTrimKubectlRun(ctx, kubeconfig, doc, cacheTrimApplyArgs(namespace)...); err != nil {
			// The first line of stderr is the API server's reason; the lines
			// after it can repeat the object, and a shard's object holds a
			// file list.
			return false, fmt.Errorf("applying document %d of %d%s: %w: %s",
				i+1, len(docs), cacheTrimDocumentLabel(doc), err, cacheTrimFirstLine(string(stderr)))
		}
	}
	// The claim route waits here too (gt-gg9hv.46): cacheTrimMeasure reads the
	// claim only once its Job has finished, because a claim still being trimmed
	// holds what the trim is removing. The wait is bounded by
	// cacheTrimWaitTimeout (2 minutes); a Job still running then is left for the
	// next run.
	if !d.cacheTrimWaitJob(ctx, kubeconfig, namespace, jobName) {
		return false, nil
	}
	if err := d.cacheTrimCleanShards(ctx, kubeconfig, namespace, m); err != nil {
		return false, err
	}
	return true, nil
}

// cacheTrimWaitJob waits for the trim Job to complete, and reports whether it
// did. The wait is the pass's one bound on a Job: a Job still running when it
// ends is reported with why, and the pass carries on, leaving the shards for
// the next run and the claim unmeasured, since neither can be judged while the
// Job works.
func (d *Daemon) cacheTrimWaitJob(ctx context.Context, kubeconfig, namespace, jobName string) bool {
	timeout := strconv.Itoa(int(cacheTrimWaitTimeout.Seconds())) + "s"
	_, stderr, err := d.cacheTrimKubectlRun(ctx, kubeconfig, nil, "wait", "--for=condition=complete", "job/"+jobName, "-n", namespace, "--timeout="+timeout)
	if err == nil {
		return true
	}
	d.cacheTrimLogf("disk_maintenance: cache trim %s: %s; the trim is left for the next run", jobName, d.cacheTrimWaitFailure(ctx, kubeconfig, namespace, jobName, stderr))
	return false
}

// cacheTrimFieldManager is the field manager every document of a trim is
// applied under. A fixed name makes a rerun of the same trim an update of the
// records the earlier run owns rather than a conflict with it.
const cacheTrimFieldManager = "gt-cache-trim"

// cacheTrimApplyArgs is the argv every document of a trim is applied with:
// server-side apply, so kubectl writes no
// kubectl.kubernetes.io/last-applied-configuration annotation. Client-side
// apply writes the whole object into that annotation, and a shard ConfigMap's
// file list on its own exceeds the 262144-byte metadata limit, so the API
// server refused the object and the trim could not start (gt-gg9hv.40).
// --force-conflicts takes back a field a manager that no longer applies still
// owns, such as the client-side manager an earlier trim applied under.
func cacheTrimApplyArgs(namespace string) []string {
	return []string{
		"apply", "--server-side", "--field-manager=" + cacheTrimFieldManager,
		"--force-conflicts", "-f", "-", "-n", namespace,
	}
}

// cacheTrimDocumentLabel is the kind and name an applied document carries, for
// an apply failure: " (ConfigMap gt-cache-trim-0)", or "" when the document
// names neither. The manifest builder renders every document as an object, so
// the empty form only costs a message detail; the apply it accompanies has
// already failed and is still reported.
func cacheTrimDocumentLabel(doc []byte) string {
	var obj struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(doc, &obj); err != nil || obj.Kind == "" || obj.Metadata.Name == "" {
		return ""
	}
	return fmt.Sprintf(" (%s %s)", obj.Kind, obj.Metadata.Name)
}

// cacheTrimFirstLine returns the first line of kubectl's stderr, the reason a
// command failed. The lines after it can repeat the object's text, which a
// shard's file list makes long, so only the first line reaches an error.
func cacheTrimFirstLine(stderr string) string {
	return strings.TrimSpace(strings.Split(strings.TrimSpace(stderr), "\n")[0])
}

// cacheTrimCleanShards deletes the shard ConfigMaps a finished trim applied. A
// Job a trim applies folds in the claim within the pass, and the shards are its
// input alone; the next run would leave them behind otherwise, since a rerun of
// the same trim names the same shards and updates them rather than deleting
// them.
//
// The caller reaches this only once the Job has finished: deleting a mount a
// running pod reads would fail the trim it is part of. A Job that did not
// finish keeps its shards, and the next run deletes the Job, and its pods with
// it, before it applies.
func (d *Daemon) cacheTrimCleanShards(ctx context.Context, kubeconfig, namespace string, m *cachevol.TrimManifest) error {
	if len(m.Shards()) == 0 {
		return nil
	}
	if _, err := d.cacheTrimKubectl(ctx, kubeconfig, "delete", "configmap", "-n", namespace, "-l", cachevol.TrimSelector()); err != nil {
		return fmt.Errorf("deleting the trim's shards: %w", err)
	}
	return nil
}

// cacheTrimWaitFailure says why the wait ended without the Job completing: the
// Job failed, the wait timed out, or kubectl could not watch the Job. Each is a
// different repair, so each is a different line.
//
// Every outcome leaves the shards in place, so a status that cannot be read
// only costs the line its precision; it does not change what the pass does.
func (d *Daemon) cacheTrimWaitFailure(ctx context.Context, kubeconfig, namespace, jobName string, stderr []byte) string {
	if d.cacheTrimJobFailed(ctx, kubeconfig, namespace, jobName) {
		return "the Job failed"
	}
	if bytes.Contains(stderr, []byte("timed out waiting for the condition")) {
		return "the Job has not finished within the wait (timed out)"
	}
	return "the wait on the Job failed"
}

// cacheTrimJobFailed reports whether the Job's own status says it failed: a
// failed count, or a Failed condition set true.
func (d *Daemon) cacheTrimJobFailed(ctx context.Context, kubeconfig, namespace, jobName string) bool {
	out, err := d.cacheTrimKubectl(ctx, kubeconfig, "get", "job", jobName, "-n", namespace, "-o", "json")
	if err != nil {
		return false
	}
	var job struct {
		Status struct {
			Failed     int `json:"failed"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal(out, &job) != nil {
		return false
	}
	if job.Status.Failed > 0 {
		return true
	}
	for _, condition := range job.Status.Conditions {
		if condition.Type == "Failed" && condition.Status == "True" {
			return true
		}
	}
	return false
}

// cacheTrimExisting lists the objects a trim applies in namespace: the Jobs and
// shard ConfigMaps carrying TrimSelector, the labels only a trim applies, and
// which of the Jobs still run.
//
// A reply without an item list refuses: an unreadable answer would read as "no
// trim ran here", and the next apply's Job would be refused by the running one.
func (d *Daemon) cacheTrimExisting(ctx context.Context, kubeconfig, namespace string) ([]cachevol.TrimObject, map[string]bool, error) {
	out, err := d.cacheTrimKubectl(ctx, kubeconfig, "get", "job,configmap", "-n", namespace, "-l", cachevol.TrimSelector(), "-o", "json")
	if err != nil {
		return nil, nil, fmt.Errorf("listing the trim's objects: %w", err)
	}
	var list struct {
		Items *[]struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Active int `json:"active"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, nil, fmt.Errorf("decoding the trim's objects: %w", err)
	}
	if list.Items == nil {
		return nil, nil, errors.New("decoding the trim's objects: the reply names no object list")
	}
	objects := make([]cachevol.TrimObject, 0, len(*list.Items))
	running := map[string]bool{}
	for _, item := range *list.Items {
		objects = append(objects, cachevol.TrimObject{Kind: item.Kind, Name: item.Metadata.Name, Labels: item.Metadata.Labels})
		if item.Kind == "Job" && item.Status.Active > 0 {
			running[item.Metadata.Name] = true
		}
	}
	return objects, running, nil
}

// cacheTrimClaimNamespaces reads the cluster's PersistentVolumeClaims and
// returns, per claim name, the namespaces that hold one, sorted. The namespace
// a trim runs in is read from the claim item itself: a pod that mounts a claim
// of the same name in another namespace is not this claim's sandbox.
//
// This is the pass's one cluster-wide read; every call after it names the
// namespace it found here. A reply that cannot be decoded or names no item list
// refuses: an unreadable answer would read as "no such claim", and the trim
// would never run.
func (d *Daemon) cacheTrimClaimNamespaces(ctx context.Context, kubeconfig string) (map[string][]string, error) {
	out, err := d.cacheTrimKubectl(ctx, kubeconfig, "get", "persistentvolumeclaim", "-A", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items *[]struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decoding the claim list: %w", err)
	}
	if list.Items == nil {
		return nil, errors.New("decoding the claim list: the reply names no item list")
	}
	namespaces := map[string][]string{}
	for _, item := range *list.Items {
		if item.Metadata.Name == "" || item.Metadata.Namespace == "" {
			return nil, errors.New("decoding the claim list: an item names no name or no namespace")
		}
		namespaces[item.Metadata.Name] = append(namespaces[item.Metadata.Name], item.Metadata.Namespace)
	}
	for _, found := range namespaces {
		slices.Sort(found)
	}
	return namespaces, nil
}

// cacheTrimSource is a pod a listing can be read through: the container that
// mounts the claim and the claim's path inside it.
type cacheTrimSource struct {
	Namespace string
	Pod       string
	Container string
	Root      string
}

// cacheTrimPods reads the pods in namespace that mount claim, and returns how
// many of them hold it -- PlanTrim's sandbox count -- with the pods a listing
// can be read through, in a stable order.
//
// A pod that has finished holds nothing: it runs no sandbox and cannot be
// exec'd, so it is neither counted nor a source. A Pending pod counts, because
// a sandbox that has not started still holds the cache; only a running pod can
// answer an exec, so only a running pod is a source.
//
// A reply that cannot be decoded, names no item list, or names a pod of another
// namespace refuses: an unreadable reply would read as no sandbox running, which
// is a claim of idle again.
func (d *Daemon) cacheTrimPods(ctx context.Context, kubeconfig, namespace, claim string) (int, []cacheTrimSource, error) {
	out, err := d.cacheTrimKubectl(ctx, kubeconfig, "get", "pods", "-n", namespace, "-o", "json")
	if err != nil {
		return 0, nil, err
	}
	var list struct {
		Items *[]cacheTrimPod `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return 0, nil, fmt.Errorf("decoding the pod list: %w", err)
	}
	if list.Items == nil {
		return 0, nil, errors.New("decoding the pod list: the reply names no item list")
	}

	sandboxes, sources := 0, []cacheTrimSource{}
	for _, pod := range *list.Items {
		if pod.Metadata.Namespace != "" && pod.Metadata.Namespace != namespace {
			return 0, nil, fmt.Errorf("the pod list for namespace %s names the pod %s in %s", namespace, pod.Metadata.Name, pod.Metadata.Namespace)
		}
		volume := cacheTrimVolume(pod, claim)
		if volume == "" {
			continue
		}
		if pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed" {
			continue
		}
		sandboxes++
		if pod.Status.Phase != "Running" {
			continue
		}
		if source, ok := cacheTrimPodSource(pod, volume); ok {
			sources = append(sources, source)
		}
	}
	slices.SortFunc(sources, func(a, b cacheTrimSource) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		if c := strings.Compare(a.Pod, b.Pod); c != 0 {
			return c
		}
		return strings.Compare(a.Container, b.Container)
	})
	return sandboxes, sources, nil
}

// cacheTrimPod is the part of a pod `kubectl get pods -o json` names that finds
// the claim's mount.
type cacheTrimPod struct {
	Metadata cacheTrimPodMeta `json:"metadata"`
	Spec     cacheTrimPodSpec `json:"spec"`
	Status   struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

type cacheTrimPodMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type cacheTrimPodSpec struct {
	Containers []cacheTrimContainer `json:"containers"`
	Volumes    []cacheTrimPodVolume `json:"volumes"`
}

type cacheTrimContainer struct {
	Name         string                 `json:"name"`
	VolumeMounts []cacheTrimVolumeMount `json:"volumeMounts"`
}

type cacheTrimVolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

type cacheTrimPodVolume struct {
	Name                  string                          `json:"name"`
	PersistentVolumeClaim *cacheTrimPersistentVolumeClaim `json:"persistentVolumeClaim"`
}

type cacheTrimPersistentVolumeClaim struct {
	ClaimName string `json:"claimName"`
}

// cacheTrimVolume is the name of the pod's volume that mounts claim, or "" when
// the pod mounts none.
func cacheTrimVolume(pod cacheTrimPod, claim string) string {
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claim {
			return volume.Name
		}
	}
	return ""
}

// cacheTrimPodSource is where the claim sits in the pod's container that mounts
// it: the first container, by name, that mounts the claim's volume.
func cacheTrimPodSource(pod cacheTrimPod, volume string) (cacheTrimSource, bool) {
	containers := slices.Clone(pod.Spec.Containers)
	slices.SortFunc(containers, func(a, b cacheTrimContainer) int { return strings.Compare(a.Name, b.Name) })
	for _, container := range containers {
		for _, mount := range container.VolumeMounts {
			if mount.Name != volume || mount.MountPath == "" {
				continue
			}
			return cacheTrimSource{
				Namespace: pod.Metadata.Namespace,
				Pod:       pod.Metadata.Name,
				Container: container.Name,
				Root:      mount.MountPath,
			}, true
		}
	}
	return cacheTrimSource{}, false
}

// cacheTrimListing reads the claim's listing through a pod that mounts it, with
// the image's trim-list helper, and decodes what PlanTrim decides on.
func (d *Daemon) cacheTrimListing(ctx context.Context, kubeconfig string, source cacheTrimSource) ([]cachevol.Entry, error) {
	out, err := d.cacheTrimKubectl(ctx, kubeconfig, "exec", "-n", source.Namespace, source.Pod, "-c", source.Container,
		"--", cachevol.HelperPath, "trim-list", "--root", source.Root)
	if err != nil {
		return nil, err
	}
	return cacheTrimParseListing(out)
}

// cacheTrimDfHeader is the columns df -Pk prints above its one data row.
var cacheTrimDfHeader = []string{"Filesystem", "1024-blocks", "Used", "Available", "Capacity", "Mounted", "on"}

// cacheTrimDfReading is what df -Pk says about the claim, in bytes: the space
// left. Only Available is read, for the fit check: on a local-path claim df's
// Used is the whole node filesystem, so the space a trim leaves behind is read
// with du (gt-eyjgal).
type cacheTrimDfReading struct {
	Available int64
}

// cacheTrimDf reads the claim's free space, in bytes, with df in the pod that
// mounts it: df is a separate command, not a helper subcommand,
// because the image's runProgram dispatches only its known subcommands and
// sends any other first argument to a full plugin run
// (internal/polecatimage/program.go). The daemon must never ask an older image
// for one (gt-gg9hv.41).
//
// A df that fails, is absent, prints anything but the expected header and one
// data row, or whose Available column is not a whole number of KiB returns an
// error. The caller reads that as an unknown reading, names the error, and
// carries on rather than failing the trim.
func (d *Daemon) cacheTrimDf(ctx context.Context, kubeconfig string, source cacheTrimSource) (cacheTrimDfReading, error) {
	out, err := d.cacheTrimKubectl(ctx, kubeconfig, "exec", "-n", source.Namespace, source.Pod, "-c", source.Container,
		"--", "df", "-Pk", source.Root)
	if err != nil {
		return cacheTrimDfReading{}, err
	}
	return cacheTrimParseDf(out)
}

// cacheTrimParseDf turns df -Pk's reply into the claim's free bytes: the
// Available column is in KiB, and the reply must be the header and one data row
// and nothing else. The Used column is not read, so its value is not checked. A
// reply that is not that shape is an error naming the rule, never a zero
// figure, since a zero would refuse every claim-carried trim.
func cacheTrimParseDf(out []byte) (cacheTrimDfReading, error) {
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 2 {
		return cacheTrimDfReading{}, fmt.Errorf("df printed %d non-empty line(s), want the header and one data row", len(lines))
	}
	if header := strings.Fields(lines[0]); !slices.Equal(header, cacheTrimDfHeader) {
		return cacheTrimDfReading{}, errors.New("df printed an unexpected header")
	}
	// Used sits before Available, so a row with an Available column has at
	// least four fields, Used among them.
	row := strings.Fields(lines[1])
	if len(row) < 4 {
		return cacheTrimDfReading{}, errors.New("df's data row names no Available column")
	}
	available, err := cacheTrimKiB("df", "Available", row[3])
	if err != nil {
		return cacheTrimDfReading{}, err
	}
	return cacheTrimDfReading{Available: available}, nil
}

// cacheTrimDu reads the claim's own used space, in bytes, with du in the pod
// that mounts it. du is the claim's size where df's Used is the node's whole
// filesystem on a local-path claim, so a trim that removes the claim's files
// moves only du's figure (gt-eyjgal). The command runs the same way as
// cacheTrimDf: a separate binary in the listing's pod and container, never a
// helper subcommand an older image would send to a full plugin run
// (gt-gg9hv.41), and with only `du -sk`, which busybox and GNU/BSD du accept.
//
// A du that fails, is absent, prints anything but one line, or whose size is
// not a whole number of KiB returns an error. The caller reads that as an
// unknown reading, names the error, and raises nothing.
func (d *Daemon) cacheTrimDu(ctx context.Context, kubeconfig string, source cacheTrimSource) (int64, error) {
	out, err := d.cacheTrimKubectl(ctx, kubeconfig, "exec", "-n", source.Namespace, source.Pod, "-c", source.Container,
		"--", "du", "-sk", source.Root)
	if err != nil {
		return 0, err
	}
	return cacheTrimParseDu(out)
}

// cacheTrimParseDu turns du -sk's reply into the claim's used bytes: one line
// naming the size in whole KiB and the path du measured, and nothing else. A
// reply that is not that shape is an error naming the rule, never a zero
// figure: a zero would drop the item for a claim nobody read.
func cacheTrimParseDu(out []byte) (int64, error) {
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		return 0, fmt.Errorf("du printed %d non-empty line(s), want one", len(lines))
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 2 {
		return 0, errors.New("du's line names no path")
	}
	return cacheTrimKiB("du", "size", fields[0])
}

// cacheTrimKiB reads one whole-KiB column of command's output as bytes: the
// figure is never negative and small enough that its byte count fits an int64.
func cacheTrimKiB(command, column, field string) (int64, error) {
	kib, err := strconv.ParseInt(field, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s's %s column is not a whole number of KiB", command, column)
	}
	if kib < 0 {
		return 0, fmt.Errorf("%s names %d KiB %s", command, kib, strings.ToLower(column))
	}
	if kib > math.MaxInt64/1024 {
		return 0, fmt.Errorf("%s's %s column is too large to read as bytes", command, column)
	}
	return kib * 1024, nil
}

// cacheTrimListingEntry is one entry of the JSON `polecat-image trim-list`
// prints.
type cacheTrimListingEntry struct {
	Path  string    `json:"path"`
	Cache string    `json:"cache"`
	Size  int64     `json:"size"`
	Atime time.Time `json:"atime"`
	Mtime time.Time `json:"mtime"`
}

// cacheTrimParseListing turns the helper's JSON into PlanTrim's entries. The
// reply is the bare array trim-list has always printed; a document is refused,
// because that is the reply an older daemon cannot parse and the room this one
// needs is read with df, not carried here. A cache name it does not know
// refuses the listing: PlanTrim chooses what to remove by the cache, and a name
// read as the wrong one would admit a module file to a trim while a sandbox
// builds.
func cacheTrimParseListing(out []byte) ([]cachevol.Entry, error) {
	var entries []cacheTrimListingEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("decoding the trim listing: %w", err)
	}
	listing := make([]cachevol.Entry, 0, len(entries))
	for _, entry := range entries {
		var cache cachevol.Cache
		switch entry.Cache {
		case polecatimage.TrimListCacheBuild:
			cache = cachevol.BuildCache
		case polecatimage.TrimListCacheModule:
			cache = cachevol.ModuleCache
		default:
			return nil, fmt.Errorf("the trim listing names the cache %q for %s: want %s or %s",
				entry.Cache, entry.Path, polecatimage.TrimListCacheBuild, polecatimage.TrimListCacheModule)
		}
		listing = append(listing, cachevol.Entry{
			Path:  entry.Path,
			Cache: cache,
			Size:  entry.Size,
			Atime: entry.Atime,
			Mtime: entry.Mtime,
		})
	}
	return listing, nil
}

// cacheTrimKubectlEnvKeys are the only variables a trim's kubectl runs with:
// PATH and HOME for the binary and its cache, and KUBECONFIG for a config the
// flag does not name.
var cacheTrimKubectlEnvKeys = []string{"PATH", "HOME", "KUBECONFIG"}

// cacheTrimKubectlEnv is the environment kubectl runs with. The daemon's whole
// environment stays out: a cluster client has no use for the town's settings,
// and every variable named here is one a kubeconfig and a binary need.
//
// The registry target's in-use read passes no variable of its own through the
// same runner (internal/diskmaint/registry/inuse.go), so these three are the
// whole contract.
func cacheTrimKubectlEnv(environ []string) []string {
	env := make([]string, 0, len(cacheTrimKubectlEnvKeys))
	for _, key := range cacheTrimKubectlEnvKeys {
		if value, ok := environValue(environ, key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// environValue returns key's value in environ, the last entry winning as it
// would for a process.
func environValue(environ []string, key string) (string, bool) {
	for _, kv := range slices.Backward(environ) {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// cacheTrimKubectl runs one kubectl command that sends no document, through the
// daemon's exec seam, so the unit tier never launches kubectl.
//
// A failure names the command and its last stderr line, never its whole output,
// which can hold a secret.
func (d *Daemon) cacheTrimKubectl(ctx context.Context, kubeconfig string, args ...string) ([]byte, error) {
	stdout, stderr, err := d.cacheTrimKubectlRun(ctx, kubeconfig, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, lastLine(string(stderr)))
	}
	return stdout, nil
}

// cacheTrimKubectlRun runs one kubectl command and returns its streams too, for
// the waiter that tells a timeout from a Job that failed and for the apply that
// names the API server's first stderr line. stdin, when it is not nil, is the
// document an apply reads. Every call starts with the shared prefix, so each
// names the kubeconfig and the sandbox context and none relies on the file's
// current-context.
func (d *Daemon) cacheTrimKubectlRun(ctx context.Context, kubeconfig string, stdin []byte, args ...string) ([]byte, []byte, error) {
	argv := append(diskmaint.KubectlArgs(kubeconfig, diskMaintSandboxContext), args...)
	cmd := exec.CommandContext(ctx, "kubectl", argv...) //nolint:gosec // G204: a fixed program, fixed verbs, and names validated as DNS-1123 labels
	cmd.Env = cacheTrimKubectlEnv(d.diskMaintenanceSeams().environ())
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return d.runCmd(cmd)
}
