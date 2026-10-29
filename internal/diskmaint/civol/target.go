package civol

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// Compile-time check that the civol target satisfies the diskmaint contract.
var _ diskmaint.Target = (*Target)(nil)

// Defaults for the containers and image the CI runner runs under.
const (
	defaultDind   = "forgejo-docker-in-docker-1"
	defaultRunner = "forgejo-runner-daemon-1"
	defaultImage  = "ci-base:latest"
)

// volumeRoot is where a throwaway container mounts the volume it reads.
const volumeRoot = "/c"

// startedAtLayout reads the first 19 characters of docker's StartedAt, which
// must be UTC for the age rules to compare it with an entry's MTime.
const startedAtLayout = "2006-01-02T15:04:05"

// deferredApply is the Result.Deferred text Apply returns until the trim lands
// in the bead that follows this one (gt-z6qre.10).
const deferredApply = "not implemented: this target only plans; the trim lands in the apply bead"

// ruleDindDown keeps a plan empty while the docker-in-docker container is not
// running: its volumes cannot be read, and a stopped runner has no cache to
// trim.
const ruleDindDown = "dind not running"

// The sh scripts a throwaway container runs to list a volume. Each is fixed so
// the output shape a Go parser reads is the same on every run; find prints
// "<mtime>\t<bytes or KB>\t<path>" and du prints "<KB>\t<path>".
const (
	sizeScript        = `du -sk /c | cut -f1`
	fileListingScript = `find /c -type f -printf '%T@\t%s\t%p\n'`
	// moduleFind names the whole "<module>@<version>" directories under
	// pkg/mod. It prunes ./cache first: the download cache's "@v" directories
	// match "*@*" too, and nothing there may be removed (gt-z6qre.9).
	moduleFind          = `find . -maxdepth 8 -path ./cache -prune -o -type d -name '*@*' -prune`
	moduleListingScript = `cd /c/pkg/mod && ` + moduleFind + ` -printf '%T@\t%p\n'`
	moduleSizesScript   = `cd /c/pkg/mod && ` + moduleFind + ` -exec du -sk {} +`
)

var (
	// errNoRunner reports a target with no docker runner: every read this
	// target makes is a docker command, so its plan refuses rather than report
	// volumes it never looked at.
	errNoRunner = errors.New("civol: no docker runner configured")
	// errDindState reports a docker-in-docker container whose running state
	// cannot be read, so whether the volumes are in use is unknown.
	errDindState = errors.New("docker inspect of the docker-in-docker container failed; its state is unreadable")
	// errJobs reports an unreadable job list inside the docker-in-docker
	// container.
	errJobs = errors.New("docker ps in the docker-in-docker container failed; the running jobs are unreadable")
	// errJobStart reports an unreadable start time for a running job, so the
	// oldest start is unknown.
	errJobStart = errors.New("docker inspect of a running job failed; its start is unreadable")
	// errDF reports a runner disk whose df cannot be read.
	errDF = errors.New("docker exec df failed; the runner disk is unreadable")
	// errVolumes reports a volume list that cannot be read, so which volumes
	// exist is unknown.
	errVolumes = errors.New("docker volume ls in the docker-in-docker container failed; the volumes are unreadable")
	// errVolumeSize reports a volume whose size cannot be read.
	errVolumeSize = errors.New("measuring the volume failed; its size is unreadable")
	// errVolumeList reports a volume whose entries cannot be listed.
	errVolumeList = errors.New("listing the volume failed; its entries are unreadable")
	// errParse reports output this file does not understand. Reading part of a
	// listing would plan on a guess, so it refuses instead.
	errParse = errors.New("docker output does not parse; the listing is refused")
)

// Config is what the civol target needs to read the CI volumes and plan a trim.
type Config struct {
	// Dind and Runner are the docker-in-docker and runner container names; a
	// blank field takes the default the CI runner uses.
	Dind, Runner string
	// Image is the container image that reads and trims the volumes; a blank
	// field takes ci-base:latest.
	Image string
	// Limits are the caps and trim targets per volume; empty takes DefaultLimits.
	Limits []Limit
	// Run runs docker on the host; a test passes a fake that returns canned
	// output, so no test reaches a real daemon.
	Run diskmaint.Runner
	// Ledger records an apply, which this target does not do yet.
	Ledger *diskmaint.Ledger
}

