package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
	"github.com/steveyegge/gastown/internal/polecatimage"
	"github.com/steveyegge/gastown/internal/workspace"
)

const (
	maintModePlan  = "plan"
	maintModeApply = "apply"

	// maintTargetRegistry is the name the registry target answers to on the
	// command line and in the target list.
	maintTargetRegistry = "registry"

	// maintKeepDefault is the newest-commit window a town that names none gets;
	// it matches the plugin's default, so gt maint plans what the plugin's
	// prune would delete.
	maintKeepDefault = 5

	// maintLedgerMaxBytes bounds the ledger before it rotates: a run appends one
	// short line, so a mebibyte holds thousands of runs and still cannot fill
	// the disk it protects.
	maintLedgerMaxBytes = 1 << 20

	// maintRegistryContainer is the registry container the garbage collect
	// execs into, the name deploy/k8s/kind/up.sh creates.
	maintRegistryContainer = "gt-registry"
)

// gt maint runs the disk-maintenance targets in this process. A refusal and a
// held lock are states of the town rather than bad arguments, so both leaves
// silence the usage block: it would print after the reason and bury it.
var maintCmd = &cobra.Command{
	Use:     "maint",
	GroupID: GroupConfig,
	Short:   "Plan or apply disk maintenance",
	Long: `Plan or apply the maintenance the town's disks need.

plan is a dry run: it prints every action it would take and changes nothing.
apply runs the plan it just printed. Every apply runs under the polecat-image
lock, so a delete never races an image push; an apply that finds the lock held
defers and says to retry.

A target names what to maintain; the known targets are listed when a name is
unknown, and naming none runs every target in name order. A plan that could not
read its inputs refuses instead of guessing, and both leaves exit non-zero then.`,
	RunE: requireSubcommand,
}

var maintPlanCmd = &cobra.Command{
	Use:          "plan [target]",
	Short:        "Print the maintenance plan without changing anything (dry run)",
	Args:         cobra.MaximumNArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return maintRun(cmd, maintModePlan, args)
	},
}

var maintApplyCmd = &cobra.Command{
	Use:          "apply [target]",
	Short:        "Apply the maintenance plan",
	Args:         cobra.MaximumNArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return maintRun(cmd, maintModeApply, args)
	},
}

func init() {
	maintCmd.AddCommand(maintPlanCmd)
	maintCmd.AddCommand(maintApplyCmd)
	rootCmd.AddCommand(maintCmd)
}

// maintDeps are runMaint's seams: a test supplies fake targets, a clock and a
// keep window, so no unit test reaches the registry, kubectl or docker.
type maintDeps struct {
	Targets func() (map[string]diskmaint.Target, error)
	Now     func() time.Time
	Keep    int
}

