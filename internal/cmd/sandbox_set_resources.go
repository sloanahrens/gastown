package cmd

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/workspace"
)

// `gt sandbox set-resources` (gt-mb54kg) changes the requests and limits of one
// sandbox profile the way `gt pool set` changes a seat cap: only the named
// fields move, the whole resulting profile is validated with the loader's own
// check before anything is written, and the file is replaced atomically with the
// previous copy kept beside it.

var (
	sandboxSetResourcesRequests string
	sandboxSetResourcesLimits   string
	sandboxSetResourcesDryRun   bool
)

var sandboxSetResourcesCmd = &cobra.Command{
	Use:   "set-resources <profile>",
	Short: "Set a sandbox profile's requests and limits, leaving the rest of it alone",
	Long: `Set the CPU and memory requests or limits of a named sandbox profile in
settings/config.json. Each flag names the fields to change; a field not named
keeps its value, and so does every other profile and key.

gt sandbox set-resources polecat-k8s --requests cpu=500m,memory=1536Mi
gt sandbox set-resources polecat-k8s --limits memory=3Gi --dry-run

Quantities take the Kubernetes forms the profile loader accepts: "500m", "1",
"1536Mi", "3Gi". The resulting profile is validated with the same check the
loader runs, so an unknown profile, an unparsable quantity, requests above
limits, or a change that names nothing refuses and leaves the file as it was.
Requests and limits are kubernetes-driver fields; a docker profile refuses them.

--dry-run prints the change and writes nothing.

The file is written atomically and the previous copy is kept beside it as
<file>.bak-<timestamp>; the newest 30 and the last 7 days are kept. No daemon
restart is needed: the profile is read from the file on each spawn and each
dispatcher tick, so the change applies to sandboxes created after it.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSandboxSetResourcesCmd(cmd.OutOrStdout(), workspace.FindFromCwdOrError, args[0], sandboxSetResourcesFlags{
			requests:    sandboxSetResourcesRequests,
			requestsSet: cmd.Flags().Changed("requests"),
			limits:      sandboxSetResourcesLimits,
			limitsSet:   cmd.Flags().Changed("limits"),
			dryRun:      sandboxSetResourcesDryRun,
		})
	},
}

// sandboxSetResourcesFlags is what the command's flags carry into one run.
// requestsSet and limitsSet say whether the flag was given at all, so an empty
// value is refused rather than read as no flag.
type sandboxSetResourcesFlags struct {
	requests    string
	requestsSet bool
	limits      string
	limitsSet   bool
	dryRun      bool
}

// runSandboxSetResourcesCmd parses the flags, then finds the town and runs the
// change. The flags are read first, so a bad value is refused without looking
// for a town. find is workspace.FindFromCwdOrError in production.
func runSandboxSetResourcesCmd(w io.Writer, find func() (string, error), name string, f sandboxSetResourcesFlags) error {
	var edits []sandboxResourceEdit
	if f.requestsSet {
		e, err := parseSandboxResourceFlag("requests", f.requests)
		if err != nil {
			return err
		}
		edits = append(edits, e...)
	}
	if f.limitsSet {
		e, err := parseSandboxResourceFlag("limits", f.limits)
		if err != nil {
			return err
		}
		edits = append(edits, e...)
	}
	townRoot, err := find()
	if err != nil {
		return err
	}
	return runSandboxSetResources(w, townRoot, name, edits, f.dryRun)
}

func init() {
	sandboxSetResourcesCmd.Flags().StringVar(&sandboxSetResourcesRequests, "requests", "", "Requests to set: cpu=<quantity>, memory=<quantity>, or both")
	sandboxSetResourcesCmd.Flags().StringVar(&sandboxSetResourcesLimits, "limits", "", "Limits to set: cpu=<quantity>, memory=<quantity>, or both")
	sandboxSetResourcesCmd.Flags().BoolVar(&sandboxSetResourcesDryRun, "dry-run", false, "Print the change and write nothing")
	sandboxCmd.AddCommand(sandboxSetResourcesCmd)
}

// sandboxResourceEdit is one field a flag names: block is "requests" or
// "limits", field is "cpu" or "memory", value the quantity to set.
type sandboxResourceEdit struct {
	Block string
	Field string
	Value string
}

// parseSandboxResourceFlag reads a --requests or --limits value: a comma-separated
// list of cpu=<quantity> and memory=<quantity>, each named at most once. The
// quantity itself is checked by the profile loader, once the whole profile is
// assembled.
func parseSandboxResourceFlag(block, spec string) ([]sandboxResourceEdit, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, fmt.Errorf("--%s: empty; want cpu=<quantity>, memory=<quantity>, or both", block)
	}
	seen := map[string]bool{}
	var out []sandboxResourceEdit
	for _, part := range strings.Split(spec, ",") {
		field, value, ok := strings.Cut(part, "=")
		if !ok || field == "" {
			return nil, fmt.Errorf("--%s %q: want cpu=<quantity> or memory=<quantity>", block, part)
		}
		if field != "cpu" && field != "memory" {
			return nil, fmt.Errorf("--%s: unknown resource %q: want cpu or memory", block, field)
		}
		if value == "" {
			return nil, fmt.Errorf("--%s %s=: empty quantity; name a value such as %s", block, field, exampleSandboxQuantity(field))
		}
		if seen[field] {
			return nil, fmt.Errorf("--%s names %s twice", block, field)
		}
		seen[field] = true
		out = append(out, sandboxResourceEdit{Block: block, Field: field, Value: value})
	}
	return out, nil
}

// exampleSandboxQuantity is a quantity of the form the field takes, for a refusal.
func exampleSandboxQuantity(field string) string {
	if field == "cpu" {
		return `"500m"`
	}
	return `"1536Mi"`
}

// sandboxResourcePlan is the change one run would make to one profile: the
// profile before and after.
type sandboxResourcePlan struct {
	before config.SandboxProfile
	after  config.SandboxProfile
}

// sandboxBackup keeps the copy a write replaces; config.BackupFile is the one the
// command uses. It is a parameter so a test can make the copy fail.
type sandboxBackup func(path, stamp string) (string, error)

// runSandboxSetResources applies edits to the named profile and writes the file.
// A dry run plans against the file as it is and changes nothing. A real run plans
// under the file's lock, so a refusal leaves the settings as they were and makes
// no copy.
func runSandboxSetResources(w io.Writer, townRoot, name string, edits []sandboxResourceEdit, dryRun bool) error {
	if len(edits) == 0 {
		return fmt.Errorf("nothing to change: name --requests cpu=...,memory=... or --limits cpu=...,memory=...")
	}
	path := config.TownSettingsPath(townRoot)
	if dryRun {
		ts, err := config.LoadOrCreateTownSettings(path)
		if err != nil {
			return fmt.Errorf("loading town settings: %w", err)
		}
		plan, err := planSandboxResources(ts, name, edits)
		if err != nil {
			return err
		}
		printSandboxResourceChange(w, name, plan)
		fmt.Fprintln(w, "dry run: nothing written")
		return nil
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	plan, backup, err := writeSandboxResources(path, stamp, name, edits, config.BackupFile)
	if err != nil {
		return err
	}
	printSandboxResourceChange(w, name, plan)
	if backup != "" {
		fmt.Fprintf(w, "previous file kept at %s\n", backup)
	}
	return nil
}

// writeSandboxResources plans the change against the settings file as it is
// under its lock, keeps a copy of the file, and replaces the profile. The plan is
// made under the lock, so a concurrent edit is what gets validated.
func writeSandboxResources(path, stamp, name string, edits []sandboxResourceEdit, backup sandboxBackup) (sandboxResourcePlan, string, error) {
	var plan sandboxResourcePlan
	var copied string
	err := config.UpdateConfigJSON(path, 0o600, func(ts *config.TownSettings, _ bool) error {
		p, err := planSandboxResources(ts, name, edits)
		if err != nil {
			return err
		}
		copied, err = backup(config.WritePath(path), stamp)
		if err != nil {
			return err
		}
		ts.Sandboxes[name] = p.after
		plan = p
		return nil
	})
	if err != nil {
		return sandboxResourcePlan{}, "", fmt.Errorf("sandbox set-resources: %w", err)
	}
	return plan, copied, nil
}

// planSandboxResources builds the profile that edits would leave and checks it.
// A profile a seat runs is checked with its defaults applied, as config.Seats
// checks it; any other profile as it stands, as the loader checks it.
func planSandboxResources(ts *config.TownSettings, name string, edits []sandboxResourceEdit) (sandboxResourcePlan, error) {
	before, ok := ts.Sandboxes[name]
	if !ok {
		return sandboxResourcePlan{}, fmt.Errorf("%q is not a sandbox profile: the profiles are %s", name, sandboxProfileNames(ts))
	}
	after := before
	after.Requests = cloneSandboxResources(before.Requests)
	after.Limits = cloneSandboxResources(before.Limits)
	for _, e := range edits {
		block := &after.Requests
		if e.Block == "limits" {
			block = &after.Limits
		}
		if *block == nil {
			*block = &config.SandboxResources{}
		}
		switch e.Field {
		case "cpu":
			(*block).CPU = e.Value
		case "memory":
			(*block).Memory = e.Value
		}
	}
	if sameSandboxResources(before.Requests, after.Requests) && sameSandboxResources(before.Limits, after.Limits) {
		return sandboxResourcePlan{}, fmt.Errorf("nothing to change: sandboxes.%s already has these requests and limits", name)
	}
	check := after
	if sandboxProfileSeated(ts, name) {
		check = after.WithDefaults()
	}
	if err := check.Validate(name); err != nil {
		return sandboxResourcePlan{}, err
	}
	return sandboxResourcePlan{before: before, after: after}, nil
}

// printSandboxResourceChange prints the before and after of the cpu and memory
// of both blocks, unset as "(unset)".
func printSandboxResourceChange(w io.Writer, name string, p sandboxResourcePlan) {
	for _, block := range []struct {
		label string
		b, a  *config.SandboxResources
	}{
		{"requests", p.before.Requests, p.after.Requests},
		{"limits", p.before.Limits, p.after.Limits},
	} {
		fmt.Fprintf(w, "sandboxes.%s.%s.cpu: %s -> %s\n", name, block.label, sandboxQuantityShown(resourceField(block.b, "cpu")), sandboxQuantityShown(resourceField(block.a, "cpu")))
		fmt.Fprintf(w, "sandboxes.%s.%s.memory: %s -> %s\n", name, block.label, sandboxQuantityShown(resourceField(block.b, "memory")), sandboxQuantityShown(resourceField(block.a, "memory")))
	}
}

// resourceField is the named field of r, or "" when r is nil.
func resourceField(r *config.SandboxResources, field string) string {
	if r == nil {
		return ""
	}
	if field == "cpu" {
		return r.CPU
	}
	return r.Memory
}

// sandboxQuantityShown is a quantity as printed, "(unset)" when it is empty.
func sandboxQuantityShown(q string) string {
	if q == "" {
		return "(unset)"
	}
	return q
}

// cloneSandboxResources copies r, so an edit cannot write through to the
// profile the caller loaded.
func cloneSandboxResources(r *config.SandboxResources) *config.SandboxResources {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

// sameSandboxResources reports whether a and b name the same quantities; a nil
// block is the same as an empty one.
func sameSandboxResources(a, b *config.SandboxResources) bool {
	var av, bv config.SandboxResources
	if a != nil {
		av = *a
	}
	if b != nil {
		bv = *b
	}
	return av == bv
}

// sandboxProfileSeated reports whether a polecat seat runs the named profile.
func sandboxProfileSeated(ts *config.TownSettings, name string) bool {
	if ts.PolecatPool == nil {
		return false
	}
	for _, seat := range ts.PolecatPool.Seats {
		if seat.Sandbox == name {
			return true
		}
	}
	return false
}

// sandboxProfileNames names a town's sandbox profiles for a refusal.
func sandboxProfileNames(ts *config.TownSettings) string {
	names := make([]string, 0, len(ts.Sandboxes))
	for name := range ts.Sandboxes {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
