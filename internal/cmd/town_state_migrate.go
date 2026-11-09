package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/townstate"
	"github.com/steveyegge/gastown/internal/workspace"
)

// daemonImageTag is the gt-daemon:<rev> tag the rendered compose file names.
var daemonImageTag = regexp.MustCompile(`gt-daemon:[0-9a-f]{7,64}`)

// migrateVolumeLabel is the label a volume the copy creates carries.
const migrateVolumeLabel = "gt/managed-by=town-state-migrate"

// migrateProbeTimeout bounds one read the migrate command makes of docker or bd,
// so a hung probe fails its check as unknown rather than holding the command.
const migrateProbeTimeout = 30 * time.Second

// migrateHelperTimeout bounds one run of the helper container: a copy, a listing,
// a df or the bd read. A run that has not finished by then is stopped and named,
// so a hung helper cannot hold the copy or the verify.
const migrateHelperTimeout = 10 * time.Minute

// volumeNamePattern is the shape a docker volume name takes. A name outside it
// is refused before it reaches a docker argv: a path such as /host/dir would
// be a bind mount, not a volume.
var volumeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// safeShellWord is the shape a word may take unquoted in a printed command.
var safeShellWord = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`)

// stoppedContainerStates are the docker states that mean the container runs no
// daemon. Every other state (running, restarting, paused) counts as running.
var stoppedContainerStates = map[string]bool{"created": true, "exited": true, "dead": true}

// The migrate subcommands sit under `gt town state`, beside export and import.
var townStateMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Plan, copy, verify and roll back the daemon-owned set on a town-state volume",
	Long: `Move the daemon-owned set into a named Docker volume, with a plan, a checksum
verify and a printed rollback. The copy is the export streamed into a helper
container that runs the import against the volume; the helper is the daemon
image by digest, from the image values file (--images) when present, else the
gt-daemon tag the rendered compose file names.

  plan           what would be copied, the volume's free space, and the cut-over
                 prerequisites (dispatch hold, daemon container stopped, no landing
                 or post-land run in flight), each met or unmet by name
  copy           stream the set into an empty volume, and write a seed record. A
                 populated volume that the seed record names gets only the files
                 that differ. --cutover also refuses unless every cut-over
                 prerequisite is met, and writes the cut-over time when the copy
                 verifies
  verify         sha256 of every copied file and the target of every link on both
                 sides; a missing, changed, extra or non-regular entry is reported
                 by path and fails the command
  rollback-plan  print the steps back: stop the container, export the rollback rows
                 from the volume, import them with --merge-newer, restore the compose