// Target reads the CI cache volumes and plans their trim.
type Target struct {
	cfg Config
}

// NewTarget returns a Target with the blank fields of cfg filled from the CI
// runner's defaults.
func NewTarget(cfg Config) *Target {
	if cfg.Dind == "" {
		cfg.Dind = defaultDind
	}
	if cfg.Runner == "" {
		cfg.Runner = defaultRunner
	}
	if cfg.Image == "" {
		cfg.Image = defaultImage
	}
	if len(cfg.Limits) == 0 {
		cfg.Limits = slices.Clone(DefaultLimits)
	}
	return &Target{cfg: cfg}
}

// Name is the target's name in plans and ledger entries.
func (t *Target) Name() string { return planTarget }

// Measure reports the runner container's disk as df -P reads it. An unreadable
// df is an error, never a zero measurement.
func (t *Target) Measure(ctx context.Context) (diskmaint.Measurement, error) {
	if t.cfg.Run == nil {
		return diskmaint.Measurement{}, errNoRunner
	}
	out, err := t.docker(ctx, "exec", t.cfg.Runner, "df", "-P", "/")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return diskmaint.Measurement{}, ctxErr
		}
		return diskmaint.Measurement{}, fmt.Errorf("measure: %w", errDF)
	}
	used, capacity, err := parseDF(out)
	if err != nil {
		return diskmaint.Measurement{}, fmt.Errorf("measure: %w", err)
	}
	return diskmaint.Measurement{
		Used:     used,
		Capacity: capacity,
		Detail:   fmt.Sprintf("runner disk: %s used of %s", bytesText(used), bytesText(capacity)),
	}, nil
}

// Plan reads the running jobs, then each volume in Limits, and returns
// BuildPlan's decision. A stopped docker-in-docker container gives an empty
// plan with one kept row; a read that fails refuses the plan rather than plan
// on a guess. Plan changes nothing.
func (t *Target) Plan(ctx context.Context, _ diskmaint.Policy) (diskmaint.Plan, error) {
	plan := diskmaint.Plan{Target: planTarget}
	if t.cfg.Run == nil {
		plan.Refused = errNoRunner.Error()
		return plan, nil
	}

	running, err := t.dindRunning(ctx)
	if err != nil {
		return planOrErr(ctx, err)
	}
	if !running {
		plan.Kept = append(plan.Kept, diskmaint.Kept{Ref: t.cfg.Dind, Rule: ruleDindDown})
		return plan, nil
	}

	in := Input{Limits: slices.Clone(t.cfg.Limits)}
	in.RunningJobs, in.OldestJobStart, err = t.jobs(ctx)
	if err != nil {
		return planOrErr(ctx, err)
	}
	// Which volumes exist comes from the list, never from a failed inspect: a
	// volume that is not there must not be told apart by docker's message, and
	// a list that cannot be read must refuse rather than report all missing.
	present, err := t.volumeNames(ctx)
	if err != nil {
		return planOrErr(ctx, err)
	}
	for _, lim := range t.cfg.Limits {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return diskmaint.Plan{}, ctxErr
		}
		v, readErrs, err := t.volume(ctx, lim.Name, present)
		if err != nil {
			return diskmaint.Plan{}, err
		}
		in.Volumes = append(in.Volumes, v)
		in.ReadErrs = append(in.ReadErrs, readErrs...)
	}
	return BuildPlan(in), nil
}

// Apply removes nothing yet: this target only reads and plans, and the trim is
// the bead that follows this one.
func (t *Target) Apply(context.Context, diskmaint.Plan) (diskmaint.Result, error) {
	return diskmaint.Result{Target: planTarget, Deferred: deferredApply}, nil
}

