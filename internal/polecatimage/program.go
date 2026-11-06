package polecatimage

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// Program runs one plugin run and then the trims that follow it: the old image
// tags, the build cache, the dangling images and the per-rig cache volumes.
//
// The trims follow a run that finished, a deferral included, and never follow a
// failure, whose answer Program returns (gt-tpg0p.7).
func (r *Run) Program() error {
	if err := r.Main(); err != nil {
		return err
	}
	r.PruneOldImages()
	r.PruneBuildCache()
	r.PruneDanglingImages()
	r.TrimCacheVolumes()
	r.PruneRegistry()
	return nil
}

// ConfigFromEnv reads a run's configuration through get, the environment the
// script read, and reports the variables it cannot carry on without: an
// unreadable POLECAT_IMAGE_STALE_MINUTES, which drives the lock takeover in the
// run path, and a town settings/config.json that exists and cannot be read,
// which leaves a registry it cannot resolve (gt-du3fi).
//
// The prune-only knobs are read as written and judged by the step that uses
// them, so a typo there stops that trim alone (gt-tpg0p.7).
func ConfigFromEnv(get func(string) string) (Config, error) {
	town, err := townRoot(get)
	if err != nil {
		return Config{}, err
	}
	stale := envOr(get, "POLECAT_IMAGE_STALE_MINUTES", "20")
	minutes, err := strconv.Atoi(stale)
	if err != nil || minutes <= 0 {
		return Config{}, fmt.Errorf("POLECAT_IMAGE_STALE_MINUTES=%s is not a whole number of minutes", stale)
	}
	registry, err := imageSetting(get, town, "POLECAT_IMAGE_REGISTRY")
	if err != nil {
		return Config{}, err
	}
	crane, err := imageSetting(get, town, "POLECAT_IMAGE_CRANE")
	if err != nil {
		return Config{}, err
	}
	if crane == "" {
		crane = "crane"
	}
	return Config{
		GT:             envOr(get, "POLECAT_IMAGE_GT", "gt"),
		BD:             envOr(get, "POLECAT_IMAGE_BD", "bd"),
		Docker:         envOr(get, "POLECAT_IMAGE_DOCKER", "docker"),
		Git:            envOr(get, "POLECAT_IMAGE_GIT", "git"),
		Make:           envOr(get, "POLECAT_IMAGE_MAKE", "make"),
		Pgrep:          envOr(get, "POLECAT_IMAGE_PGREP", "pgrep"),
		Openshell:      envOr(get, "POLECAT_IMAGE_OPENSHELL", "openshell"),
		Crane:          crane,
		RigRepo:        envOr(get, "POLECAT_IMAGE_RIG_REPO", filepath.Join(town, "gastown", ".repo.git")),
		BuildRoot:      envOr(get, "POLECAT_IMAGE_BUILD_ROOT", filepath.Join(town, "gastown", ".polecat-image-build")),
		LockDir:        envOr(get, "POLECAT_IMAGE_LOCK", filepath.Join(town, ".runtime", "polecat-image.lock")),
		StaleAfter:     time.Duration(minutes) * time.Minute,
		SandboxRecords: envOr(get, "POLECAT_IMAGE_SANDBOX_RECORDS", filepath.Join(town, ".runtime", "sandboxes")),
		Image:          envOr(get, "POLECAT_IMAGE_NAME", "gastown-polecat:dev"),
		Label:          "org.gastown.gt-commit",
		BdLabel:        "org.gastown.bd-commit",
		Registry:       registry,
		Published:      envOr(get, "POLECAT_IMAGE_PUBLISHED", filepath.Join(town, ".runtime", "polecat-image.published")),
		BdStuck:        filepath.Join(town, ".runtime", "polecat-image.bd-stuck"),
		// Only 1 turns it on, so a stray POLECAT_IMAGE_FAIL_CLOSED=0 cannot fail
		// a run (gt-efupr).
		FailClosed:         get("POLECAT_IMAGE_FAIL_CLOSED") == "1",
		Keep:               envOr(get, "POLECAT_IMAGE_KEEP", "3"),
		TrimContext:        get("POLECAT_IMAGE_TRIM_CONTEXT"),
		BuilderKeep:        envOr(get, "POLECAT_IMAGE_BUILDER_KEEP", "8GB"),
		CacheCap:           envOr(get, "POLECAT_CACHE_CAP", "10GB"),
		RegistryKeep:       envOr(get, "POLECAT_IMAGE_REGISTRY_KEEP", registryKeepDefault),
		RegistryPrune:      envOr(get, "POLECAT_IMAGE_REGISTRY_PRUNE", registryPruneDefault),
		RegistryKubeconfig: filepath.Join(town, ".runtime", "sbx-k8s", "kubeconfig"),
		RegistryLedger:     filepath.Join(town, ".runtime", "diskmaint", "ledger.jsonl"),
	}, nil
}