// runMaint plans each chosen target and, in apply mode, applies the plan it
// just printed. A target that cannot plan or apply does not stop the others,
// and every failure is returned joined, so one broken target cannot make a
// maintenance run read as clean.
func runMaint(out io.Writer, deps maintDeps, mode, target string) error {
	if mode != maintModePlan && mode != maintModeApply {
		return fmt.Errorf("unknown mode %q: use %s or %s", mode, maintModePlan, maintModeApply)
	}
	if deps.Keep < 1 {
		// A keep of zero would delete every commit but the installed one: a
		// window that tight is a mistyped setting, not an instruction.
		return fmt.Errorf("keep must be at least 1, got %d", deps.Keep)
	}
	if deps.Targets == nil {
		return errors.New("no maintenance targets are configured")
	}
	targets, err := deps.Targets()
	if err != nil {
		return fmt.Errorf("building the maintenance targets: %w", err)
	}
	names, err := maintChosenNames(targets, target)
	if err != nil {
		return err
	}

	now := time.Now
	if deps.Now != nil {
		now = deps.Now
	}
	ctx := context.Background()
	var errs []error
	for _, name := range names {
		t := targets[name]
		plan, err := t.Plan(ctx, diskmaint.Policy{Keep: deps.Keep, Apply: mode == maintModeApply, Now: now()})
		if err != nil {
			fmt.Fprintf(out, "%s: the plan could not be built: %v\n", name, err)
			errs = append(errs, fmt.Errorf("%s: plan: %w", name, err))
			continue
		}
		maintPrintPlan(out, name, plan)
		if plan.Refused != "" {
			// A refused plan says the run could not check its inputs, so it is
			// an alarm in both modes: applying nothing by accident and applying
			// nothing on purpose must not read alike.
			fmt.Fprintf(out, "%s: refused: %s\n", name, plan.Refused)
			errs = append(errs, fmt.Errorf("%s: plan refused: %s", name, plan.Refused))
			continue
		}
		if mode == maintModePlan {
			continue
		}
		result, applyErr := t.Apply(ctx, plan)
		maintPrintResult(out, result)
		var runErr error
		if result.Deferred != "" {
			fmt.Fprintf(out, "%s: deferred: %s\n", name, result.Deferred)
			runErr = fmt.Errorf("%s: apply deferred, retry later: %s", name, result.Deferred)
		}
		if applyErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("%s: apply: %w", name, applyErr))
		}
		if runErr != nil {
			errs = append(errs, runErr)
		}
	}
	return errors.Join(errs...)
}

// maintChosenNames returns the target names to run: the named one, or every
// target sorted, so two runs of the same town print the same order.
func maintChosenNames(targets map[string]diskmaint.Target, only string) ([]string, error) {
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Strings(names)
	if only != "" {
		if _, ok := targets[only]; !ok {
			if len(names) == 0 {
				return nil, fmt.Errorf("unknown target %q: no maintenance targets are configured", only)
			}
			return nil, fmt.Errorf("unknown target %q; known targets: %s", only, strings.Join(names, ", "))
		}
		return []string{only}, nil
	}
	if len(names) == 0 {
		return nil, errors.New("no maintenance targets are configured")
	}
	return names, nil
}

// maintPrintPlan prints the plan a dry run would stop at: every action it would
// take and every ref it keeps, with the rule that kept it.
func maintPrintPlan(out io.Writer, name string, plan diskmaint.Plan) {
	fmt.Fprintf(out, "%s: %d action(s), %d kept\n", name, len(plan.Actions), len(plan.Kept))
	for _, action := range plan.Actions {
		fmt.Fprintf(out, "  would %s %s (%d bytes): %s\n", action.Kind, action.Ref, action.Bytes, action.Why)
	}
	for _, kept := range plan.Kept {
		fmt.Fprintf(out, "  kept %s: %s\n", kept.Ref, kept.Rule)
	}
}

// maintPrintResult prints what an apply did, including a garbage collect that
// did not run: a skipped collect looks like a quiet one otherwise.
func maintPrintResult(out io.Writer, result diskmaint.Result) {
	for _, action := range result.Done {
		fmt.Fprintf(out, "  did %s %s\n", action.Kind, action.Ref)
	}
	for _, failure := range result.Failed {
		fmt.Fprintf(out, "  failed %s %s: %s\n", failure.Action.Kind, failure.Action.Ref, failure.Err)
	}
	if len(result.Done) > 0 || len(result.Failed) > 0 {
		fmt.Fprintf(out, "  freed %d bytes\n", result.BytesFreed)
	}
	if len(result.Done) > 0 && !result.Collected {
		fmt.Fprintf(out, "  garbage collect did not run: the freed space is reclaimed by the next apply\n")
	}
}

// maintRun is the command's entry point.
func maintRun(cmd *cobra.Command, mode string, args []string) error {
	return maintRunIn(cmd, workspace.FindFromCwdOrError, os.Getenv, mode, args)
}