// dindRunning reports whether the docker-in-docker container is up. A container
// that is not there at all is an unreadable state rather than a stopped one,
// because the name may also be wrong.
func (t *Target) dindRunning(ctx context.Context) (bool, error) {
	out, err := t.docker(ctx, "inspect", "-f", "{{.State.Running}}", t.cfg.Dind)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, errDindState
	}
	switch strings.TrimSpace(string(out)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, errDindState
}

// jobs counts the containers a running job holds and reads the earliest start
// among them. A start that does not parse leaves the oldest start unknown, so
// the gated volume waits instead of trimming around a job of unknown age. That
// is stricter than "zero only when none parse": one unreadable start can hide a
// job older than every start this target could read.
func (t *Target) jobs(ctx context.Context) (count int, oldest time.Time, err error) {
	out, err := t.dind(ctx, "ps", "-q")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, time.Time{}, ctxErr
		}
		return 0, time.Time{}, errJobs
	}
	ids := nonEmptyLines(string(out))
	if len(ids) == 0 {
		return 0, time.Time{}, nil
	}
	known := true
	for _, id := range ids {
		start, err := t.jobStart(ctx, id)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return 0, time.Time{}, ctxErr
			}
			return 0, time.Time{}, err
		}
		if start.IsZero() {
			known = false
			continue
		}
		if oldest.IsZero() || start.Before(oldest) {
			oldest = start
		}
	}
	if !known {
		return len(ids), time.Time{}, nil
	}
	return len(ids), oldest, nil
}

// jobStart reads one job container's start. A start that does not parse is an
// unknown time (the zero value), not a failed read: docker answered, and the
// answer is not a timestamp this target can use.
func (t *Target) jobStart(ctx context.Context, id string) (time.Time, error) {
	out, err := t.dind(ctx, "inspect", "-f", "{{.State.StartedAt}}", id)
	if err != nil {
		return time.Time{}, errJobStart
	}
	return parseStartedAt(string(out)), nil
}

// volumeNames lists the volume names the docker-in-docker daemon holds. An
// unreadable list is an error, never an empty one: every volume would otherwise
// read as missing.
func (t *Target) volumeNames(ctx context.Context) (map[string]bool, error) {
	out, err := t.dind(ctx, "volume", "ls", "--format", "{{.Name}}")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errVolumes
	}
	present := make(map[string]bool)
	for _, name := range nonEmptyLines(string(out)) {
		present[strings.TrimSpace(name)] = true
	}
	return present, nil
}

// volume reads one volume: whether it exists, its size in KB and its entries.
// It returns read errors rather than fail, so one unreadable volume refuses the
// plan instead of hiding what the others hold.
func (t *Target) volume(ctx context.Context, name string, present map[string]bool) (VolumeInput, []string, error) {
	v := VolumeInput{Name: name}
	if !present[name] {
		return v, nil, nil
	}
	v.Exists = true

	size, err := t.volumeSize(ctx, name)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return v, nil, ctxErr
		}
		return v, []string{fmt.Sprintf("%s: %v", name, errVolumeSize)}, nil
	}
	v.SizeKB = size

	entries, readErrs, err := t.entries(ctx, name)
	if err != nil {
		return v, nil, err
	}
	v.Entries = entries
	return v, readErrs, nil
}

// volumeSize reads a volume's size in KB from a read-only mount of it.
func (t *Target) volumeSize(ctx context.Context, name string) (int64, error) {
	out, err := t.readOnlyScript(ctx, name, sizeScript)
	if err != nil {
		return 0, err
	}
	return parseKB(string(out))
}

// readOnlyScript runs script in a throwaway root container that mounts the
// volume read-only: the read commands share one mount shape, so none of them
// can change what it measures.
func (t *Target) readOnlyScript(ctx context.Context, name, script string) ([]byte, error) {
	return t.dind(ctx, "run", "--rm", "--user", "0",
		"-v", name+":"+volumeRoot+":ro", "--entrypoint", "sh", t.cfg.Image, "-c", script)
}

