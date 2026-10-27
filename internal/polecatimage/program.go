package polecatimage

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Program runs one plugin run and then the trims that follow it: the old image
// tags, the build cache and the per-rig cache volumes.
//
// The trims follow a run that finished, a deferral included, and never follow a
// failure, whose answer Program returns (gt-tpg0p.7).
func (r *Run) Program() error {
	if err := r.Main(); err != nil {
		return err
	}
	r.PruneOldImages()
	r.PruneBuildCache()
	r.TrimCacheVolumes()
	return nil
}

// ConfigFromEnv reads a run's configuration through get, the environment the
// script read, and reports the one variable it cannot carry on without: an
// unreadable POLECAT_IMAGE_STALE_MINUTES, which drives the lock takeover in the
// run path.
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
	return Config{
		GT:             envOr(get, "POLECAT_IMAGE_GT", "gt"),
		Docker:         envOr(get, "POLECAT_IMAGE_DOCKER", "docker"),
		Git:            envOr(get, "POLECAT_IMAGE_GIT", "git"),
		Make:           envOr(get, "POLECAT_IMAGE_MAKE", "make"),
		Pgrep:          envOr(get, "POLECAT_IMAGE_PGREP", "pgrep"),
		Openshell:      envOr(get, "POLECAT_IMAGE_OPENSHELL", "openshell"),
		Crane:          envOr(get, "POLECAT_IMAGE_CRANE", "crane"),
		RigRepo:        envOr(get, "POLECAT_IMAGE_RIG_REPO", filepath.Join(town, "gastown", ".repo.git")),
		BuildRoot:      envOr(get, "POLECAT_IMAGE_BUILD_ROOT", filepath.Join(town, "gastown", ".polecat-image-build")),
		BDSrc:          envOr(get, "POLECAT_IMAGE_BD_SRC", filepath.Join(town, "beads", "mayor", "rig")),
		LockDir:        envOr(get, "POLECAT_IMAGE_LOCK", filepath.Join(town, ".runtime", "polecat-image.lock")),
		StaleAfter:     time.Duration(minutes) * time.Minute,
		SandboxRecords: envOr(get, "POLECAT_IMAGE_SANDBOX_RECORDS", filepath.Join(town, ".runtime", "sandboxes")),
		Image:          envOr(get, "POLECAT_IMAGE_NAME", "gastown-polecat:dev"),
		Label:          "org.gastown.gt-commit",
		Registry:       get("POLECAT_IMAGE_REGISTRY"),
		Published:      envOr(get, "POLECAT_IMAGE_PUBLISHED", filepath.Join(town, ".runtime", "polecat-image.published")),
		// Only 1 turns it on, so a stray POLECAT_IMAGE_FAIL_CLOSED=0 cannot fail
		// a run (gt-efupr).
		FailClosed:  get("POLECAT_IMAGE_FAIL_CLOSED") == "1",
		Keep:        envOr(get, "POLECAT_IMAGE_KEEP", "3"),
		BuilderKeep: envOr(get, "POLECAT_IMAGE_BUILDER_KEEP", "8GB"),
		CacheCap:    envOr(get, "POLECAT_CACHE_CAP", "10GB"),
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

// runProgram answers Main: no arguments runs one plugin run, and "trim-cache"
// is the helper a cache volume's short-lived container runs.
func runProgram(args []string, stdout, stderr io.Writer, get func(string) string) int {
	if len(args) > 0 && args[0] == "trim-cache" {
		return trimCacheMain(args[1:], stdout, stderr)
	}
	return runMain(stdout, get)
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