// maintRunIn is maintRun with the town lookup and the environment injected, so
// a test drives both refusals without depending on where the suite runs.
func maintRunIn(cmd *cobra.Command, findTown func() (string, error), get func(string) string, mode string, args []string) error {
	town, err := findTown()
	if err != nil {
		return err
	}
	deps, err := maintProductionDeps(cmd.Context(), cmd.OutOrStdout(), town, get)
	if err != nil {
		return err
	}
	target := ""
	if len(args) == 1 {
		target = args[0]
	}
	return runMaint(cmd.OutOrStdout(), deps, mode, target)
}

// maintProductionDeps builds the deps a real run uses: the targets come from
// the town's own settings, through the same reader the polecat-image plugin
// uses, so gt maint and the plugin agree on the registry and the lock.
func maintProductionDeps(ctx context.Context, out io.Writer, town string, get func(string) string) (maintDeps, error) {
	cfg, err := polecatimage.ConfigFromEnv(func(key string) string {
		if key == "POLECAT_IMAGE_TOWN_ROOT" {
			return town
		}
		return get(key)
	})
	if err != nil {
		return maintDeps{}, err
	}
	keep, err := maintKeep(get)
	if err != nil {
		return maintDeps{}, err
	}
	targets, err := maintTargets(ctx, town, cfg, keep, maintExecRun, get("HOME"), out)
	if err != nil {
		return maintDeps{}, err
	}
	return maintDeps{
		Targets: func() (map[string]diskmaint.Target, error) { return targets, nil },
		Now:     time.Now,
		Keep:    keep,
	}, nil
}

// maintKeep reads the keep window the registry prune uses; an unreadable value
// is an error rather than the default, because a plan built on a different
// window would delete what the plugin keeps.
func maintKeep(get func(string) string) (int, error) {
	value := get("POLECAT_IMAGE_REGISTRY_KEEP")
	if value == "" {
		return maintKeepDefault, nil
	}
	keep, err := strconv.Atoi(value)
	if err != nil || keep < 1 {
		return 0, fmt.Errorf("POLECAT_IMAGE_REGISTRY_KEEP=%s is not a whole number of images to keep", value)
	}
	return keep, nil
}

// maintTargets builds every target the town configures. A town that names no
// registry publishes nothing, so it has no registry to prune and gets no
// target; the first target that cannot be built fails the run rather than
// leaving a half-built set.
func maintTargets(ctx context.Context, town string, cfg polecatimage.Config, keep int, run diskmaint.Runner, home string, out io.Writer) (map[string]diskmaint.Target, error) {
	targets := map[string]diskmaint.Target{}
	if cfg.Registry == "" {
		return targets, nil
	}
	if strings.Contains(cfg.Registry, "/") {
		// The value once named the sandbox records directory, so a path here is
		// that old setting, and a URL is a scheme this reader adds itself.
		return nil, errors.New("POLECAT_IMAGE_REGISTRY is not a host:port; the registry address is host:port, and the sandbox records are POLECAT_IMAGE_SANDBOX_RECORDS")
	}
	if home == "" {
		return nil, errors.New("cannot locate the colima docker socket: HOME is unset")
	}
	ledgerDir := filepath.Join(town, ".runtime", "diskmaint")
	if err := os.MkdirAll(ledgerDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating the maintenance ledger directory: %w", err)
	}
	gate := &maintPushGate{cfg: cfg, out: out, now: time.Now}
	targets[maintTargetRegistry] = registry.NewTarget(registry.Config{
		Base:         "http://" + cfg.Registry,
		Kubeconfig:   filepath.Join(town, ".runtime", "sbx-k8s", "kubeconfig"),
		RecordsDir:   cfg.SandboxRecords,
		DockerHost:   "unix://" + filepath.Join(home, ".colima", "gt-sbx", "docker.sock"),
		Container:    maintRegistryContainer,
		Keep:         keep,
		Installed:    maintInstalled(ctx, run, cfg.GT),
		PushInFlight: gate.pushInFlight,
		Lock:         gate.withLock,
		Run:          run,
		Ledger:       diskmaint.NewLedger(filepath.Join(ledgerDir, "ledger.jsonl"), maintLedgerMaxBytes),
	})
	return targets, nil
}