// entries lists what a volume holds. The module cache is listed by whole module
// directory, since only those may be removed; the others by file.
func (t *Target) entries(ctx context.Context, name string) ([]Entry, []string, error) {
	if name == gomodVolume {
		return t.moduleEntries(ctx, name)
	}
	return t.fileEntries(ctx, name)
}

// fileEntries lists the files of a build or npm cache volume.
func (t *Target) fileEntries(ctx context.Context, name string) ([]Entry, []string, error) {
	out, err := t.readOnlyScript(ctx, name, fileListingScript)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		return nil, []string{fmt.Sprintf("%s: %v", name, errVolumeList)}, nil
	}
	entries, err := parseFiles(string(out))
	if err != nil {
		return nil, []string{fmt.Sprintf("%s: %v", name, err)}, nil
	}
	return entries, nil, nil
}

// moduleEntries lists the whole "<module>@<version>" directories of the module
// cache and reads each one's size from du.
func (t *Target) moduleEntries(ctx context.Context, name string) ([]Entry, []string, error) {
	out, err := t.readOnlyScript(ctx, name, moduleListingScript)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		return nil, []string{fmt.Sprintf("%s: %v", name, errVolumeList)}, nil
	}
	modules, err := parseModules(string(out))
	if err != nil {
		return nil, []string{fmt.Sprintf("%s: %v", name, err)}, nil
	}

	sizesOut, err := t.readOnlyScript(ctx, name, moduleSizesScript)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		return nil, []string{fmt.Sprintf("%s: %v", name, errVolumeSize)}, nil
	}
	sizes, err := parseModuleSizes(string(sizesOut))
	if err != nil {
		return nil, []string{fmt.Sprintf("%s: %v", name, err)}, nil
	}

	entries := make([]Entry, 0, len(modules))
	for _, m := range modules {
		kb, ok := sizes[m.path]
		if !ok {
			return nil, []string{fmt.Sprintf("%s: %s has no size in the du listing", name, m.entryPath)}, nil
		}
		entries = append(entries, Entry{Path: m.entryPath, MTime: m.mtime, Bytes: kb * 1024})
	}
	return entries, nil, nil
}

// docker runs one docker command on the host and returns its stdout.
func (t *Target) docker(ctx context.Context, args ...string) ([]byte, error) {
	return t.cfg.Run(ctx, nil, "docker", args...)
}

// dind runs one docker command inside the docker-in-docker container, which is
// the daemon that holds the CI volumes and job containers.
func (t *Target) dind(ctx context.Context, args ...string) ([]byte, error) {
	argv := make([]string, 0, 3+len(args))
	argv = append(argv, "exec", t.cfg.Dind, "docker")
	argv = append(argv, args...)
	return t.docker(ctx, argv...)
}

// planOrErr refuses the plan for a read that failed, and hands back the
// caller's cancellation as an error instead: a plan cut short is not a decision
// about the volumes.
func planOrErr(ctx context.Context, err error) (diskmaint.Plan, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return diskmaint.Plan{}, ctxErr
	}
	return diskmaint.Plan{Target: planTarget, Refused: err.Error()}, nil
}

// module is one "<module>@<version>" directory a listing named, before its size
// is looked up.
type module struct {
	path      string // as du named it: ./<module>@<version>
	entryPath string // relative to the volume root: pkg/mod/<module>@<version>
	mtime     time.Time
}

// parseModules reads find's "<mtime>\t<path>" listing of module directories. A
// path that is not a whole module directory outside the download cache means
// the listing script and this parser disagree, so the listing is refused rather
// than planned on a path this target would not remove.
func parseModules(out string) ([]module, error) {
	var modules []module
	for _, line := range nonEmptyLines(out) {
		stamp, path, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, errParse
		}
		mtime, err := parseEpoch(stamp)
		if err != nil {
			return nil, errParse
		}
		rel, ok := moduleRelPath(path)
		if !ok {
			return nil, errParse
		}
		entryPath := "pkg/mod/" + rel
		if isModuleCache(entryPath) || !isModuleDir(entryPath) {
			return nil, errParse
		}
		modules = append(modules, module{path: path, entryPath: entryPath, mtime: mtime})
	}
	return modules, nil
}