// townRoot is the town this run belongs to: the root the operator names, or the
// home directory's gt. Both unset is an error, because a run that guessed a
// town would build from another one's repo.
func townRoot(get func(string) string) (string, error) {
	if root := get("POLECAT_IMAGE_TOWN_ROOT"); root != "" {
		return root, nil
	}
	home := get("HOME")
	if home == "" {
		return "", fmt.Errorf("cannot find the town root: POLECAT_IMAGE_TOWN_ROOT and HOME are both unset")
	}
	return filepath.Join(home, "gt"), nil
}

// imageSetting is one of the two settings a town keeps for its image runs:
// get's value, which the daemon and the installer export, else the town's
// settings/config.json daemon.env, which a hand install's shell does not carry.
// An exported value wins, so a town whose config cannot be read fails no run
// that named both keys itself (gt-du3fi).
func imageSetting(get func(string) string, townRoot, key string) (string, error) {
	if v := get(key); v != "" {
		return v, nil
	}
	return townImageSetting(townRoot, key)
}

// envOr is the variable's value, or def when it is unset. Every path and program
// a run calls can be overridden this way, which is how a test or an operator
// stubs it.
func envOr(get func(string) string, key, def string) string {
	if v := get(key); v != "" {
		return v
	}
	return def
}

// Main is the program's entry point and answers its exit status.
func Main(args []string) int {
	return runProgram(args, os.Stdout, os.Stderr, os.Getenv)
}

// runProgram answers Main: no arguments, or the verb "run", runs one plugin run,
// and "trim-cache", "trim-paths", "trim-list", "trim-list-write", "seed-clone"
// and "seed-cache" are the helpers a container of the image runs: in the seed
// and trim Jobs, or through a kubectl exec into a pod that mounts the claim.
//
// Any other first argument is refused with exit 2 and the verbs this program
// knows, rather than run as a plugin run: a plugin run builds, publishes and
// trims, so a Job whose image predates the subcommand it names would print a
// deferral, complete, and trim nothing (gt-gg9hv.47). The refusal protects an
// image built since this change only: one built before it still falls through
// to the plugin run, so the Jobs pull a moving tag (gt-gg9hv.45).
func runProgram(args []string, stdout, stderr io.Writer, get func(string) string) int {
	return runProgramWith(args, stdout, stderr, get, runMain)
}

// pluginRun starts one plugin run: runMain, or a test's counter, which is how a
// test shows that a refusal started none.
type pluginRun func(out io.Writer, get func(string) string) int

// helperCall is one helper run: the arguments after the verb, the writers, and
// the plugin run the verb "run" starts. "run" ignores the arguments after it,
// as the program ignored every argument before it had verbs.
type helperCall struct {
	args   []string
	stdout io.Writer
	stderr io.Writer
	get    func(string) string
	plugin pluginRun
}

// helperRun is one first argument runProgram dispatches: the verb, and the
// helper it runs with the arguments after it.
type helperRun struct {
	verb string
	help func(helperCall) int
}