The secret files named in the secrets manifest are left out of the copy and of the
verify listing. No file contents are printed. How bd reaches Dolt from the
container is in docs/design/gt-in-container.md; the copy prints the endpoint its
bd read used.`,
	RunE: requireSubcommand,
}

var townStateMigratePlanCmd = &cobra.Command{
	Use:   "plan",
	Short: "Print what a copy would move and the cut-over prerequisites",
	Args:  cobra.NoArgs,
	RunE:  runTownStateMigratePlan,
}

var townStateMigrateCopyCmd = &cobra.Command{
	Use:   "copy",
	Short: "Stream the daemon-owned set into an empty volume, or a delta onto a seeded one",
	Args:  cobra.NoArgs,
	RunE:  runTownStateMigrateCopy,
}

var townStateMigrateVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Compare every copied file's sha256 and link target on the host and in the volume",
	Args:  cobra.NoArgs,
	RunE:  runTownStateMigrateVerify,
}

var townStateMigrateRollbackPlanCmd = &cobra.Command{
	Use:   "rollback-plan",
	Short: "Print the steps that put the daemon-owned set back on the host",
	Args:  cobra.NoArgs,
	RunE:  runTownStateMigrateRollbackPlan,
}

func init() {
	townStateMigrateCmd.AddCommand(townStateMigratePlanCmd)
	townStateMigrateCmd.AddCommand(townStateMigrateCopyCmd)
	townStateMigrateCmd.AddCommand(townStateMigrateVerifyCmd)
	townStateMigrateCmd.AddCommand(townStateMigrateRollbackPlanCmd)
	townStateCmd.AddCommand(townStateMigrateCmd)

	for _, c := range []*cobra.Command{townStateMigratePlanCmd, townStateMigrateCopyCmd, townStateMigrateVerifyCmd, townStateMigrateRollbackPlanCmd} {
		c.Flags().String("town", "", "the town root (default: the town containing the working directory)")
		c.Flags().String("images", "", "the image values file (default "+imagevalues.DefaultRelPath+" under the town root); a missing file falls back to the compose file's daemon tag")
		c.Flags().String("compose-file", "", "the rendered compose file (default: <town>/daemon/container/compose.yml)")
	}
	townStateMigratePlanCmd.Flags().String("volume", "", "the volume whose free space the plan reads (optional)")
	townStateMigrateCopyCmd.Flags().String("volume", "", "the named volume to copy into; it is created when absent (required)")
	townStateMigrateCopyCmd.Flags().String("since", "", "keep only regular files modified after this RFC 3339 time (optional)")
	townStateMigrateCopyCmd.Flags().Bool("daemon-removed", false, "with --cutover: the daemon container was removed (compose down), so a container docker cannot find counts as stopped; without it an absent container is an unmet check, because a misnamed container reads the same")
	townStateMigrateCopyCmd.Flags().Bool("cutover", false, "the cut-over copy: refuse unless every cut-over prerequisite is met, and write the cut-over time when the copy verifies")
	townStateMigrateCopyCmd.Flags().String("secrets-manifest", "", "deploy/daemon/secrets.yaml: its file names are left out of the copy (required)")
	townStateMigrateVerifyCmd.Flags().String("volume", "", "the named volume to verify (required)")
	townStateMigrateVerifyCmd.Flags().String("since", "", "the --since the copy ran with, so the listing compares the same set (optional)")
	townStateMigrateVerifyCmd.Flags().String("secrets-manifest", "", "deploy/daemon/secrets.yaml: its file names are left out of the listing (required)")
	townStateMigrateRollbackPlanCmd.Flags().String("volume", "", "the volume the rollback reads from (required)")
	townStateMigrateRollbackPlanCmd.Flags().String("saved-compose", "", "the compose file saved before the cut-over (default: <compose-file>.pre-cutover)")
	_ = townStateMigrateCopyCmd.MarkFlagRequired("volume")
	_ = townStateMigrateVerifyCmd.MarkFlagRequired("volume")
	_ = townStateMigrateRollbackPlanCmd.MarkFlagRequired("volume")
	_ = townStateMigrateCopyCmd.MarkFlagRequired("secrets-manifest")
	_ = townStateMigrateVerifyCmd.MarkFlagRequired("secrets-manifest")
}

// townStateMigrateEnv is one migrate command's session: the town, the volume,
// the helper image, the engine, the probes and the clock. Production fills it
// from flags, the docker engine and the daemon's own probes; tests fill it with
// fakes.
type townStateMigrateEnv struct {
	town         string
	volume       string
	image        string
	composePath  string
	savedCompose string
	since        time.Time
	cutover      bool
	exclude      func(string) bool
	allowNoRigs  bool
	// deltaSent names the paths a delta copy sends. A changed path among them
	// after the import is one the merge kept on the volume, so verify names it
	// with the way out; nil for a seed or a full copy.
	deltaSent map[string]bool
	// daemonRemoved is the operator's word that the daemon container was removed
	// (compose down), not merely stopped: a container docker cannot find reads the
	// same whether it was removed or the name is wrong, so only this flag lets an
	// absent container count as stopped.
	daemonRemoved bool
	// beadsHost is the Dolt host the helper's bd reads, from the operator's
	// shell or the town's dolt.container_host; beadsHostSource names which. An
	// empty beadsHost means the bd read is not forwarded.
	beadsHost       string
	beadsHostSource string
	engine          townstate.Engine
	probes          migrateProbes
	now             func() time.Time
	out             io.Writer
	summary         io.Writer
}

// migrateProbes reads the cut-over prerequisites. Each one that cannot answer
// returns an error, and the caller counts that prerequisite as unmet.
type migrateProbes struct {
	// holdPresent reports whether the operator's dispatch hold file is set.
	holdPresent func(town string) (bool, error)
	// daemonContainer names the town's daemon container, from its settings.
	daemonContainer func(town string) (string, error)
	// containerRunning reports whether the named container is running.
	containerRunning func(name string) (bool, error)
	// landingQueue counts the ready-to-land beads the landing worker still acts
	// on, in every actionable status, summed over the town's rigs.
	landingQueue func(town string) (int, error)
	// landingCandidates names the rigs whose landing work root holds a landing,
	// post-landing or revert run.
	landingCandidates func(town string) ([]string, error)
	// postLandPending names the rigs whose red-main state holds post-landing
	// verdicts still waiting on CI, with the commit the newest one is for.
	postLandPending func(town string) ([]string, error)
}

// migrateCheck is one cut-over prerequisite as the plan reports it.
type migrateCheck struct {
	Met  bool
	Text string
}

// exportOptions is the export the copy, the verify and the plan run with.
func (env townStateMigrateEnv) exportOptions() townstate.ExportOptions {
	return townstate.ExportOptions{Since: env.since, AllowNoRigs: env.allowNoRigs, Exclude: env.exclude}
}

// cutoverChecks reads the cut-over prerequisites through their probes, each by
// name. A probe that cannot answer is unmet, never met.
func cutoverChecks(env townStateMigrateEnv) []migrateCheck {
	var checks []migrateCheck
	present, err := env.probes.holdPresent(env.town)
	switch {
	case err != nil:
		checks = append(checks, migrateCheck{Text: fmt.Sprintf("dispatch hold unreadable: %v", err)})
	case present:
		checks = append(checks, migrateCheck{Met: true, Text: "dispatch hold present"})
	default:
		checks = append(checks, migrateCheck{Text: "dispatch hold missing"})
	}

	name, err := env.probes.daemonContainer(env.town)
	if err != nil {
		checks = append(checks, migrateCheck{Text: fmt.Sprintf("daemon container state unreadable: %v", err)})
	} else if running, err := env.probes.containerRunning(name); errors.Is(err, errDaemonContainerAbsent) {
		if env.daemonRemoved {
			checks = append(checks, migrateCheck{Met: true, Text: fmt.Sprintf("daemon container absent (%s), confirmed removed by --daemon-removed", name)})
		} else {
			checks = append(checks, migrateCheck{Text: fmt.Sprintf("daemon container %q not found: a removed container and a misnamed one read the same; if the daemon was removed (compose down), re-run with --daemon-removed", name)})
		}
	} else if err != nil {
		checks = append(checks, migrateCheck{Text: fmt.Sprintf("daemon container state unreadable: %v", err)})
	} else if running {
		checks = append(checks, migrateCheck{Text: fmt.Sprintf("daemon container running (%s)", name)})
	} else {
		checks = append(checks, migrateCheck{Met: true, Text: "daemon container stopped"})
	}

	n, err := env.probes.landingQueue(env.town)
	switch {
	case err != nil:
		checks = append(checks, migrateCheck{Text: fmt.Sprintf("landing queue unreadable, so a landing run in flight is not ruled out: %v", err)})
	case n > 0:
		checks = append(checks, migrateCheck{Text: fmt.Sprintf("landing queue: run in flight (%d ready-to-land bead(s) waiting)", n)})
	default:
		checks = append(checks, migrateCheck{Met: true, Text: "landing queue: no ready-to-land bead"})
	}

	rigs, err := env.probes.landingCandidates(env.town)
	switch {
	case err != nil:
		checks = append(checks, migrateCheck{Text: fmt.Sprintf("landing candidate unreadable, so a run in flight is not ruled out: %v", err)})
	case len(rigs) > 0:
		checks = append(checks, migrateCheck{Text: fmt.Sprintf("landing candidate: run in flight on %s", strings.Join(rigs, ", "))})
	default:
		checks = append(checks, migrateCheck{Met: true, Text: "landing candidate: no landing, post-landing or revert run in flight"})
	}

	pending, err := env.probes.postLandPending(env.town)
	switch {
	case err != nil:
		checks = append(checks, migrateCheck{Text: fmt.Sprintf("post-land status unreadable, so a run in flight is not ruled out: %v", err)})
	case len(pending) > 0:
		checks = append(checks, migrateCheck{Text: fmt.Sprintf("post-land status: run in flight, verdict pending on %s", strings.Join(pending, ", "))})
	default:
		checks = append(checks, migrateCheck{Met: true, Text: "post-land status: no verdict pending"})
	}
	return checks
}

// unmetChecks returns the text of each check that is not met, in order.
func unmetChecks(checks []migrateCheck) []string {
	var out []string
	for _, c := range checks {
		if !c.Met {
			out = append(out, c.Text)
		}
	}
	return out
}

func runTownStateMigratePlan(cmd *cobra.Command, _ []string) error {
	env, err := migrateEnvFromFlags(cmd, false, workspace.FindFromCwdOrError, os.Getenv("BEADS_DOLT_SERVER_HOST"))
	if err != nil {
		return err
	}
	return runTownStateMigratePlanIn(env)
}

// runTownStateMigratePlanIn is what `gt town state migrate plan` runs.
func runTownStateMigratePlanIn(env townStateMigrateEnv) error {
	inv, err := townstate.Survey(env.town, townstate.Default(), env.exportOptions())
	if err != nil {
		return fmt.Errorf("surveying the town state: %w", err)
	}
	fmt.Fprintf(env.out, "migrate plan: %d entries: %s\n", len(inv.Entries), strings.Join(inv.Entries, ", "))
	if env.image != "" {
		fmt.Fprintf(env.out, "helper image: %s\n", env.image)
	}
	fmt.Fprintf(env.out, "copy: %d files, %d directories, %d symlinks, %d bytes (metadata only; the symlink count is an estimate where the export would refuse a link)\n",
		inv.Files, inv.Dirs, inv.Symlinks, inv.Bytes)
	fmt.Fprintf(env.out, "skipped: %s\n", skippedLine(inv.Skipped))
	if env.volume == "" {
		fmt.Fprintln(env.out, "free space: not measured (no --volume)")
	} else {
		free, err := townstate.FreeBytes(env.engine, env.volume)
		if err != nil {
			return err
		}
		fmt.Fprintf(env.out, "free space on volume %s: %d bytes\n", env.volume, free)
		fmt.Fprintf(env.out, "compose: the compose write takes the same volume name: gt daemon compose write --town-state-volume %s\n", shellWord(env.volume))
	}
	fmt.Fprintln(env.out, "cut-over prerequisites:")
	for _, c := range cutoverChecks(env) {
		state := "unmet"
		if c.Met {
			state = "met"
		}
		fmt.Fprintf(env.out, "  %s: %s\n", state, c.Text)
	}
	return nil
}

// skippedLine renders the skip counts in the order the export prints them.
func skippedLine(skipped map[townstate.SkipClass]int) string {
	var parts []string
	for class, n := range skipped {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, class))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func runTownStateMigrateCopy(cmd *cobra.Command, _ []string) error {
	env, err := migrateEnvFromFlags(cmd, true, workspace.FindFromCwdOrError, os.Getenv("BEADS_DOLT_SERVER_HOST"))
	if err != nil {
		return err
	}
	return runTownStateMigrateCopyIn(env)
}

// runTownStateMigrateCopyIn is what `gt town state migrate copy` runs. It refuses
// before any byte moves or any volume is created: a name that is not a volume, a
// cut-over with an unmet prerequisite, and a populated volume the seed record does
// not name. An absent volume is created, and an empty one is seeded; a seeded one
// gets the delta.
func runTownStateMigrateCopyIn(env townStateMigrateEnv) error {
	if err := checkVolumeName(env.volume); err != nil {
		return err
	}
	exists, err := env.engine.HasVolume(env.volume)
	if err != nil {
		return fmt.Errorf("checking volume %s: %w", env.volume, err)
	}
	inv, err := townstate.Survey(env.town, townstate.Default(), env.exportOptions())
	if err != nil {
		return fmt.Errorf("surveying the town state: %w", err)
	}
	if env.cutover {
		if unmet := unmetChecks(cutoverChecks(env)); len(unmet) > 0 {
			return fmt.Errorf("refusing the cut-over: %s", strings.Join(unmet, "; "))
		}
	}
	if !exists {
		if err := env.engine.CreateVolume(env.volume); err != nil {
			return err
		}
		fmt.Fprintf(env.out, "created volume %s (label %s)\n", env.volume, migrateVolumeLabel)
	}
	// The cut-over window starts here: the copy, its bd read and its verify are
	// all inside it, so the time written is the time the daemon is down for.
	start := env.now()
	empty, err := townstate.VolumeEmpty(env.engine, env.volume)
	if err != nil {
		return err
	}
	if empty {
		return copyIntoEmpty(env, inv, start)
	}
	return copyDelta(env, start)
}

// copyIntoEmpty seeds an empty volume. The seed record is written before the copy
// so the copy carries it, and removed when the copy fails part way, so a partial
// volume names no seed and is refused by every later copy.
func copyIntoEmpty(env townStateMigrateEnv, inv townstate.Inventory, start time.Time) error {
	free, err := townstate.FreeBytes(env.engine, env.volume)
	if err != nil {
		return err
	}
	if free < inv.Bytes {
		return fmt.Errorf("volume %s has %d bytes free; the copy needs %d", env.volume, free, inv.Bytes)
	}
	rec := seedRecord{SeededAt: env.now().UTC(), Volume: env.volume, Files: inv.Files, Bytes: inv.Bytes}
	if err := writeRecordFile(env.town, seedRecordRel, rec); err != nil {
		return err
	}
	sum, err := townstate.SeedVolume(env.engine, env.volume, env.town, townstate.Default(), env.exportOptions(), townstate.ImportArgv, env.out)
	fmt.Fprintln(env.summary, sum.String())
	if err != nil {
		return failedCopy(env, err)
	}
	return finishCopy(env, start, env.exportOptions())
}

// copyDelta sends a populated volume the files it lacks or holds with other
// bytes. It runs only when the seed record names this volume, and it deletes
// nothing: an extra entry, a non-regular entry or a secret on the volume is
// refused before any byte moves, because the copy cannot correct it.
func copyDelta(env townStateMigrateEnv, start time.Time) error {
	seed, ok, err := readSeedRecord(env.town)
	if err != nil {
		return err
	}
	if !ok || seed.Volume != env.volume {
		return fmt.Errorf("volume %s is not empty and no seed record of this town names it; a copy goes into an empty volume, or onto the volume the seed record names", env.volume)
	}
	// The volume holds the whole copy set from its seed, so it is compared with
	// the whole set. --since only limits which differing files this copy sends;
	// an older file that differs and is not sent fails the verify by name.
	full := env.exportOptions()
	full.Since = time.Time{}
	res, err := townstate.VerifyVolume(env.engine, env.volume, env.town, townstate.Default(), full)
	if err != nil {
		return err
	}
	var refused []string
	for _, m := range res.Mismatches {
		if m.Reason == townstate.ReasonExtra || m.Reason == townstate.ReasonNotRegular {
			refused = append(refused, m.Reason+" "+m.Path)
		}
	}
	if res.SecretsOnVolume > 0 {
		refused = append(refused, fmt.Sprintf("%d secret-named entries on the volume (their names are not listed)", res.SecretsOnVolume))
	}
	if len(refused) > 0 {
		for _, line := range refused {
			fmt.Fprintln(env.out, line)
		}
		return fmt.Errorf("volume %s holds entries the copy set does not; a delta deletes nothing, so remove them from the volume, or re-seed it", env.volume)
	}
	// A merge never replaces a link, so a link that differs is refused by name
	// before any byte moves.
	var changedLinks []string
	for _, m := range res.Mismatches {
		if m.Reason != townstate.ReasonChanged {
			continue
		}
		isLink, err := hostIsLink(env.town, m.Path)
		if err != nil {
			return err
		}
		if isLink {
			changedLinks = append(changedLinks, m.Path)
		}
	}
	if len(changedLinks) > 0 {
		for _, name := range changedLinks {
			fmt.Fprintln(env.out, "changed link "+name)
		}
		return fmt.Errorf("volume %s holds links that differ from the copy set; a merge never replaces a link, so remove them from the volume, or re-seed it", env.volume)
	}

	diff := res.Differing()
	host, err := hostDelta(env.town, diff, env.since)
	if err != nil {
		return err
	}
	free, err := townstate.FreeBytes(env.engine, env.volume)
	if err != nil {
		return err
	}
	if free < host.bytes {
		return fmt.Errorf("volume %s has %d bytes free; the delta needs %d", env.volume, free, host.bytes)
	}
	missing, changed := 0, 0
	for _, m := range res.Mismatches {
		switch m.Reason {
		case townstate.ReasonMissing:
			missing++
		case townstate.ReasonChanged:
			changed++
		}
	}
	fmt.Fprintf(env.out, "delta onto seeded volume %s: %d path(s) to send (%d missing, %d changed)\n",
		env.volume, len(diff), missing, changed)

	env.deltaSent = host.sent
	if len(diff) > 0 {
		// The import runs with --merge-newer, which keeps an existing file whose
		// mtime is not older than the archive's. A host file older than the
		// volume's copy therefore stays on the volume and verify refuses it by
		// name. That limit is the merge's, and rollback shares the merge, so the
		// delta keeps it: no import path, mode or flag changes. Touch the host
		// file, or use the full copy, to send it.
		opts := env.exportOptions()
		opts.Only = func(name string) bool { return diff[name] }
		sum, err := townstate.SeedVolume(env.engine, env.volume, env.town, townstate.Default(), opts, townstate.ImportMergeArgv, env.out)
		fmt.Fprintln(env.summary, sum.String())
		if err != nil {
			return failedCopy(env, err)
		}
	}
	return finishCopy(env, start, full)
}

// failedCopy is the return for a copy that stopped part way: the seed record is
// removed, so the partial volume names no seed. A removal that fails is joined
// into the error, never dropped.
func failedCopy(env townStateMigrateEnv, err error) error {
	if rmErr := removeSeedRecord(env.town); rmErr != nil {
		return errors.Join(err, rmErr)
	}
	return err
}

// deltaHost is the host's side of a delta: the bytes the delta weighs and the
// names its export sends.
type deltaHost struct {
	bytes int64
	// sent names the differing entries the export keeps. A regular file is kept
	// only when it is modified after since (the zero time keeps every file), the
	// way Export keeps it; any other entry is always kept.
	sent map[string]bool
}

// hostDelta reads the host's differing names from metadata alone. A name that
// cannot be read is an error, never zero bytes or an unsent file.
func hostDelta(town string, names map[string]bool, since time.Time) (deltaHost, error) {
	d := deltaHost{sent: map[string]bool{}}
	for name := range names {
		info, err := os.Lstat(filepath.Join(town, filepath.FromSlash(name)))
		if err != nil {
			return deltaHost{}, fmt.Errorf("reading the size of %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			d.sent[name] = true
			continue
		}
		d.bytes += info.Size()
		if since.IsZero() || info.ModTime().After(since) {
			d.sent[name] = true
		}
	}
	return d, nil
}

// hostIsLink reports whether the host's name is a symbolic link, read from its
// metadata alone. A name that cannot be read is an error, never a regular file.
func hostIsLink(town, name string) (bool, error) {
	info, err := os.Lstat(filepath.Join(town, filepath.FromSlash(name)))
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", name, err)
	}
	return info.Mode()&fs.ModeSymlink != 0, nil
}

// finishCopy is the end of every copy that moved bytes: the bd read through the
// volume, then the verify of verifyOpts' set. Only when both pass does a cut-over
// write its time, which runs from start (the copy's first read) to the end of the
// verify. A failed read or a verify that names a problem writes no cut-over record.
func finishCopy(env townStateMigrateEnv, start time.Time, verifyOpts townstate.ExportOptions) error {
	if err := runBeadsReadThroughVolume(env); err != nil {
		return err
	}
	res, err := verifyCopy(env, verifyOpts)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.out, "copy verified: volume %s matches the host copy set (%d files)\n", env.volume, res.Files)
	elapsed := env.now().Sub(start)
	if !env.cutover {
		fmt.Fprintf(env.out, "copy took %s\n", elapsed.Round(time.Millisecond))
		return nil
	}
	return writeCutover(env, elapsed, verifyOpts)
}

// verifyCopy compares the volume with the host copy set and returns what it
// compared. Any problem it names is an exit-1 error, and the error says no
// cut-over time is written.
func verifyCopy(env townStateMigrateEnv, verifyOpts townstate.ExportOptions) (townstate.VerifyResult, error) {
	res, err := townstate.VerifyVolume(env.engine, env.volume, env.town, townstate.Default(), verifyOpts)
	if err != nil {
		return res, err
	}
	if problems := printVerifyProblems(env.out, res, env.deltaSent); problems > 0 {
		return res, &ExitCodeError{Code: 1, Err: fmt.Errorf("the copy to volume %s does not verify: %d problems; no cut-over time is written", env.volume, problems)}
	}
	return res, nil
}

// writeCutover writes the cut-over time, then sends that record to the volume and
// verifies the volume again with it in the set. The record is written after the
// copy's verify, so the copy cannot carry it; the send puts it on the volume the
// daemon reads, and the second verify proves the volume holds it. A record that
// does not reach the volume, or a volume that does not verify with it, is removed
// from the host and from the volume, so no cut-over time stands for a volume
// without it.
func writeCutover(env townStateMigrateEnv, elapsed time.Duration, verifyOpts townstate.ExportOptions) error {
	rec := cutoverRecord{CutoverAt: env.now().UTC().Format(time.RFC3339), Volume: env.volume, CopySeconds: elapsed.Seconds()}
	if err := writeRecordFile(env.town, cutoverRecordRel, rec); err != nil {
		return err
	}
	if err := sendCutoverRecord(env); err != nil {
		return errors.Join(err, unwindCutover(env))
	}
	if _, err := verifyCopy(env, verifyOpts); err != nil {
		return errors.Join(err, unwindCutover(env))
	}
	fmt.Fprintf(env.out, "cut-over copy took %s; the cut-over time is in %s and on volume %s\n", elapsed.Round(time.Millisecond), cutoverRecordRel, env.volume)
	return nil
}

// unwindCutover removes the cut-over record from the host and from the volume. A
// removal that fails is named in the error, never dropped: the volume may still
// carry the record.
func unwindCutover(env townStateMigrateEnv) error {
	hostErr := removeRecordFile(env.town, cutoverRecordRel)
	if err := townstate.RemoveVolumeFile(env.engine, env.volume, cutoverRecordRel); err != nil {
		return errors.Join(hostErr, fmt.Errorf("removing the cut-over record from volume %s: %w", env.volume, err))
	}
	return hostErr
}

// sendCutoverRecord copies the cut-over record alone onto the volume, through the
// merge import a delta uses, so nothing else on the volume is touched.
func sendCutoverRecord(env townStateMigrateEnv) error {
	opts := env.exportOptions()
	opts.Only = func(name string) bool { return name == cutoverRecordRel }
	sum, err := townstate.SeedVolume(env.engine, env.volume, env.town, townstate.Default(), opts, townstate.ImportMergeArgv, env.out)
	fmt.Fprintln(env.summary, sum.String())
	if err != nil {
		return fmt.Errorf("sending the cut-over record to volume %s: %w", env.volume, err)
	}
	return nil
}

// runBeadsReadThroughVolume runs one bd read through the volume copy and reports
// the endpoint it used. A read that was not forwarded, or that failed, is an
// error: the copy is not proven to serve bd.
func runBeadsReadThroughVolume(env townStateMigrateEnv) error {
	if env.beadsHost == "" {
		fmt.Fprintln(env.out, "bd read through the volume copy: not forwarded (BEADS_DOLT_SERVER_HOST is unset in this shell, and the town names no dolt.container_host)")
		return errors.New("bd read through the volume copy was not forwarded; set BEADS_DOLT_SERVER_HOST or dolt.container_host in the town settings")
	}
	var out bytes.Buffer
	if err := env.engine.Run(env.volume, townstate.BeadsReadArgv, nil, &out); err != nil {
		fmt.Fprintf(env.out, "bd read through the volume copy: FAILED (%v)\n", err)
		return fmt.Errorf("bd read through the volume copy failed; check BEADS_DOLT_SERVER_HOST in the container")
	}
	fmt.Fprintf(env.out, "bd read through the volume copy: ok (%d lines), reaching Dolt at BEADS_DOLT_SERVER_HOST=%s (from %s)\n",
		strings.Count(out.String(), "\n"), env.beadsHost, env.beadsHostSource)
	return nil
}

func runTownStateMigrateVerify(cmd *cobra.Command, _ []string) error {
	env, err := migrateEnvFromFlags(cmd, true, workspace.FindFromCwdOrError, os.Getenv("BEADS_DOLT_SERVER_HOST"))
	if err != nil {
		return err
	}
	return runTownStateMigrateVerifyIn(env)
}

// runTownStateMigrateVerifyIn is what `gt town state migrate verify` runs. Any
// difference, or any secret-named entry on the volume, fails it.
func runTownStateMigrateVerifyIn(env townStateMigrateEnv) error {
	if err := requireMigrateVolume(env); err != nil {
		return err
	}
	res, err := townstate.VerifyVolume(env.engine, env.volume, env.town, townstate.Default(), env.exportOptions())
	if err != nil {
		return err
	}
	if problems := printVerifyProblems(env.out, res, nil); problems > 0 {
		return &ExitCodeError{Code: 1, Err: fmt.Errorf("volume %s does not match the host: %d problems", env.volume, problems)}
	}
	fmt.Fprintf(env.out, "migrate verify: volume %s matches the host copy set (%d files)\n", env.volume, res.Files)
	return nil
}

// olderHostNote is the way out printed for a changed file the delta sent: the
// merge kept the volume's copy because the host's was not newer.
const olderHostNote = "host copy is older than the volume's, so the delta does not overwrite it; touch the host file or use the full copy"

// printVerifyProblems prints each mismatch by path and the count of secret-named
// entries, never their names or contents, and returns how many problems it found.
// A changed path in sent (a delta's) also names olderHostNote.
func printVerifyProblems(out io.Writer, res townstate.VerifyResult, sent map[string]bool) int {
	for _, m := range res.Mismatches {
		if m.Reason == townstate.ReasonChanged && sent[m.Path] {
			fmt.Fprintf(out, "%s %s: %s\n", m.Reason, m.Path, olderHostNote)
			continue
		}
		fmt.Fprintf(out, "%s %s\n", m.Reason, m.Path)
	}
	problems := len(res.Mismatches)
	if res.SecretsOnVolume > 0 {
		fmt.Fprintf(out, "%d secret-named entries are on the volume; the copy must not carry them (their names are not listed)\n", res.SecretsOnVolume)
		problems++
	}
	return problems
}

func runTownStateMigrateRollbackPlan(cmd *cobra.Command, _ []string) error {
	env, err := migrateEnvFromFlags(cmd, false, workspace.FindFromCwdOrError, os.Getenv("BEADS_DOLT_SERVER_HOST"))
	if err != nil {
		return err
	}
	return runTownStateMigrateRollbackPlanIn(env)
}

// runTownStateMigrateRollbackPlanIn prints the rollback in the order it runs. It
// reads the town's daemon container name for the first step, so an unnamed
// container is an error, not a step with a blank name. Every path it prints is
// one shell word, so a path with a space still prints a command that runs.
func runTownStateMigrateRollbackPlanIn(env townStateMigrateEnv) error {
	if env.volume == "" {
		return errors.New("the rollback plan needs --volume")
	}
	container, err := env.probes.daemonContainer(env.town)
	if err != nil {
		return fmt.Errorf("naming the daemon container for the rollback: %w", err)
	}
	steps := []string{
		fmt.Sprintf("stop the daemon container on the volume: docker --context %s stop %s",
			templates.ContainerDockerContext, shellWord(container)),
		fmt.Sprintf("export the rollback rows from the volume: docker --context %s run --rm --entrypoint \"\" -v %s %s gt town state export --town %s --rollback > rollback.tar",
			templates.ContainerDockerContext, shellWord(env.volume+":"+townstate.VolumeMount), shellWord(env.image), shellWord(townstate.VolumeMount)),
		fmt.Sprintf("import the rollback onto the host tree, writing only the newer files: gt town state import --town %s --merge-newer < rollback.tar", shellWord(env.town)),
		fmt.Sprintf("restore the saved compose file: cp %s %s", shellWord(env.savedCompose), shellWord(env.composePath)),
	}
	fmt.Fprintf(env.out, "rollback plan for volume %s, run in this order:\n", env.volume)
	for i, s := range steps {
		fmt.Fprintf(env.out, "%d. %s\n", i+1, s)
	}
	return nil
}

// shellWord returns s as one shell word: a word of safe characters is printed
// bare, anything else is single-quoted, with each embedded quote closed, escaped
// and reopened, so a path that holds a quote still prints as one word.
func shellWord(s string) string {
	if s != "" && safeShellWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// checkVolumeName refuses a name outside the docker volume shape, so a path can
// never become a bind mount.
func checkVolumeName(volume string) error {
	if !volumeNamePattern.MatchString(volume) {
		return fmt.Errorf("volume %q is not a docker volume name", volume)
	}
	return nil
}

// requireMigrateVolume is verify's check: a valid name and a volume that exists.
// Only copy creates a volume, so a mistyped name is refused here instead of being
// listed as an empty volume.
func requireMigrateVolume(env townStateMigrateEnv) error {
	if err := checkVolumeName(env.volume); err != nil {
		return err
	}
	exists, err := env.engine.HasVolume(env.volume)
	if err != nil {
		return fmt.Errorf("checking volume %s: %w", env.volume, err)
	}
	if !exists {
		return fmt.Errorf("volume %s does not exist; migrate copy creates it", env.volume)
	}
	return nil
}

// migrateEnvFromFlags reads one migrate command's flags into its env, with the
// production engine and probes. The engine and the image are only resolved when
// the command reaches the volume (copy, verify, or a plan given --volume, or a
// rollback plan, which prints the image). The copy also resolves the Dolt host
// its bd read forwards: shellBeadsHost, the caller's BEADS_DOLT_SERVER_HOST, wins,
// else the town settings. A command reads only the flags it defines.
func migrateEnvFromFlags(cmd *cobra.Command, needSecrets bool, findTown func() (string, error), shellBeadsHost string) (townStateMigrateEnv, error) {
	env := townStateMigrateEnv{now: time.Now, out: cmd.OutOrStdout(), summary: cmd.ErrOrStderr(), probes: productionMigrateProbes()}
	town, err := migrateFlagString(cmd, "town")
	if err != nil {
		return env, err
	}
	if town == "" {
		town, err = findTown()
		if err != nil {
			return env, fmt.Errorf("finding the town root: %w", err)
		}
	}
	env.town = town
	images, err := migrateFlagString(cmd, "images")
	if err != nil {
		return env, err
	}
	if images == "" {
		// The town's own values file, whatever directory the command runs from.
		images = filepath.Join(town, filepath.FromSlash(imagevalues.DefaultRelPath))
	}
	compose, err := migrateFlagString(cmd, "compose-file")
	if err != nil {
		return env, err
	}
	if compose == "" {
		compose = filepath.Join(town, "daemon", "container", "compose.yml")
	}
	env.composePath = compose
	if env.volume, err = migrateFlagString(cmd, "volume"); err != nil {
		return env, err
	}
	saved, err := migrateFlagString(cmd, "saved-compose")
	if err != nil {
		return env, err
	}
	if saved == "" {
		saved = compose + ".pre-cutover"
	}
	env.savedCompose = saved
	since, err := migrateFlagString(cmd, "since")
	if err != nil {
		return env, err
	}
	if since != "" {
		parsed, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return env, fmt.Errorf("--since %q is not an RFC 3339 time", since)
		}
		env.since = parsed
	}
	if cmd.Flags().Lookup("cutover") != nil {
		if env.cutover, err = cmd.Flags().GetBool("cutover"); err != nil {
			return env, fmt.Errorf("reading --cutover: %w", err)
		}
	}
	if cmd.Flags().Lookup("daemon-removed") != nil {
		if env.daemonRemoved, err = cmd.Flags().GetBool("daemon-removed"); err != nil {
			return env, fmt.Errorf("reading --daemon-removed: %w", err)
		}
	}
	if needSecrets {
		secretsPath, err := migrateFlagString(cmd, "secrets-manifest")
		if err != nil {
			return env, err
		}
		data, err := os.ReadFile(secretsPath)
		if err != nil {
			return env, fmt.Errorf("reading the secrets manifest: %w", err)
		}
		if env.exclude, err = secretExclusion(data); err != nil {
			return env, err
		}
	}
	needImage := env.volume != "" || cmd.Name() == "rollback-plan"
	if needImage {
		if env.image, err = helperImage(images, compose); err != nil {
			return env, err
		}
		if cmd.Name() == "copy" {
			if env.beadsHost, env.beadsHostSource, err = resolveBeadsHost(town, shellBeadsHost); err != nil {
				return env, err
			}
		}
		env.engine = newDockerEngine(env.image, env.beadsHost)
	}
	return env, nil
}

// resolveBeadsHost names the Dolt host the helper's bd reads. shellHost is the
// operator's BEADS_DOLT_SERVER_HOST, and it wins; else the town's
// dolt.container_host, the name the container reaches the server by. A town that
// names neither returns an empty host, which the bd read reports as not
// forwarded. An unreadable town settings file is an error.
func resolveBeadsHost(town, shellHost string) (string, string, error) {
	if v := strings.TrimSpace(shellHost); v != "" {
		return v, "the operator's shell", nil
	}
	cfg, err := config.LoadTownConfig(filepath.Join(town, "mayor", "town.json"))
	if errors.Is(err, config.ErrNotFound) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("reading the town settings for dolt.container_host: %w", err)
	}
	if cfg.Dolt != nil {
		if v := strings.TrimSpace(cfg.Dolt.ContainerHost); v != "" {
			return v, "the town's dolt.container_host", nil
		}
	}
	return "", "", nil
}

// migrateFlagString reads a string flag the command defines, and "" for one it
// does not define, so the shared reads serve every migrate subcommand.
func migrateFlagString(cmd *cobra.Command, name string) (string, error) {
	if cmd.Flags().Lookup(name) == nil {
		return "", nil
	}
	v, err := cmd.Flags().GetString(name)
	if err != nil {
		return "", fmt.Errorf("reading --%s: %w", name, err)
	}
	return v, nil
}

// secretExclusion parses the secrets manifest and returns the exclusion for its
// files: a file whose base name the manifest lists is a secret. The unclaimed
// list is not a secret list, so it is not excluded.
func secretExclusion(data []byte) (func(string) bool, error) {
	sm, err := templates.ParseSecretsManifest(data)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, f := range sm.Files {
		names[f.Name] = true
	}
	return func(name string) bool { return names[path.Base(name)] }, nil
}

// helperImage names the daemon image the helper runs: the digest in the image
// values file when that file exists, else the gt-daemon tag the compose file
// names. A values file that exists but cannot be read is an error, never a
// fall back to the tag.
func helperImage(imagesPath, composePath string) (string, error) {
	v, err := imagevalues.Load(imagesPath)
	switch {
	case err == nil:
		// imagevalues.Parse refuses a file with no daemon line, so v.Daemon is set.
		return v.Daemon, nil
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("reading the image values %s: %w", imagesPath, err)
	}
	data, err := os.ReadFile(composePath)
	if err != nil {
		return "", fmt.Errorf("reading the compose file %s for the daemon image: %w", composePath, err)
	}
	tag := daemonImageTag.Find(data)
	if tag == nil {
		return "", fmt.Errorf("%s names no gt-daemon image tag", composePath)
	}
	return string(tag), nil
}

// dockerEngine is the production townstate.Engine: each command is one docker
// run of the helper image with the volume mounted at townstate.VolumeMount.
type dockerEngine struct {
	image string
	run   dockerRun
	// beadsHost is the Dolt host the helper's bd reads. It is forwarded by name,
	// with its value in the docker client's environment, never on the command
	// line. Empty forwards nothing.
	beadsHost string
	// helperTimeout bounds one helper run. Zero means migrateHelperTimeout; a
	// test sets it short to drive the timeout without waiting ten minutes.
	helperTimeout time.Duration
}

// dockerRun runs one docker argv with the environment and the streams given.
// Production is execDocker; a test supplies a fake and starts no process.
type dockerRun func(ctx context.Context, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error

// newDockerEngine is the engine the migrate command runs against the host's docker.
func newDockerEngine(image, beadsHost string) dockerEngine {
	return dockerEngine{image: image, run: execDocker, beadsHost: beadsHost}
}

// execDocker is the production dockerRun: the docker binary with args, and env
// added to the inherited environment.
func execDocker(ctx context.Context, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // fixed argv; the volume name is validated before it is built
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// Run runs argv in a fresh helper, bounded by migrateHelperTimeout. The image's
// own entrypoint is cleared: it runs `gt install --force` before the command,
// which must not happen on a copy. Stderr goes to the operator's terminal, never
// into an error.
func (e dockerEngine) Run(volume string, argv []string, stdin io.Reader, stdout io.Writer) error {
	name := helperName()
	args, err := helperRunArgs(volume, e.image, name, argv, e.beadsHost != "")
	if err != nil {
		return err
	}
	var env []string
	if e.beadsHost != "" {
		env = []string{"BEADS_DOLT_SERVER_HOST=" + e.beadsHost}
	}
	timeout := e.helperTimeout
	if timeout == 0 {
		timeout = migrateHelperTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := e.run(ctx, args, env, stdin, stdout, os.Stderr); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return e.stopHelper(volume, name, timeout)
		}
		return fmt.Errorf("docker run of the helper for volume %s exited: %w", volume, err)
	}
	return nil
}

// stopHelper removes the helper container by name after its run passed the
// deadline. Killing the docker client does not stop the container, so the removal
// goes to docker itself. The message says "stopped" only when the removal
// succeeded; docker's output is never put in the error.
func (e dockerEngine) stopHelper(volume, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), migrateProbeTimeout)
	defer cancel()
	args := []string{"--context", templates.ContainerDockerContext, "rm", "-f", name}
	if err := e.run(ctx, args, nil, nil, io.Discard, io.Discard); err != nil {
		return fmt.Errorf("the helper for volume %s did not finish within %s; helper may still be running: container %s", volume, timeout, name)
	}
	return fmt.Errorf("the helper for volume %s did not finish within %s and was stopped: container %s", volume, timeout, name)
}

// helperName is the container name of one helper run, so a run past its deadline
// can be removed by name. The process id and clock keep names apart across runs.
func helperName() string {
	return fmt.Sprintf("gt-town-state-migrate-%d-%d", os.Getpid(), time.Now().UnixNano())
}

// helperRunArgs is the docker argv of one helper run: the volume mounted at
// townstate.VolumeMount, the entrypoint cleared, and the image's own argv. The
// volume name is validated first, so a path can never become a bind mount.
// forwardBeads passes BEADS_DOLT_SERVER_HOST through by name, never by value.
func helperRunArgs(volume, image, name string, argv []string, forwardBeads bool) ([]string, error) {
	if !volumeNamePattern.MatchString(volume) {
		return nil, fmt.Errorf("volume %q is not a docker volume name", volume)
	}
	// A listing or a df only reads the volume, so it mounts it read-only: a
	// verify or a plan cannot change what it checks.
	mount := volume + ":" + townstate.VolumeMount
	if strings.Join(argv, " ") == strings.Join(townstate.ListArgv, " ") || strings.Join(argv, " ") == strings.Join(townstate.FreeArgv, " ") {
		mount += ":ro"
	}
	args := []string{"--context", templates.ContainerDockerContext, "run", "--rm", "-i",
		"--name", name,
		"--entrypoint", "", "-w", townstate.VolumeMount,
		"-v", mount}
	if forwardBeads {
		args = append(args, "-e", "BEADS_DOLT_SERVER_HOST")
	}
	args = append(args, image)
	return append(args, argv...), nil
}

// HasVolume reports whether docker knows the named volume. Only the marker that
// docker prints for an absent volume is read as "absent"; any other failure is
// an error, and the stderr text is never put in it.
func (e dockerEngine) HasVolume(volume string) (bool, error) {
	if !volumeNamePattern.MatchString(volume) {
		return false, fmt.Errorf("volume %q is not a docker volume name", volume)
	}
	ctx, cancel := context.WithTimeout(context.Background(), migrateProbeTimeout)
	defer cancel()
	var stderr bytes.Buffer
	err := e.run(ctx, []string{"--context", templates.ContainerDockerContext, "volume", "inspect", volume}, nil, nil, io.Discard, &stderr)
	if err == nil {
		return true, nil
	}
	if strings.Contains(stderr.String(), "No such volume") {
		return false, nil
	}
	return false, fmt.Errorf("docker volume inspect %s failed: %w", volume, err)
}

// CreateVolume creates the named volume with the migrate label, so an operator can
// tell a volume the copy made from one compose or a person created. A failure is
// reported without docker's stderr, which the error never carries.
func (e dockerEngine) CreateVolume(volume string) error {
	if !volumeNamePattern.MatchString(volume) {
		return fmt.Errorf("volume %q is not a docker volume name", volume)
	}
	ctx, cancel := context.WithTimeout(context.Background(), migrateProbeTimeout)
	defer cancel()
	args := []string{"--context", templates.ContainerDockerContext, "volume", "create", "--label", migrateVolumeLabel, volume}
	if err := e.run(ctx, args, nil, nil, io.Discard, io.Discard); err != nil {
		return fmt.Errorf("docker volume create %s failed: %w", volume, err)
	}
	return nil
}

// productionMigrateProbes reads the cut-over prerequisites from the host: the
// operator's dispatch hold file, the daemon container's state under docker, and
// the landing worker's own queue, candidate and post-land state of each rig.
func productionMigrateProbes() migrateProbes {
	return migrateProbes{
		holdPresent:     holdFilePresent,
		daemonContainer: daemonContainerName,
		containerRunning: func(name string) (bool, error) {
			return containerRunningWith(templates.RunDockerInspect, name)
		},
		landingQueue: func(town string) (int, error) {
			return landingQueueCount(listReadyToLand, town)
		},
		landingCandidates: landingCandidates,
		postLandPending:   postLandPending,
	}
}

func holdFilePresent(town string) (bool, error) {
	_, err := os.Stat(dispatch.HoldFilePath(town))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// daemonContainerName names the container from the town's own settings. A town
// that names no container has none to read, which is an error, not a default.
func daemonContainerName(town string) (string, error) {
	cfg, err := daemon.ReadPatrolConfig(town)
	if err != nil {
		return "", fmt.Errorf("reading the daemon settings: %w", err)
	}
	if cfg == nil || cfg.ContainerNameSetting == "" {
		return "", errors.New("the town names no daemon.container_name")
	}
	if err := cfg.ValidateContainerNames(); err != nil {
		return "", err
	}
	return cfg.ContainerName(), nil
}

// errDaemonContainerAbsent is containerRunningWith's answer for a container
// docker cannot find. It is neither running nor stopped: the cut-over gate reads
// it only with the operator's --daemon-removed.
var errDaemonContainerAbsent = errors.New("the daemon container is absent")

// containerRunningWith asks docker, through run, whether the container is
// running. A container docker cannot find is errDaemonContainerAbsent, never
// stopped (a misnamed container reads the same as a removed one); a container
// docker cannot inspect is unknown.
func containerRunningWith(run templates.ContainerDocker, name string) (bool, error) {
	st := templates.ContainerJobState(run, templates.ContainerDockerContext, name)(config.SupervisorContainer)
	if st.Err != nil {
		return false, st.Err
	}
	if !st.Loaded {
		return false, errDaemonContainerAbsent
	}
	return !stoppedContainerStates[st.Status], nil
}

// townRigNames is the town's rigs from its registry, sorted. A town with no
// registry has no rigs; a registry that does not parse is an error.
func townRigNames(town string) ([]string, error) {
	rigs, err := config.LoadRigsConfigOrEmpty(constants.MayorRigsPath(town))
	if err != nil {
		return nil, fmt.Errorf("reading the rig registry: %w", err)
	}
	names := make([]string, 0, len(rigs.Rigs))
	for name := range rigs.Rigs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// readyLister lists one rig's beads labeled ready-to-land, as the landing worker
// lists them. Production is listReadyToLand; a test supplies a fake.
type readyLister func(rigRoot string) ([]*beads.Issue, error)

// listReadyToLand is the production readyLister: the rig's own bead store, asked
// the same question the landing worker asks, and bounded by migrateProbeTimeout.
func listReadyToLand(rigRoot string) ([]*beads.Issue, error) {
	return listWithin(migrateProbeTimeout, func() ([]*beads.Issue, error) {
		return beads.NewWithBeadsDir(rigRoot, beads.ResolveBeadsDir(rigRoot)).List(readyToLandListOptions())
	})
}

// readyToLandStatuses are the statuses the landing worker still acts on. The
// list asks bd for them by name: bd's own default for a status left out is not
// relied on.
var readyToLandStatuses = []beads.IssueStatus{beads.StatusOpen, beads.StatusInProgress, beads.StatusBlocked, beads.IssueStatusHooked}

// readyToLandListOptions is the bd query the queue runs: the ready-to-land label,
// in the actionable statuses, at every priority.
func readyToLandListOptions() beads.ListOptions {
	statuses := make([]string, 0, len(readyToLandStatuses))
	for _, s := range readyToLandStatuses {
		statuses = append(statuses, string(s))
	}
	return beads.ListOptions{Status: strings.Join(statuses, ","), Label: land.LabelReadyToLand, Priority: -1}
}

// listWithin runs list and returns its answer, or an error once d has passed. A
// list still running at the deadline is left to finish on its own; the command
// that asked has already moved on with an unmet check.
func listWithin(d time.Duration, list func() ([]*beads.Issue, error)) ([]*beads.Issue, error) {
	type answer struct {
		issues []*beads.Issue
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		issues, err := list()
		done <- answer{issues, err}
	}()
	select {
	case a := <-done:
		return a.issues, a.err
	case <-time.After(d):
		return nil, fmt.Errorf("the bead list did not answer within %s", d)
	}
}

// landingQueueCount counts the ready-to-land beads the landing worker still acts
// on: those in an actionable status (open, hooked, in_progress or blocked), not
// deferred, pinned or closed. A rig whose list cannot be read fails the count.
func landingQueueCount(list readyLister, town string) (int, error) {
	rigs, err := townRigNames(town)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, name := range rigs {
		issues, err := list(filepath.Join(town, name))
		if err != nil {
			// bd's failure text can carry its output, which an error never prints:
			// the rig and the list that failed are named, the cause is withheld.
			return 0, fmt.Errorf("rig %s: listing its ready-to-land beads failed; its output is withheld", name)
		}
		for _, issue := range issues {
			if issue != nil && beads.IssueStatus(strings.TrimSpace(issue.Status)).IsActionable() && beads.HasLabel(issue, land.LabelReadyToLand) {
				total++
			}
		}
	}
	return total, nil
}

// landingCandidates names the rigs whose landing work root holds a landing,
// post-landing or revert run. The root is the landing worker's own (daemon's
// LandingWorkRoot), and each run's directory is listed by land.WorkRuns. Each
// rig is named with its run directories and how long ago each last changed, so
// a run that never ended reads as stale, not as live.
func landingCandidates(town string) ([]string, error) {
	cfg, err := daemon.ReadPatrolConfig(town)
	if err != nil {
		return nil, fmt.Errorf("reading the daemon settings: %w", err)
	}
	rigs, err := townRigNames(town)
	if err != nil {
		return nil, err
	}
	var busy []string
	for _, name := range rigs {
		root, err := daemon.LandingWorkRoot(cfg, town, name)
		if err != nil {
			return nil, fmt.Errorf("rig %s: %w", name, err)
		}
		runs, err := land.WorkRuns(root)
		if err != nil {
			return nil, fmt.Errorf("rig %s work root: %w", name, err)
		}
		if len(runs) == 0 {
			continue
		}
		named := make([]string, 0, len(runs))
		for _, r := range runs {
			named = append(named, fmt.Sprintf("%s last changed %s ago", r.Name, time.Since(r.ModTime).Round(time.Minute)))
		}
		busy = append(busy, fmt.Sprintf("%s [%s]", name, strings.Join(named, "; ")))
	}
	return busy, nil
}

// postLandPending names each rig whose red-main state holds post-landing verdicts
// still waiting on CI, with the commit the newest one is for. The state is the
// landing worker's own store, so a missing file is the zero state, not a fault.
func postLandPending(town string) ([]string, error) {
	rigs, err := townRigNames(town)
	if err != nil {
		return nil, err
	}
	var pending []string
	for _, name := range rigs {
		st, err := daemon.RedMainStateStore(town, name).Load()
		if err != nil {
			return nil, fmt.Errorf("rig %s red-main state: %w", name, err)
		}
		if n := len(st.PostLandPending); n > 0 {
			pending = append(pending, fmt.Sprintf("%s (newest verdict at %s)", name, shortCommit(st.PostLandPending[n-1].Land.Commit)))
		}
	}
	return pending, nil
}

// shortCommit is a commit's first twelve characters, or the whole of a shorter one.
func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}