// parseModuleSizes reads du's "<KB>\t<path>" listing, keyed by the path du
// printed so a module's size is matched to the directory find named.
func parseModuleSizes(out string) (map[string]int64, error) {
	sizes := make(map[string]int64)
	for _, line := range nonEmptyLines(out) {
		size, path, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, errParse
		}
		kb, err := strconv.ParseInt(strings.TrimSpace(size), 10, 64)
		if err != nil {
			return nil, errParse
		}
		sizes[path] = kb
	}
	return sizes, nil
}

// parseFiles reads find's "<mtime>\t<bytes>\t<path>" listing of files in a
// volume.
func parseFiles(out string) ([]Entry, error) {
	var entries []Entry
	for _, line := range nonEmptyLines(out) {
		stamp, rest, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, errParse
		}
		size, path, ok := strings.Cut(rest, "\t")
		if !ok {
			return nil, errParse
		}
		mtime, err := parseEpoch(stamp)
		if err != nil {
			return nil, errParse
		}
		n, err := strconv.ParseInt(strings.TrimSpace(size), 10, 64)
		if err != nil {
			return nil, errParse
		}
		rel, ok := volumeRelPath(path)
		if !ok {
			return nil, errParse
		}
		entries = append(entries, Entry{Path: rel, MTime: mtime, Bytes: n})
	}
	return entries, nil
}

// volumeRelPath turns a path find printed under the mount into the path the
// plan names, which is relative to the volume root.
func volumeRelPath(path string) (string, bool) {
	return strings.CutPrefix(path, volumeRoot+"/")
}

// moduleRelPath turns a path find printed from /c/pkg/mod into a path relative
// to the module volume's root.
func moduleRelPath(path string) (string, bool) {
	rel, ok := strings.CutPrefix(path, "./")
	if !ok || rel == "" {
		return "", false
	}
	return rel, true
}

// parseStartedAt reads docker's StartedAt, taking its first 19 characters as
// UTC. Output that is too short or does not parse is the zero time.
func parseStartedAt(text string) time.Time {
	s := strings.TrimSpace(text)
	if len(s) < len(startedAtLayout) {
		return time.Time{}
	}
	start, err := time.Parse(startedAtLayout, s[:len(startedAtLayout)])
	if err != nil {
		return time.Time{}
	}
	return start.UTC()
}

// parseEpoch reads find's %T@ timestamp: whole seconds, then a fraction.
func parseEpoch(text string) (time.Time, error) {
	whole, frac, _ := strings.Cut(strings.TrimSpace(text), ".")
	sec, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return time.Time{}, errParse
	}
	if frac == "" {
		return time.Unix(sec, 0).UTC(), nil
	}
	frac = frac[:min(len(frac), 9)]
	frac += strings.Repeat("0", 9-len(frac))
	nsec, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return time.Time{}, errParse
	}
	return time.Unix(sec, nsec).UTC(), nil
}

// parseKB reads du -sk's single number, in KB.
func parseKB(out string) (int64, error) {
	line := strings.TrimSpace(out)
	kb, err := strconv.ParseInt(line, 10, 64)
	if err != nil {
		return 0, errParse
	}
	return kb, nil
}

// parseDF reads df -P's totals for the one filesystem it was asked about: the
// 1024-blocks column is the capacity and the Used column the bytes in use.
func parseDF(out []byte) (used, capacity int64, err error) {
	lines := nonEmptyLines(string(out))
	if len(lines) < 2 {
		return 0, 0, errParse
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 3 {
		return 0, 0, errParse
	}
	blocks, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, 0, errParse
	}
	inUse, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return 0, 0, errParse
	}
	return inUse * 1024, blocks * 1024, nil
}

// nonEmptyLines splits output into its lines, dropping blank ones so a trailing
// newline does not read as an entry of its own.
func nonEmptyLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// bytesText renders a byte count in GB for a measurement or a rule.
func bytesText(b int64) string {
	return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
}
