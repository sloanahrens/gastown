package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/models"
	"github.com/steveyegge/gastown/internal/workspace"
)

// gt model reads and sets which model each role runs. The town's model table
// (settings/models.json) says which profile each role should run; a write puts
// that profile in the live config — role_agents and default_agent in
// settings/config.json, the backend in the reviewer's own config — and reads it
// back, so the answer to "is it live" is in the output rather than in a second
// command (gt-jaagc).
var modelCmd = &cobra.Command{
	Use:     "model",
	GroupID: GroupConfig,
	Short:   "Show and switch the model each role runs",
	Long: `Show and switch the model each role runs.

A profile names an agent preset — the value written to role_agents and
default_agent — and, for the reviewer, a model or a whole backend command. The
builtin profiles are haiku, sonnet and opus; a town adds its own in
settings/models.json. Any role may run any profile: a switch is one step, with
no approval.`,
	RunE: requireSubcommand,
}

var modelShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print each role's profile, preset, live setting and drift",
	// A refusal here is about the town's files, not this command's arguments.
	SilenceUsage: true,
	Long: `Print each role's profile from settings/models.json beside the live setting it
should match: role_agents.<role> for a role, default_agent for the default, and
the backend's model for om-review, which runs on the reviewer's own config.

The DRIFT column names the table's value where the live setting differs, and the
command exits 4 then, so a check can run it without parsing the table:

  gt model show || echo the town is not running its table

A role no role_agents key carries — deep review, and the retired polecat — has
no live value to match: LIVE names which kind of row it is, and a key left for
one is drift.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		townRoot, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		err = runModelShow(cmd.OutOrStdout(), townModelEnv(townRoot, omConfigPath()))
		if _, ok := IsSilentExit(err); ok {
			// The exit code is the answer; cobra must not print it as an error.
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
		}
		return err
	},
}

var modelSetRoleCmd = &cobra.Command{
	Use:   "set-role <role> <profile>",
	Short: "Switch one role to a profile and make the live config run it",
	// A refusal here is about the town's files, not this command's arguments.
	SilenceUsage: true,
	Long: `Set one role's profile in settings/models.json and make the live config run it,
in one step, then read the live setting back.

For a role the live setting is role_agents.<role>; for om-review it is the om
config's backend, whose model is swapped in place so the operator's command
keeps its path and flags. Every other key of both files is left as it was, so a
switch for one role cannot change another role's model or a polecat's seat. A
role no role_agents key carries — deep review, and the retired polecat — is
refused as the target, because nothing would run it.

  gt model set-role om-review haiku
  gt model set-role crew sonnet

The previous copy of each file is kept beside it as <file>.bak-<timestamp>;
the newest 30 and the last 7 days are kept.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		townRoot, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		return runModelSetRole(cmd.OutOrStdout(), townModelEnv(townRoot, omConfigPath()), args[0], args[1])
	},
}

var modelSetDefaultCmd = &cobra.Command{
	Use:   "set-default <profile>",
	Short: "Set the profile every agent with no role of its own runs",
	// A refusal here is about the town's files, not this command's arguments.
	SilenceUsage: true,
	Long: `Set settings/models.json's default profile and settings/config.json's
default_agent, then read default_agent back. Every role with no role_agents
entry of its own runs it; a role the table names keeps the profile it names.

  gt model set-default sonnet`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		townRoot, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		return runModelSetDefault(cmd.OutOrStdout(), townModelEnv(townRoot, ""), args[0])
	},
}

var modelApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Write every role of the model table to the live config",
	// A refusal here is about the town's files, not this command's arguments.
	SilenceUsage: true,
	Long: `Write settings/models.json to the live config in one pass: role_agents for each
role it names, default_agent for its default, and the om config's backend for
om-review.

The whole table is validated before anything is written, so an unknown role, a
missing profile or a profile with no preset leaves every file as it was. A
role_agents key the table does not name is left alone, and a role no key carries
— deep review, and the retired polecat — is skipped and named in the output. The
table is printed after the write with the live values read back.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		townRoot, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		return runModelApply(cmd.OutOrStdout(), townModelEnv(townRoot, omConfigPath()))
	},
}

func init() {
	modelCmd.AddCommand(modelShowCmd, modelSetRoleCmd, modelSetDefaultCmd, modelApplyCmd)
	rootCmd.AddCommand(modelCmd)
}

// modelPaths are the files gt model reads and writes.
type modelPaths struct {
	townRoot string
	table    string
	settings string
	om       string
}

func modelPathsFor(townRoot, omPath string) modelPaths {
	return modelPaths{
		townRoot: townRoot,
		table:    models.TablePath(townRoot),
		settings: config.TownSettingsPath(townRoot),
		om:       omPath,
	}
}

// modelEnv is what a model command reads the world through: a town's files and
// the resolver that says whether a preset names an agent this town runs. A test
// builds its own resolver, so its result does not depend on which agents the
// machine running the test resolves (gt-jaagc).
type modelEnv struct {
	paths  modelPaths
	agents func(preset, townRoot string) bool
}

// townModelEnv is a town's own files and resolver.
func townModelEnv(townRoot, omPath string) modelEnv {
	return modelEnv{paths: modelPathsFor(townRoot, omPath), agents: modelAgentKnown}
}

// modelAgentKnown reports whether preset names an agent this town resolves. A
// town with no settings file declares no agent of its own, and passing it to the
// resolver would have the resolver create the file this read must not write
// (gt-jaagc).
func modelAgentKnown(preset, townRoot string) bool {
	if ts, err := config.LoadTownSettings(config.TownSettingsPath(townRoot)); err == nil && ts == nil {
		townRoot = ""
	}
	return config.ResolveAgentConfigByName(preset, townRoot, "") != nil
}

// modelBuiltinProfiles are the profiles gt ships, named for the model they run,
// so a switch to a cheaper model needs no hand edit of the table. A town's entry
// of the same name wins, and the town supplies any family gt does not ship
// (gt-jaagc).
//
// opus names the claude preset, which is the CLI's own default model; the
// cost-tier presets are the only model-specific ones gt ships (config.Agents).
func modelBuiltinProfiles() map[string]models.Profile {
	return map[string]models.Profile{
		"haiku":  {Preset: "claude-haiku", Model: "haiku"},
		"sonnet": {Preset: "claude-sonnet", Model: "sonnet"},
		"opus":   {Preset: "claude", Model: "opus"},
	}
}

// label names a file relative to the town root, as the rest of gt's messages do.
func (p modelPaths) label(path string) string { return config.SourcePathRel(p.townRoot, path) }

// backupStamp names a backup to the nanosecond, so two switches inside one
// second leave two copies and the older file survives (gt-jaagc).
func backupStamp(now time.Time) string { return now.UTC().Format("20060102T150405.000000000Z") }

// modelRow is one line of gt model show: what the table says a role should run,
// what the live config runs, and the drift between them.
type modelRow struct {
	role    string
	profile string
	preset  string
	live    string
	drift   string
}

// modelView is gt model show's whole reading.
type modelView struct {
	table    *models.Table
	settings *config.TownSettings
	om       models.OMConfig
	omErr    error
	rows     []modelRow
}

// readModelView reads the table, the town settings and the reviewer's config. A
// settings file that is not there is a town that set nothing, and a table that
// is not there is a town that declares nothing; neither is an error.
func readModelView(p modelPaths) (modelView, error) {
	v := modelView{}
	table, err := models.Load(p.table, modelBuiltinProfiles())
	if err != nil {
		return v, fmt.Errorf("reading %s: %w", p.label(p.table), err)
	}
	v.table = table
	ts, err := config.LoadTownSettings(p.settings)
	if err != nil {
		return v, fmt.Errorf("reading %s: %w", p.label(p.settings), err)
	}
	v.settings = ts
	// An unreadable om config is the panel's row to report, not this read's
	// failure: the roles still have a table to drift from.
	v.om, v.omErr = models.LoadOMConfig(p.om)
	v.rows = v.buildRows()
	return v, nil
}

// buildRows is every role the table or the live config names, in models.Roles'
// order, then any other role, then the default.
func (v modelView) buildRows() []modelRow {
	live := map[string]string{}
	if v.settings != nil {
		for role, agent := range v.settings.RoleAgents {
			live[role] = strings.TrimSpace(agent)
		}
	}
	shown := map[string]bool{}
	var order []string
	for _, role := range models.Roles {
		if role == models.OMReview {
			order = append(order, role)
			shown[role] = true
			continue
		}
		if _, named := v.table.Roles[role]; named || live[role] != "" {
			order = append(order, role)
			shown[role] = true
		}
	}
	// A "default" key in roles reads as a role where the default is a key of
	// its own; Validate refuses it, and the default row below prints the real
	// one (gt-jaagc).
	shown["default"] = true
	rest := map[string]bool{}
	for role := range v.table.Roles {
		rest[role] = true
	}
	for role := range live {
		rest[role] = true
	}
	extra := make([]string, 0, len(rest))
	for role := range rest {
		if !shown[role] {
			extra = append(extra, role)
		}
	}
	sort.Strings(extra)
	order = append(order, extra...)

	rows := make([]modelRow, 0, len(order)+1)
	for _, role := range order {
		if role == models.OMReview {
			rows = append(rows, v.omRow())
			continue
		}
		rows = append(rows, v.roleRow(role, live[role]))
	}
	return append(rows, v.defaultRow())
}

// roleRow is a role beside the table's profile for it. A role the table does
// not name declares nothing and cannot drift; one it names drifts until
// role_agents carries the preset exactly.
func (v modelView) roleRow(role, live string) modelRow {
	row := modelRow{role: role, live: live}
	name, named := v.table.Roles[role]
	if !named {
		return row
	}
	row.profile = name
	p, ok := v.table.ProfileOf(name)
	switch {
	case !ok:
		row.preset, row.drift = "(no such profile)", "(no such profile)"
	case p.Preset == "":
		row.preset, row.drift = "", "(no preset in profile)"
	default:
		row.preset = p.Preset
		if only, tableOnly := models.TableOnlyOf(role); tableOnly {
			// No write carries such a role to role_agents, so a key there is a
			// leftover the role does not run: drift, not a value to match
			// (gt-2ih67).
			if live == "" {
				row.live = only.Live
			} else {
				row.drift = "(stale-key)"
			}
			return row
		}
		if live != p.Preset {
			row.drift = p.Preset
		}
	}
	return row
}

// omRow is the reviewer beside the table's profile for it: the live model is the
// one the om backend runs, and a switch compares models rather than commands so
// a different path to the same model is no drift.
func (v modelView) omRow() modelRow {
	row := modelRow{role: models.OMReview}
	name, named := v.table.Roles[models.OMReview]
	var want string
	if named {
		row.profile = name
		if p, ok := v.table.ProfileOf(name); ok {
			want = p.ReviewerModel()
		} else {
			want = "(no such profile)"
		}
	}
	switch {
	case v.omErr != nil:
		row.live = "(unreadable)"
		if named {
			row.drift = "(unreadable)"
		}
	case len(v.om.Backend) == 0:
		if named {
			row.drift = orValue(want, "(no backend in profile)")
		}
	default:
		row.live = models.ModelName(v.om.Backend)
		if named && want != row.live {
			row.drift = orValue(want, "(no model in profile)")
		}
	}
	return row
}

// defaultRow is the table's default profile beside default_agent.
func (v modelView) defaultRow() modelRow {
	row := modelRow{role: "default"}
	if v.settings != nil {
		row.live = strings.TrimSpace(v.settings.DefaultAgent)
	}
	if v.table.Default == "" {
		return row
	}
	row.profile = v.table.Default
	p, ok := v.table.ProfileOf(v.table.Default)
	switch {
	case !ok:
		row.preset, row.drift = "(no such profile)", "(no such profile)"
	case p.Preset == "":
		row.drift = "(no preset in profile)"
	default:
		row.preset = p.Preset
		if row.live != p.Preset {
			row.drift = p.Preset
		}
	}
	return row
}

// drifted names the rows whose live setting does not match the table.
func (v modelView) drifted() []string {
	var out []string
	for _, r := range v.rows {
		if r.drift != "" {
			out = append(out, r.role)
		}
	}
	return out
}

// runModelShow prints the table and exits 4 when any row drifts, which is the
// check a scheduler runs before it trusts the table (gt-jaagc).
func runModelShow(w io.Writer, env modelEnv) error {
	p := env.paths
	v, err := readModelView(p)
	if err != nil {
		return err
	}
	printModelView(w, p, v)
	// A town with no table has no declared role to check the live values against,
	// so the check cannot pass: it exits 4, as drift does (gt-jaagc).
	if d := v.drifted(); len(d) > 0 || !v.table.Loaded() {
		return NewSilentExit(4)
	}
	return nil
}

// printModelView is the role table: one row per role, then the default, then the
// files the values came from.
func printModelView(w io.Writer, p modelPaths, v modelView) {
	rows := [][]string{{"ROLE", "PROFILE", "PRESET", "LIVE", "DRIFT"}}
	for _, r := range v.rows {
		rows = append(rows, []string{dash(r.role), dash(r.profile), dash(r.preset), dash(r.live), dash(r.drift)})
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], len(cell))
		}
	}
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, cell := range row {
			cells[i] = cell + strings.Repeat(" ", widths[i]-len(cell))
		}
		fmt.Fprintln(w, strings.TrimRight(strings.Join(cells, "  "), " "))
	}
	fmt.Fprintln(w)
	if v.omErr == nil && len(v.om.Backend) > 0 {
		fmt.Fprintf(w, "om backend: %s\n", strings.Join(v.om.Backend, " "))
	}
	fmt.Fprintf(w, "read: %s, %s, %s\n", p.label(p.table), p.label(p.settings), omPathLabel(p.om))
	if !v.table.Loaded() {
		fmt.Fprintf(w, "no table yet: %s is not there, so no role is declared; gt model set-role <role> <profile> writes one\n", p.label(p.table))
	}
	if v.omErr != nil {
		fmt.Fprintf(w, "%s could not be read: %v\n", omPathLabel(p.om), v.omErr)
	}
}

// runModelSetRole switches one role and applies it, then reads the live setting
// back and says what it found. A failure after the first of the two writes puts
// that file back, so the table and the live config never disagree (gt-jaagc).
func runModelSetRole(w io.Writer, env modelEnv, role, profileName string) error {
	p := env.paths
	if strings.TrimSpace(role) != role || role == "" {
		return fmt.Errorf("role %q: want a role name as gt model show prints it", role)
	}
	if only, tableOnly := models.TableOnlyOf(role); tableOnly {
		return fmt.Errorf("role %s has no role_agents setting to switch: %s", role, only.Why)
	}
	table, err := models.Load(p.table, modelBuiltinProfiles())
	if err != nil {
		return fmt.Errorf("reading %s: %w", p.label(p.table), err)
	}
	profile, ok := table.ProfileOf(profileName)
	if !ok {
		return fmt.Errorf("profile %q: the profiles are %s; a town adds its own under profiles in %s",
			profileName, table.ProfileNames(), p.label(p.table))
	}
	table.Roles[role] = profileName
	if err := table.ValidateRole(role); err != nil {
		return fmt.Errorf("set-role would write a table gt model apply refuses: %w", err)
	}

	// The live write is planned and checked before either file changes, so a
	// profile that cannot run the role writes nothing at all (gt-jaagc).
	var backend []string
	var omBefore models.OMConfig
	if role == models.OMReview {
		if omBefore, err = models.LoadOMConfig(p.om); err != nil {
			return fmt.Errorf("reading %s: %w", omPathLabel(p.om), err)
		}
		if backend, err = reviewerBackend(profileName, profile, omBefore.Backend); err != nil {
			return err
		}
	} else if err := modelPresetKnown(env, profile.Preset); err != nil {
		return err
	}

	stamp := backupStamp(time.Now())
	before := ""
	if role == models.OMReview {
		before = models.ModelName(omBefore.Backend)
	} else if ts, err := config.LoadTownSettings(p.settings); err != nil {
		return fmt.Errorf("reading %s: %w", p.label(p.settings), err)
	} else if ts != nil {
		before = strings.TrimSpace(ts.RoleAgents[role])
	}

	var writes writtenModelFiles
	tableBackup, err := config.BackupFile(p.table, stamp)
	if err != nil {
		return err
	}
	if err := table.Save(p.table); err != nil {
		return writes.fail(fmt.Errorf("writing %s: %w", p.label(p.table), err))
	}
	writes = append(writes, writtenModelFile{path: p.table, backup: tableBackup})

	liveLabel := p.label(p.settings)
	if role == models.OMReview {
		liveLabel = omPathLabel(p.om)
		written, err := writeOMBackend(p, stamp, backend)
		if err != nil {
			return writes.fail(err)
		}
		writes = append(writes, written)
		got, err := readBackOMBackend(p, backend)
		if err != nil {
			return writes.fail(err)
		}
		fmt.Fprintf(w, "%s: %s -> %s (profile %s)\n", role, orValue(before, "(unset)"), models.ModelName(got), profileName)
		fmt.Fprintf(w, "%s: roles.%s = %s\n", p.label(p.table), role, profileName)
		fmt.Fprintf(w, "%s: backend = %s (read back, runs %s)\n", liveLabel, strings.Join(got, " "), models.ModelName(got))
		return nil
	}

	settingsBackup, err := writeModelLive(p, stamp, func(ts *config.TownSettings) {
		ts.RoleAgents[role] = profile.Preset
	})
	if err != nil {
		return writes.fail(err)
	}
	writes = append(writes, writtenModelFile{path: config.WritePath(p.settings), backup: settingsBackup})
	got, err := readBackRoleAgent(p, role, profile.Preset)
	if err != nil {
		return writes.fail(err)
	}
	fmt.Fprintf(w, "%s: %s -> %s (profile %s)\n", role, orValue(before, "(unset)"), got, profileName)
	fmt.Fprintf(w, "%s: roles.%s = %s\n", p.label(p.table), role, profileName)
	fmt.Fprintf(w, "%s: role_agents.%s = %s (read back)\n", liveLabel, role, got)
	return nil
}

// reviewerBackend is the backend a profile gives the reviewer, refused when the
// profile would write a backend that runs a different model than it names.
func reviewerBackend(name string, p models.Profile, live []string) ([]string, error) {
	backend, err := p.ReviewerBackend(live)
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", name, err)
	}
	if want := p.ReviewerModel(); want != "" {
		if got := models.ModelName(backend); got != want {
			return nil, fmt.Errorf("profile %q: its om_backend runs %s, and the profile names %s", name, got, want)
		}
	}
	return backend, nil
}

// writtenModelFile is one file a command has written, and the copy it had before,
// so a later failure of the same command can put it back (gt-jaagc).
type writtenModelFile struct {
	path   string
	backup string // "" when the file did not exist before the write
}

// writtenModelFiles are the files a command has written, in the order written.
//
// A command that writes the table and the live config has two files and no
// transaction across them. Rather than leave them disagreeing when the second
// write fails, it puts every file it has written back, so a failed switch changes
// nothing and the table still says what the town runs (gt-jaagc).
type writtenModelFiles []writtenModelFile

// fail is cause, with every file written so far put back as it was.
func (ws writtenModelFiles) fail(cause error) error {
	for i := len(ws) - 1; i >= 0; i-- {
		if err := ws[i].restore(); err != nil {
			return fmt.Errorf("%w; %s was written and could not be put back: %v", cause, ws[i].path, err)
		}
	}
	if len(ws) == 0 {
		return cause
	}
	return fmt.Errorf("%w (nothing was changed: every file written before it was put back)", cause)
}

// restore puts the file back as the backup has it, or removes it when the write
// created it.
func (f writtenModelFile) restore() error {
	if f.backup == "" {
		if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := os.ReadFile(f.backup) //nolint:gosec // G304: a backup this command just wrote
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(f.backup); err == nil {
		mode = fi.Mode().Perm()
	}
	return config.ReplaceFile(f.path, data, mode)
}

// writeOMBackend replaces the reviewer's backend, keeping the keys beside it, and
// returns the write a failure can undo.
func writeOMBackend(p modelPaths, stamp string, backend []string) (writtenModelFile, error) {
	backup, err := config.BackupFile(p.om, stamp)
	if err != nil {
		return writtenModelFile{}, err
	}
	live, err := models.LoadOMConfig(p.om)
	if err != nil {
		return writtenModelFile{}, fmt.Errorf("reading %s: %w", omPathLabel(p.om), err)
	}
	if err := live.WithBackend(backend).Save(p.om); err != nil {
		return writtenModelFile{}, fmt.Errorf("writing %s: %w", omPathLabel(p.om), err)
	}
	return writtenModelFile{path: p.om, backup: backup}, nil
}

// readBackOMBackend is the backend the reviewer's config reads after the write,
// refused when it is not want: the "read back" in the output is this check.
func readBackOMBackend(p modelPaths, want []string) ([]string, error) {
	got, err := models.LoadOMConfig(p.om)
	if err != nil {
		return nil, fmt.Errorf("%s after the write: %w", omPathLabel(p.om), err)
	}
	if !models.SameCommand(got.Backend, want) {
		return nil, fmt.Errorf("%s: wrote backend %q and read back %q", omPathLabel(p.om), strings.Join(want, " "), strings.Join(got.Backend, " "))
	}
	return got.Backend, nil
}

// runModelSetDefault sets the default profile and default_agent, then reads
// default_agent back.
func runModelSetDefault(w io.Writer, env modelEnv, profileName string) error {
	p := env.paths
	table, err := models.Load(p.table, modelBuiltinProfiles())
	if err != nil {
		return fmt.Errorf("reading %s: %w", p.label(p.table), err)
	}
	profile, ok := table.ProfileOf(profileName)
	if !ok {
		return fmt.Errorf("profile %q: the profiles are %s; a town adds its own under profiles in %s",
			profileName, table.ProfileNames(), p.label(p.table))
	}
	before := ""
	if ts, err := config.LoadTownSettings(p.settings); err != nil {
		return fmt.Errorf("reading %s: %w", p.label(p.settings), err)
	} else if ts != nil {
		before = strings.TrimSpace(ts.DefaultAgent)
	}
	table.Default = profileName
	if err := table.ValidateDefault(); err != nil {
		return fmt.Errorf("set-default would write a table gt model apply refuses: %w", err)
	}
	if err := modelPresetKnown(env, profile.Preset); err != nil {
		return err
	}

	stamp := backupStamp(time.Now())
	var writes writtenModelFiles
	tableBackup, err := config.BackupFile(p.table, stamp)
	if err != nil {
		return err
	}
	if err := table.Save(p.table); err != nil {
		return writes.fail(fmt.Errorf("writing %s: %w", p.label(p.table), err))
	}
	writes = append(writes, writtenModelFile{path: p.table, backup: tableBackup})
	settingsBackup, err := writeModelLive(p, stamp, func(ts *config.TownSettings) {
		ts.DefaultAgent = profile.Preset
	})
	if err != nil {
		return writes.fail(err)
	}
	writes = append(writes, writtenModelFile{path: config.WritePath(p.settings), backup: settingsBackup})
	got, err := readBackDefaultAgent(p, profile.Preset)
	if err != nil {
		return writes.fail(err)
	}
	fmt.Fprintf(w, "default: %s -> %s (profile %s)\n", orValue(before, "(unset)"), got, profileName)
	fmt.Fprintf(w, "%s: default = %s\n", p.label(p.table), profileName)
	fmt.Fprintf(w, "%s: default_agent = %s (read back)\n", p.label(p.settings), got)
	return nil
}

// runModelApply writes the whole table to the live config: role_agents for every
// role, default_agent for the default, the om backend for the reviewer. The
// table is validated first and the write is one pass per file, then every value
// is read back; a failure after the first write puts it back (gt-jaagc).
func runModelApply(w io.Writer, env modelEnv) error {
	p := env.paths
	table, err := models.Load(p.table, modelBuiltinProfiles())
	if err != nil {
		return fmt.Errorf("reading %s: %w", p.label(p.table), err)
	}
	if !table.Loaded() {
		return fmt.Errorf("no model table: %s is not there, so there is nothing to apply; gt model set-role <role> <profile> writes one", p.label(p.table))
	}
	if err := table.Validate(); err != nil {
		return fmt.Errorf("the table this would write is invalid: %w", err)
	}

	roles := make([]string, 0, len(table.Roles))
	for role := range table.Roles {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	presets := map[string]string{}
	var backend []string
	applied := 0
	var skipped []string
	for _, role := range roles {
		profileName := table.Roles[role]
		profile, _ := table.ProfileOf(profileName)
		if role == models.OMReview {
			live, err := models.LoadOMConfig(p.om)
			if err != nil {
				return fmt.Errorf("reading %s: %w", omPathLabel(p.om), err)
			}
			if backend, err = reviewerBackend(profileName, profile, live.Backend); err != nil {
				return err
			}
			applied++
			continue
		}
		// A table-only row has no role_agents setting to write, and the retired
		// polecat key would refuse the settings file it landed in (gt-2ih67).
		if _, tableOnly := models.TableOnlyOf(role); tableOnly {
			skipped = append(skipped, role)
			continue
		}
		if err := modelPresetKnown(env, profile.Preset); err != nil {
			return fmt.Errorf("role %s: %w", role, err)
		}
		presets[role] = profile.Preset
		applied++
	}
	defaultPreset := ""
	if table.Default != "" {
		profile, _ := table.ProfileOf(table.Default)
		if err := modelPresetKnown(env, profile.Preset); err != nil {
			return fmt.Errorf("default: %w", err)
		}
		defaultPreset = profile.Preset
	}

	stamp := backupStamp(time.Now())
	var writes writtenModelFiles
	if len(presets) > 0 || defaultPreset != "" {
		settingsBackup, err := writeModelLive(p, stamp, func(ts *config.TownSettings) {
			for role, preset := range presets {
				ts.RoleAgents[role] = preset
			}
			if defaultPreset != "" {
				ts.DefaultAgent = defaultPreset
			}
		})
		if err != nil {
			return writes.fail(err)
		}
		writes = append(writes, writtenModelFile{path: config.WritePath(p.settings), backup: settingsBackup})
	}
	if len(backend) > 0 {
		written, err := writeOMBackend(p, stamp, backend)
		if err != nil {
			return writes.fail(err)
		}
		writes = append(writes, written)
		if _, err := readBackOMBackend(p, backend); err != nil {
			return writes.fail(err)
		}
	}

	// The printed table is the read-back: LIVE carries what each file says now,
	// so a write that did not land shows as drift here (gt-jaagc).
	v, err := readModelView(p)
	if err != nil {
		return writes.fail(err)
	}
	printModelView(w, p, v)
	if d := v.drifted(); len(d) > 0 {
		return writes.fail(fmt.Errorf("read back after the write: %s does not match the table", strings.Join(d, ", ")))
	}
	if len(skipped) > 0 {
		fmt.Fprintf(w, "skipped: %s have no role_agents setting of their own\n", strings.Join(skipped, ", "))
	}
	fmt.Fprintf(w, "applied: %d roles from %s, and every live setting reads back as the table\n", applied, p.label(p.table))
	return nil
}

// writeModelLive changes town settings under the file's lock, keeping a backup
// first, so two writers never lose each other's update. It returns the backup,
// which the caller restores if a later write of the same command fails (gt-jaagc).
func writeModelLive(p modelPaths, stamp string, change func(*config.TownSettings)) (string, error) {
	path := p.settings
	backup := ""
	err := config.UpdateConfigJSON(path, 0o600, func(ts *config.TownSettings, exists bool) error {
		if !exists {
			*ts = *config.NewTownSettings()
		}
		b, err := config.BackupFile(config.WritePath(path), stamp)
		if err != nil {
			return err
		}
		backup = b
		if ts.RoleAgents == nil {
			ts.RoleAgents = map[string]string{}
		}
		change(ts)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("writing %s: %w", p.label(path), err)
	}
	return backup, nil
}

// readBackRoleAgent is role_agents.<role> as the file reads after the write,
// refused when it is not want: the "read back" in the output is this check.
func readBackRoleAgent(p modelPaths, role, want string) (string, error) {
	ts, err := config.LoadTownSettings(p.settings)
	if err != nil {
		return "", fmt.Errorf("reading %s after the write: %w", p.label(p.settings), err)
	}
	got := ""
	if ts != nil {
		got = strings.TrimSpace(ts.RoleAgents[role])
	}
	if got != want {
		return "", fmt.Errorf("%s: wrote role_agents.%s = %s and read back %q", p.label(p.settings), role, want, got)
	}
	return got, nil
}

// readBackDefaultAgent is default_agent as the settings file reads after the
// write, refused when it is not want (gt-jaagc).
func readBackDefaultAgent(p modelPaths, want string) (string, error) {
	ts, err := config.LoadTownSettings(p.settings)
	if err != nil {
		return "", fmt.Errorf("reading %s back: %w", p.label(p.settings), err)
	}
	got := ""
	if ts != nil {
		got = strings.TrimSpace(ts.DefaultAgent)
	}
	if got != want {
		return "", fmt.Errorf("%s: wrote default_agent = %s and read back %q", p.label(p.settings), want, got)
	}
	return got, nil
}

// modelPresetKnown refuses a preset name no agent resolves, before it is written
// to role_agents where every spawn would warn and fall back to the default
// (gt-jaagc).
func modelPresetKnown(env modelEnv, preset string) error {
	if preset == "" {
		return fmt.Errorf("the profile names no preset to write")
	}
	if !env.agents(preset, env.paths.townRoot) {
		return fmt.Errorf("preset %q is no agent this town resolves: name a preset from gt config agents, or add it to settings/config.json", preset)
	}
	return nil
}

// dash is an empty cell, so a hole in the table is not read as a value.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// orValue is s, or fallback when s is empty.
func orValue(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// omPathLabel names the reviewer's config the way an operator writes it: relative
// to the home directory when it is under it, the path itself otherwise.
func omPathLabel(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return omPathLabelIn(home, path)
}

// omPathLabelIn is omPathLabel against home, so a caller that has one — and a test
// that makes one up — does not read the process environment (gt-jaagc).
func omPathLabelIn(home, path string) string {
	if path == "" {
		return "(no home directory)"
	}
	if home != "" {
		// A relative path is outside home only when it is ".." or starts with
		// "../": "..config" is a directory name, and a name of its own (gt-jaagc).
		if rel, relErr := filepath.Rel(home, path); relErr == nil && rel != ".." &&
			!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return path
}