// maintInstalled reads the installed gt's commit by running `gt version`, the
// one source the plan names for it. A read that fails is an error, so a plan
// refuses rather than keeping tags for a commit it guessed.
func maintInstalled(ctx context.Context, run diskmaint.Runner, gt string) func() (string, error) {
	return func() (string, error) {
		out, err := run(ctx, nil, gt, "version")
		if err != nil {
			return "", fmt.Errorf("%s version: %w", gt, err)
		}
		commit := maintCommitFromGTVersion(string(out))
		if commit == "" {
			return "", fmt.Errorf("%s version names no commit", gt)
		}
		return commit, nil
	}
}

// maintCommitFromGTVersion reads the commit from `gt version`'s last
// parenthesised group, which is "(<build>: <commit>)" or "(<build>:
// <branch>@<commit>)". Only a lowercase hex string at least as long as git's
// shortest abbreviation counts, so nothing else in the line is read as one.
func maintCommitFromGTVersion(out string) string {
	line, _, _ := strings.Cut(out, "\n")
	open := strings.LastIndex(line, "(")
	if open < 0 {
		return ""
	}
	group := line[open+1:]
	if end := strings.Index(group, ")"); end >= 0 {
		group = group[:end]
	}
	_, field, ok := strings.Cut(group, ":")
	if !ok {
		return ""
	}
	field = strings.TrimSpace(field)
	if at := strings.LastIndex(field, "@"); at >= 0 {
		field = field[at+1:]
	}
	field = strings.ReplaceAll(field, " ", "")
	if !maintIsCommit(field) {
		return ""
	}
	return field
}

// maintIsCommit reports a commit-shaped string: lowercase hexadecimal at least
// as long as git's shortest abbreviation, so it can reach no program as an
// option or a revision expression.
func maintIsCommit(s string) bool {
	if len(s) < 7 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// maintExecRun runs one program for the registry target.
func maintExecRun(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: name is a fixed program and the arguments are the target's validated refs
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, maintExecFailure(name, err, stderr.String())
	}
	return out, nil
}

// maintExecFailure carries the last line of a failed program's stderr, so a
// failed kubectl or docker names its own reason in the ledger without the
// whole output.
func maintExecFailure(name string, err error, stderr string) error {
	line := strings.TrimSpace(stderr)
	if i := strings.LastIndexByte(line, '\n'); i >= 0 {
		line = line[i+1:]
	}
	return fmt.Errorf("%s: %w: %s", name, err, line)
}

// maintPushGate answers the registry target's two questions about the plugin
// lock. A push holds that lock, so possessing it proves no push is writing and
// the garbage collect may run; without it the answer is "a push may be" and
// the collect is skipped rather than guessed. The flag is atomic because the
// apply's delete loop reads it while the lock's callback writes it.
type maintPushGate struct {
	cfg  polecatimage.Config
	out  io.Writer
	now  func() time.Time
	held atomic.Bool
}

// pushInFlight reports whether a push may hold the registry; only the holder of
// the lock can answer that no push does.
func (g *maintPushGate) pushInFlight() bool { return !g.held.Load() }

// withLock runs fn under the plugin lock, reporting a held lock as the
// registry's deferral so the apply leaves the registry alone and says why.
func (g *maintPushGate) withLock(ctx context.Context, fn func(context.Context) error) error {
	err := polecatimage.WithLock(ctx, g.cfg, polecatimage.WithLockOptions{Out: g.out, Now: g.now}, func(ctx context.Context) error {
		g.held.Store(true)
		return fn(ctx)
	})
	if errors.Is(err, polecatimage.ErrLockHeld) {
		return fmt.Errorf("%w: %w", registry.ErrDeferred, err)
	}
	return err
}