// helperRuns are the first arguments this program knows, in the order the
// refusal names them. The table is the only list: a verb added here is
// dispatched and named in the refusal together, so the two cannot disagree.
var helperRuns = []helperRun{
	{"run", func(c helperCall) int { return c.plugin(c.stdout, c.get) }},
	{"trim-cache", func(c helperCall) int { return trimCacheMain(c.args, c.stdout, c.stderr) }},
	{"trim-paths", func(c helperCall) int { return trimPathsMain(c.args, c.stdout, c.stderr) }},
	{"trim-list", func(c helperCall) int { return trimListMain(c.args, c.stdout, c.stderr) }},
	{"trim-list-write", func(c helperCall) int { return trimListWriteMain(c.args, c.stdout, c.stderr) }},
	{"seed-clone", func(c helperCall) int { return seedCloneMain(c.args, c.stdout, c.stderr) }},
	{"seed-cache", func(c helperCall) int { return seedCacheMain(c.args, c.stdout, c.stderr) }},
}

// runProgramWith answers runProgram over entry, the plugin run it starts.
func runProgramWith(args []string, stdout, stderr io.Writer, get func(string) string, entry pluginRun) int {
	if len(args) == 0 {
		return entry(stdout, get)
	}
	if h, ok := helperRunFor(args[0]); ok {
		return h.help(helperCall{args: args[1:], stdout: stdout, stderr: stderr, get: get, plugin: entry})
	}
	fmt.Fprintf(stderr, "polecat-image: unknown subcommand %q: known are %s\n", oneLineArgument(args[0]), helperVerbNames())
	return 2
}

// helperRunFor is the helper the verb names, and whether this program knows it.
func helperRunFor(verb string) (helperRun, bool) {
	for _, h := range helperRuns {
		if h.verb == verb {
			return h, true
		}
	}
	return helperRun{}, false
}

// helperVerbNames is the known verbs as the refusal lists them.
func helperVerbNames() string {
	verbs := make([]string, len(helperRuns))
	for i, h := range helperRuns {
		verbs[i] = h.verb
	}
	return strings.Join(verbs, ", ")
}

// oneLineArgument is arg as the refusal prints it: a newline, a terminal escape
// or any other character that is not printable becomes '?', so an argument
// cannot forge a second log line or drive the terminal the Job's log is read in.
func oneLineArgument(arg string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return '?'
	}, arg)
}

// runMain configures and runs one plugin run, and answers the process exit
// status: 1 when the run has logged its failure.
func runMain(out io.Writer, get func(string) string) int {
	cfg, err := ConfigFromEnv(get)
	r := &Run{Cfg: cfg, out: out}
	if err != nil {
		r.Logf("ERROR: %v", err)
		return 1
	}
	defer r.Cleanup()
	if err := r.Program(); err != nil {
		return 1
	}
	return 0
}

// trimCacheMain runs the helper: it trims the cache tree under --root to
// --cap-bytes.
//
// stdout carries "<under|trimmed> <bytes>", which is what the caller logs, and
// exit 3 with a reason on stderr means the tree could not be measured or
// trimmed: the caller reports a trim it cannot vouch for rather than a silent
// success.
func trimCacheMain(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("trim-cache", flag.ContinueOnError)
	flags.SetOutput(stderr)
	capText := flags.String("cap-bytes", "", "the size to bring the cache tree back under, in bytes")
	root := flags.String("root", cacheMount, "the cache tree to trim")
	if err := flags.Parse(args); err != nil {
		return 3
	}
	cap, err := strconv.ParseInt(*capText, 10, 64)
	if err != nil || cap < 0 {
		fmt.Fprintln(stderr, "trim-cache: --cap-bytes needs a whole number of bytes")
		return 3
	}
	res, err := TrimCacheTree(*root, cap, diskTrimWalk())
	if err != nil {
		fmt.Fprintf(stderr, "trim-cache: %v\n", err)
		return 3
	}
	if res.Before <= cap {
		fmt.Fprintf(stdout, "under %d\n", res.Before)
		return 0
	}
	fmt.Fprintf(stdout, "trimmed %d %d\n", res.Before, res.After)
	return 0
}

// trimPathsMain runs the trim Job's container over the disk.
func trimPathsMain(args []string, stdout, stderr io.Writer) int {
	return trimPathsRun(args, stdout, stderr, diskTrimPathsFS())
}

// trimPathsRun is trimPathsMain over the filesystem w, so a test answers the calls.
//
// The list comes from the mounted shards, or from the file a caller wrote into the
// claim when --list-file names it. Once the run reaches the list, stdout carries
// "removed N absent M", the counts of what it did, so a run that fails part way still
// says how much it removed. Exit 3 with a reason on stderr means the list could not be
// read whole or a removal failed: a trim the helper cannot vouch for is reported
// rather than passed off as a partial success.
func trimPathsRun(args []string, stdout, stderr io.Writer, w trimPathsFS) int {
	flags := flag.NewFlagSet("trim-paths", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", cachevol.SeedCacheRoot, "the claim the listed paths are relative to")
	list := flags.String("list", cachevol.TrimListRoot, "the directory the shard volumes are mounted under")
	listFile := flags.String("list-file", "", "the claim-relative file a claim-carried trim reads its list from")
	listDigest := flags.String("list-sha256-file", "", "the claim-relative file holding the digest of the claim-carried list")
	shardsText := flags.String("shards", "", "the number of shards the Job mounted")
	if err := flags.Parse(args); err != nil {
		return 3
	}
	var res TrimPathsResult
	var err error
	if *listFile != "" {
		if *shardsText != "" {
			fmt.Fprintln(stderr, "trim-paths: --list-file and --shards name two routes; give one")
			return 3
		}
		if *listDigest == "" {
			fmt.Fprintln(stderr, "trim-paths: --list-file needs --list-sha256-file, the digest the writer put beside the list")
			return 3
		}
		res, err = RemoveTrimPathsFromClaim(*root, *listFile, *listDigest, w)
	} else {
		if *listDigest != "" {
			fmt.Fprintln(stderr, "trim-paths: --list-sha256-file names a claim-carried list; give --list-file")
			return 3
		}
		shards, aerr := strconv.Atoi(*shardsText)
		if aerr != nil || shards <= 0 {
			fmt.Fprintln(stderr, "trim-paths: --shards needs a whole number of shards, at least 1")
			return 3
		}
		res, err = RemoveTrimPaths(*root, *list, shards, w)
	}
	fmt.Fprintf(stdout, "removed %d absent %d\n", res.Removed, res.Absent)
	if err != nil {
		fmt.Fprintf(stderr, "trim-paths: %v\n", err)
		return 3
	}
	return 0
}

// trimListWriteMain runs the writer: it plans PlanTrim's removal list from the claim at --root, under
// the cap and sandbox count its Job passes, and writes the list and its digest into the claim where
// the Job's trim container reads them. It is the claim-carried trim Job's init container.
//
// The list is planned here, in the pod that mounts the claim, because a list past one set of shards
// has no other channel that reaches the claim whole and nothing outside the cluster mounts it. Exit 3
// with a reason on stderr means nothing was written: the list is refused whole or written whole, so
// the trim container never reads a list the writer did not vouch for.
func trimListWriteMain(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("trim-list-write", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", cachevol.SeedCacheRoot, "the shared cache claim the trim plans and writes its list in")
	capText := flags.String("cap-bytes", "", "the size to bring the claim under, in bytes")
	sandboxesText := flags.String("sandboxes", "", "the count of running sandboxes, whose module cache the plan leaves alone")
	if err := flags.Parse(args); err != nil {
		return 3
	}
	cap, err := strconv.ParseInt(*capText, 10, 64)
	if err != nil || cap < 0 {
		fmt.Fprintln(stderr, "trim-list-write: --cap-bytes needs a whole number of bytes")
		return 3
	}
	sandboxes, err := strconv.Atoi(*sandboxesText)
	if err != nil || sandboxes < 0 {
		fmt.Fprintln(stderr, "trim-list-write: --sandboxes needs a whole number of running sandboxes, 0 when none runs")
		return 3
	}
	paths, err := TrimListPaths(*root, cap, sandboxes > 0, filepath.Walk)
	var digest string
	if err == nil {
		digest, err = writeTrimListInto(*root, paths)
	}
	if err != nil {
		fmt.Fprintf(stderr, "trim-list-write: %v\n", err)
		return 3
	}
	fmt.Fprintf(stdout, "wrote %d paths %s %s\n", len(paths), cachevol.TrimClaimListPath, digest)
	return 0
}

// trimListMain runs the reader: it prints the claim's listing under --root as JSON, the input a
// caller builds a trim manifest from without mounting the claim itself. The disk_maintenance
// patrol's trim is the caller, reading it through a kubectl exec into a pod that mounts the claim
// (gt-rm4am).
//
// Exit 3 with a reason on stderr means the listing could not be read whole: a partial listing would
// plan a trim of a claim it never saw.
func trimListMain(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("trim-list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", cachevol.SeedCacheRoot, "the claim whose cache files the listing names")
	if err := flags.Parse(args); err != nil {
		return 3
	}
	listing, err := trimListing(*root, filepath.Walk)
	if err != nil {
		fmt.Fprintf(stderr, "trim-list: %v\n", err)
		return 3
	}
	if listing == nil {
		// The encoder prints a nil slice as null; an empty claim is a list of no entries.
		listing = []trimListEntry{}
	}
	if err := json.NewEncoder(stdout).Encode(listing); err != nil {
		fmt.Fprintf(stderr, "trim-list: %v\n", err)
		return 3
	}
	return 0
}

// seedCloneMain runs the seed Job's init container: it checks each pin out under
// --dest from the pin's clone URL, unless the claim at --root already holds
// these pins. Exit 3 with a reason on stderr means a pin could not be cloned or
// the marker could not be read, and the Job stops before the seed container
// touches the claim.
func seedCloneMain(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("seed-clone", flag.ContinueOnError)
	flags.SetOutput(stderr)
	pinsText := flags.String("pins", "", "the seed pins, as the seed Job renders them")
	dest := flags.String("dest", defaultSeedSource, "the directory each pinned rig is checked out under")
	root := flags.String("root", defaultSeedRoot, "the shared cache claim, whose marker says whether the pins are already checked out")
	tokenFile := flags.String("token-file", "", "the file holding the read-only Forgejo token the clone authenticates with; empty clones anonymously")
	if err := flags.Parse(args); err != nil {
		return 3
	}
	pins, err := decodePins(*pinsText)
	token := ""
	if err == nil && *tokenFile != "" {
		// Read at run time: the file is the Secret the Job mounts, and the token never reaches an
		// argument, the manifest or a log.
		token, err = readCloneToken(*tokenFile)
	}
	if err == nil {
		err = CloneSeed(*dest, *root, pins, token, OSFiles{}, execSeedRun, outLogf(stdout))
	}
	if err != nil {
		fmt.Fprintf(stderr, "seed-clone: %v\n", err)
		return 3
	}
	return 0
}

// seedCacheMain runs the seed Job's seed container: it fills the claim at --root
// from the checkouts under --source. Exit 3 with a reason on stderr means the
// claim was not seeded to completion, so the Job reports itself and leaves no
// marker for a rerun to mistake for a finished seed.
func seedCacheMain(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("seed-cache", flag.ContinueOnError)
	flags.SetOutput(stderr)
	pinsText := flags.String("pins", "", "the seed pins, as the seed Job renders them")
	root := flags.String("root", defaultSeedRoot, "the shared cache claim to fill")
	source := flags.String("source", defaultSeedSource, "the directory holding each pinned rig's checkout")
	if err := flags.Parse(args); err != nil {
		return 3
	}
	pins, err := decodePins(*pinsText)
	if err == nil {
		pass := &SeedPass{Root: *root, Source: *source, Pins: pins, Files: OSFiles{}, Run: execSeedRun, Logf: outLogf(stdout)}
		err = pass.Seed()
	}
	if err != nil {
		fmt.Fprintf(stderr, "seed-cache: %v\n", err)
		return 3
	}
	return 0
}

// outLogf writes a seed helper's progress to stdout, where the Job's log holds it.
func outLogf(w io.Writer) func(string, ...any) {
	return func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }
}
