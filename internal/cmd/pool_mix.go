package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/specdispatch"
	"github.com/steveyegge/gastown/internal/workspace"
)

// gt pool mix puts the whole polecat model mix on one line of one command: the
// pool seat, the second seat, the pro seat and max_total, plus
// role_agents.polecat and the polecat role of the model table gt-models.sh
// reads. Setting the mix key by key is six commands with three traps in them
// (gt-lwlva): gt-models.sh set-role polecat follow rewrites the pool seat as a
// side effect, gt pool set refuses an agent the pool seat no longer holds, and
// a second seat naming the pool seat's agent is silently not run. The run
// refuses to leave work pinned to an agent the mix gives no seat, which is how
// the deep reviews pinned to agent:claude-sonnet were stranded without a Sonnet
// seat.

const poolMixMaxAgents = 2

var (
	poolMixDryRun     bool
	poolMixKeepPinned bool
)

var poolMixCmd = &cobra.Command{
	Use:   "mix <agent>=<n> [<agent>=<n>]",
	Short: "Set the polecat seats, the total cap and the polecat role in one write",
	Long: `Set the whole polecat model mix in one write. The first agent takes the pool
seat (polecat_pool.agent, max_seats) and the second the second seat
(spec_dispatch hooked_agent, max_hooked); the town runs one seat per agent, so
the two agents must differ. A one-agent mix closes the second seat.

gt pool mix deepseek-flash=2 claude-sonnet=2     two seats, two agents
gt pool mix deepseek-flash=4                     four flash, no second seat
gt pool mix claude-sonnet=4                      four sonnet, no second seat

max_total becomes the mix's total, role_agents.polecat becomes the agent the
mix leaves first open (a polecat spawned on the role rather than a seat runs
it), and the pro seat closes unless --keep-pinned reserves it. role_agents and
the models table's polecat role are written together, so gt-models.sh show reads
the mix as the town's model rather than as DRIFT.

<agent> must be a known preset; gt config agent list names them. A count of 0
closes that agent's seat.

Work pinned to an agent — a scheduled_slings entry naming one, or an open bead
carrying an agent:<name> label — that the mix gives no seat fails the run with
the command that would seat it: name that agent in the mix, or pass
--keep-pinned to reserve the pro seat for it (pro_agent that agent, pro_max 1,
pro_label agent:<name>, max_total one higher).

--dry-run prints the seats the mix would leave and the keys it would write,
and writes nothing. The pool seat and the pro seat take effect at the
dispatcher's next tick; the second seat is read when the daemon starts, so a
change to it needs a kickstart, which the run says. Each file is written
atomically and the previous copy is kept beside it as <file>.bak-<timestamp>;
keys gt pool does not own are left as they are.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		mix, err := parsePoolMix(args)
		if err != nil {
			return err
		}
		townRoot, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		opts := poolMixOptions{dryRun: poolMixDryRun, keepPinned: poolMixKeepPinned}
		return runPoolMix(cmd.OutOrStdout(), townRoot, mix, opts)
	},
}

func init() {
	poolMixCmd.Flags().BoolVar(&poolMixDryRun, "dry-run", false, "Print the seats and the keys the mix would write, and write nothing")
	poolMixCmd.Flags().BoolVar(&poolMixKeepPinned, "keep-pinned", false, "Reserve the pro seat for a pinned agent the mix gives no seat")
	poolCmd.AddCommand(poolMixCmd)
}

// poolMixEntry is one agent a mix names and the number of seats it is given.
type poolMixEntry struct {
	agent string
	seats int
}

// poolMixOptions is one run's flags.
type poolMixOptions struct {
	dryRun     bool
	keepPinned bool
}

// parsePoolMix reads the mix arguments. Two agents is the ceiling: the town has
// two seats besides the reserved pro seat, so a third name would be describing
// a seat that does not exist.
func parsePoolMix(args []string) ([]poolMixEntry, error) {
	if len(args) > poolMixMaxAgents {
		return nil, fmt.Errorf("a mix names at most %d agents (the town runs one seat per agent); %s name %d", poolMixMaxAgents, strings.Join(args, " "), len(args))
	}
	mix := make([]poolMixEntry, 0, len(args))
	seen := map[string]bool{}
	for _, arg := range args {
		agent, count, ok := strings.Cut(arg, "=")
		agent, count = strings.TrimSpace(agent), strings.TrimSpace(count)
		if !ok || agent == "" {
			return nil, fmt.Errorf("mix %q: want <agent>=<n>, as in gt pool mix deepseek-flash=2 claude-sonnet=2", arg)
		}
		n, err := strconv.Atoi(count)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("mix %q: %q is not a number of seats, 0 or more (0 closes that agent's seat)", arg, count)
		}
		if seen[agent] {
			return nil, fmt.Errorf("mix names %s twice: one entry per agent", agent)
		}
		seen[agent] = true
		mix = append(mix, poolMixEntry{agent: agent, seats: n})
	}
	return mix, nil
}

// poolMixArgs renders a mix the way it is typed, for a refusal that names the
// command to run instead.
func poolMixArgs(mix []poolMixEntry) string {
	parts := make([]string, 0, len(mix))
	for _, e := range mix {
		parts = append(parts, fmt.Sprintf("%s=%d", e.agent, e.seats))
	}
	return strings.Join(parts, " ")
}

// poolMixInputs is the town a mix plans against: the configs it rewrites and
// the agents the town has pinned work for.
type poolMixInputs struct {
	townRoot string
	ts       *config.TownSettings
	pc       *config.DaemonPatrolConfig
	table    *poolMixModelTable
	pinned   []string
}

// poolMixPlan is a mix applied, in memory, to a copy of the seats: every value
// the run writes, the keys it names, and whether the daemon has to be
// kickstarted for the second seat to take effect. ts and sd are the plan's own
// copies holding the result, so a dry run renders the page the write would
// leave behind without touching the file's own picture.
type poolMixPlan struct {
	poolAgent      string
	poolSeats      int
	secondAgent    string
	secondAgentSet bool
	secondSeats    int
	// proAgent is the agent the reserved pro seat runs; empty closes the seat,
	// which is what a mix with no pinned work to seat leaves behind.
	proAgent   string
	proSeats   int
	maxTotal   int
	roleAgent  string  // role_agents.polecat; empty leaves the key alone
	modelTable bool    // whether the models table's polecat role is written
	modelRole  *string // its value; nil is the table's null, "follow the default"
	kickstart  bool
	notes      []string
	ts         *config.TownSettings
	sd         *config.SpecDispatchConfig
}

// planPoolMix applies mix to a copy of the town's seats and reports what the
// run would write. It writes nothing: every refusal a mix can earn is decided
// here, before the first file is opened.
func planPoolMix(mix []poolMixEntry, opts poolMixOptions, in poolMixInputs) (poolMixPlan, error) {
	for _, e := range mix {
		if err := checkKnownAgent(in.townRoot, e.agent); err != nil {
			return poolMixPlan{}, err
		}
	}
	var sd *config.SpecDispatchConfig
	if in.pc != nil && in.pc.Patrols != nil {
		sd = in.pc.Patrols.SpecDispatch
	}
	secondAgent, secondCap := "", 0
	if sd != nil {
		secondAgent, secondCap = sd.HookedAgent, sd.MaxHooked
	}

	ts, resSD := poolMixCopies(in.ts, sd)
	plan := poolMixPlan{
		ts:          ts,
		sd:          resSD,
		poolAgent:   mix[0].agent,
		poolSeats:   mix[0].seats,
		maxTotal:    mix[0].seats,
		roleAgent:   poolMixRoleAgent(mix),
		secondAgent: secondAgent,
		secondSeats: secondCap,
	}
	if len(mix) > 1 {
		plan.secondAgent, plan.secondAgentSet = mix[1].agent, true
		plan.secondSeats = mix[1].seats
		plan.maxTotal += mix[1].seats
	}

	seated := map[string]bool{}
	if plan.poolSeats > 0 {
		seated[plan.poolAgent] = true
	}
	if plan.secondSeats > 0 {
		seated[plan.secondAgent] = true
	}
	missing := poolMixUnseatedPins(in.pinned, seated)
	switch {
	case len(missing) == 0:
		// The pro seat is the reserved one: with nothing to reserve it for,
		// the mix closes it, so the seats it leaves are the mix and no more.
	case len(missing) == 1 && opts.keepPinned:
		plan.proAgent, plan.proSeats = missing[0], 1
		plan.maxTotal++
	case len(missing) == 1:
		return poolMixPlan{}, fmt.Errorf("work is pinned to %s, which this mix gives no seat: run `gt pool mix %s --keep-pinned` to reserve the pro seat for it (pro_agent %s, pro_max 1, pro_label %s%s), or name it in the mix",
			missing[0], poolMixArgs(mix), missing[0], specdispatch.AgentLabelPrefix, missing[0])
	default:
		return poolMixPlan{}, fmt.Errorf("work is pinned to %s and the town seats two agents plus one reserved pro seat: run `%s` to seat two of them, and re-point the other pins (a scheduled_slings entry's agent, a bead's %s<name> label) at an agent the mix seats",
			strings.Join(missing, ", "), poolMixSeatSuggestion(mix, missing), specdispatch.AgentLabelPrefix)
	}

	// The seats the plan leaves, applied to the copies the report renders and
	// applyPoolMix re-applies to each file's own copy.
	setPoolSeat(ts, plan.poolAgent, plan.poolSeats)
	setProSeat(ts, plan.proSeats)
	if plan.proSeats > 0 {
		ts.PolecatPool.ProAgent = plan.proAgent
		ts.PolecatPool.ProLabel = specdispatch.AgentLabelPrefix + plan.proAgent
	}
	setMaxTotal(ts, plan.maxTotal)
	if plan.roleAgent != "" {
		setRoleAgent(ts, constants.RolePolecat, plan.roleAgent)
	}
	if plan.sd != nil {
		if plan.secondAgentSet {
			plan.sd.HookedAgent = plan.secondAgent
		}
		plan.sd.MaxHooked = plan.secondSeats
	} else if plan.secondAgentSet {
		plan.sd = &config.SpecDispatchConfig{HookedAgent: plan.secondAgent, MaxHooked: plan.secondSeats}
	}

	// The second seat is read when the daemon starts, so only a change to it
	// asks for a kickstart; the other seats are read every tick. An agent
	// recorded under a closed seat is not a change: the dispatcher runs no seat
	// to read it from.
	finalAgent := secondAgent
	if plan.secondAgentSet {
		finalAgent = plan.secondAgent
	}
	plan.kickstart = plan.secondSeats != secondCap || (plan.secondSeats > 0 && finalAgent != secondAgent)

	plan.modelTable, plan.modelRole, plan.notes = poolMixPlanModelRole(in.table, plan.roleAgent)
	// The mix owns max_total, so a town at a cap of its own is told the mix is
	// what moved it rather than left to find the new number on the page.
	if before := in.ts.PolecatPool.GetMaxTotal(); before > 0 && before != plan.maxTotal {
		plan.notes = append(plan.notes, fmt.Sprintf("max_total was %d and this mix makes it %d", before, plan.maxTotal))
	}
	return plan, nil
}

// poolMixCopies is the town settings and the second seat as the plan mutates
// them: copies, so planning a mix never rewrites the picture the caller loaded.
func poolMixCopies(ts *config.TownSettings, sd *config.SpecDispatchConfig) (*config.TownSettings, *config.SpecDispatchConfig) {
	out := *ts
	if ts.PolecatPool != nil {
		pool := *ts.PolecatPool
		out.PolecatPool = &pool
	}
	roles := make(map[string]string, len(ts.RoleAgents)+1)
	for role, agent := range ts.RoleAgents {
		roles[role] = agent
	}
	out.RoleAgents = roles
	var outSD *config.SpecDispatchConfig
	if sd != nil {
		second := *sd
		outSD = &second
	}
	return &out, outSD
}

// poolMixRoleAgent is the agent role_agents.polecat takes: the first agent the
// mix leaves a seat open for, which is the pool seat's agent whenever the pool
// seat is open. Empty when the mix leaves no seat open, and then the key is
// left as the file holds it.
func poolMixRoleAgent(mix []poolMixEntry) string {
	for _, e := range mix {
		if e.seats > 0 {
			return e.agent
		}
	}
	return ""
}

// poolMixUnseatedPins is the agents the town has pinned work for that the mix
// leaves no seat open for, sorted. A closed seat is no seat: the dispatcher
// runs a seat only above 0.
func poolMixUnseatedPins(pinned []string, seated map[string]bool) []string {
	var out []string
	for _, agent := range pinned {
		if agent != "" && !seated[agent] {
			out = append(out, agent)
		}
	}
	sort.Strings(out)
	return out
}

// poolMixSeatSuggestion is a mix that seats one more pinned agent beside the
// first agent the mix names, for a refusal that has to name a command that
// would work. The counts are the mix's own; it is the shape of the fix, not a
// recommendation.
func poolMixSeatSuggestion(mix []poolMixEntry, missing []string) string {
	seats := mix[0].seats
	if seats <= 0 {
		seats = 1
	}
	args := []string{
		fmt.Sprintf("%s=%d", mix[0].agent, seats),
		fmt.Sprintf("%s=%d", missing[0], seats),
		"--keep-pinned",
	}
	return "gt pool mix " + strings.Join(args, " ")
}

// poolMixPlanModelRole is the models table write: the profile whose preset is
// the mix's role agent, spelled null when that profile is the table's default
// (a null role follows the default, which is how the table says "the same
// agent" without naming one profile twice). No table, or no profile running
// that preset, is a note rather than a failure: the seats are still worth
// writing, and the note says what gt-models.sh show will report.
func poolMixPlanModelRole(table *poolMixModelTable, agent string) (write bool, role *string, notes []string) {
	if agent == "" {
		return false, nil, nil
	}
	if table == nil {
		return false, nil, []string{"there is no settings/models.json, so role_agents.polecat is written alone: run gt-models.sh init to give the town a model table"}
	}
	profile, ok := table.polecatRoleFor(agent)
	if !ok {
		return false, nil, []string{fmt.Sprintf("settings/models.json has no profile whose preset is %s, so gt-models.sh show will report DRIFT for role_agents.polecat until one is added", agent)}
	}
	return true, profile, nil
}

// poolMixTownReads is the town's two reads a mix makes: the open beads the
// pinned scan walks, and the occupancy pooled with the seats the page prints.
// Both are passed in the way the pool's other readers take their collaborators,
// so a test drives a mix from canned data.
type poolMixTownReads struct {
	openBeads poolMixOpenBeadsFunc
	live      func() poolShowLive
}

// poolMixReadsFor is the reads a caller in townRoot wants.
func poolMixReadsFor(townRoot string) poolMixTownReads {
	return poolMixTownReads{
		openBeads: poolMixOpenBeadsFor(townRoot),
		live:      func() poolShowLive { return poolLiveFor(townRoot) },
	}
}

// runPoolMix plans and writes one mix.
func runPoolMix(w io.Writer, townRoot string, mix []poolMixEntry, opts poolMixOptions) error {
	return runPoolMixWith(w, townRoot, mix, opts, poolMixReadsFor(townRoot))
}

// runPoolMixWith is runPoolMix with the town's reads passed in.
func runPoolMixWith(w io.Writer, townRoot string, mix []poolMixEntry, opts poolMixOptions, reads poolMixTownReads) error {
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return fmt.Errorf("loading town settings: %w", err)
	}
	pc, err := loadPoolPatrolConfig(townRoot)
	if err != nil {
		return err
	}
	table, err := loadPoolMixModelTable(townRoot)
	if err != nil {
		return err
	}
	pinned, err := poolMixPinnedAgents(townRoot, pc, reads.openBeads)
	if err != nil {
		return err
	}
	plan, err := planPoolMix(mix, opts, poolMixInputs{townRoot: townRoot, ts: ts, pc: pc, table: table, pinned: pinned})
	if err != nil {
		return err
	}
	live := poolShowLive{}
	if reads.live != nil {
		live = reads.live()
	}
	if err := renderPoolMixReport(w, townRoot, plan, opts, live); err != nil {
		return err
	}
	if opts.dryRun {
		fmt.Fprintln(w, "dry run: nothing written")
		return nil
	}
	stamp, err := applyPoolMix(townRoot, plan)
	if err != nil {
		return err
	}
	written := []string{
		config.SourcePathRel(townRoot, config.TownSettingsPath(townRoot)),
		patrolConfigLocation(townRoot),
	}
	if plan.modelTable {
		written = append(written, poolMixModelTableRel(townRoot))
	}
	fmt.Fprintf(w, "wrote %s (the previous copy of each is beside it as <file>.bak-%s)\n", strings.Join(written, ", "), stamp)
	if plan.kickstart {
		fmt.Fprintln(w, "the second seat is read when the daemon starts: kickstart the daemon for this mix to take effect")
	} else {
		fmt.Fprintln(w, "the pool seat and the pro seat take effect at the dispatcher's next tick; no daemon kickstart is needed")
	}
	return nil
}

// renderPoolMixReport prints the page a mix produces: the seats it leaves, in
// the dispatcher's own list, then the keys it writes. The dry run prints
// exactly this and writes nothing.
func renderPoolMixReport(w io.Writer, townRoot string, plan poolMixPlan, opts poolMixOptions, live poolShowLive) error {
	if err := renderPoolShow(w, townRoot, poolSeatsOf(plan.ts, plan.sd), live); err != nil {
		return err
	}
	if opts.dryRun {
		fmt.Fprintln(w, "keys this would write:")
	} else {
		fmt.Fprintln(w, "keys written:")
	}
	for _, line := range plan.keyLines() {
		fmt.Fprintln(w, "  "+line)
	}
	for _, note := range plan.notes {
		fmt.Fprintln(w, "note: "+note)
	}
	return nil
}

// keyLines names every key the run writes with the value it takes, so a dry run
// and the write it previews report the same change.
func (p poolMixPlan) keyLines() []string {
	var out []string
	if p.poolSeats > 0 {
		out = append(out,
			"polecat_pool.agent = "+p.poolAgent,
			fmt.Sprintf("polecat_pool.max_seats = %d", p.poolSeats))
	} else {
		out = append(out, "polecat_pool.agent and polecat_pool.max_seats removed (the pool seat is closed)")
	}
	if p.roleAgent != "" {
		out = append(out, "role_agents.polecat = "+p.roleAgent)
	}
	if p.maxTotal > 0 {
		out = append(out, fmt.Sprintf("polecat_pool.max_total = %d", p.maxTotal))
	} else {
		out = append(out, "polecat_pool.max_total removed (no total cap)")
	}
	if p.proSeats > 0 {
		out = append(out,
			"polecat_pool.pro_agent = "+p.proAgent,
			fmt.Sprintf("polecat_pool.pro_max = %d", p.proSeats),
			"polecat_pool.pro_label = "+specdispatch.AgentLabelPrefix+p.proAgent)
	} else {
		out = append(out, "polecat_pool.pro_max = 0 (the pro seat is closed)")
	}
	if p.secondAgentSet {
		out = append(out, "patrols.spec_dispatch.hooked_agent = "+p.secondAgent)
	}
	if p.secondSeats > 0 {
		out = append(out, fmt.Sprintf("patrols.spec_dispatch.max_hooked = %d", p.secondSeats))
	} else {
		out = append(out, "patrols.spec_dispatch.max_hooked = 0 (the second seat is closed)")
	}
	if p.modelTable {
		if p.modelRole == nil {
			out = append(out, "settings/models.json roles."+constants.RolePolecat+" = null (follows the table's default profile)")
		} else {
			out = append(out, "settings/models.json roles."+constants.RolePolecat+" = "+*p.modelRole)
		}
	}
	return out
}

// applyPoolMix writes the plan: the town settings, the patrol config's second
// seat and the models table's polecat role. Each file is written atomically
// under its own lock, with the previous copy kept beside it, and each write
// re-applies the plan's values to that file's own copy, so a key the mix does
// not own survives a change made since the plan was drawn.
func applyPoolMix(townRoot string, plan poolMixPlan) (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	err := writeTownSeats(townRoot, stamp, func(ts *config.TownSettings) error {
		setPoolSeat(ts, plan.poolAgent, plan.poolSeats)
		setProSeat(ts, plan.proSeats)
		if plan.proSeats > 0 {
			ts.PolecatPool.ProAgent = plan.proAgent
			ts.PolecatPool.ProLabel = specdispatch.AgentLabelPrefix + plan.proAgent
		}
		setMaxTotal(ts, plan.maxTotal)
		if plan.roleAgent != "" {
			setRoleAgent(ts, constants.RolePolecat, plan.roleAgent)
		}
		return ts.PolecatPool.Validate()
	})
	if err != nil {
		return "", fmt.Errorf("writing the polecat seats: %w", err)
	}
	err = writePatrolSeats(townRoot, stamp, func(pc *config.DaemonPatrolConfig) {
		sd := specDispatchOf(pc)
		if plan.secondAgentSet {
			sd.HookedAgent = plan.secondAgent
		}
		sd.MaxHooked = plan.secondSeats
	})
	if err != nil {
		return "", fmt.Errorf("writing the second seat: %w", err)
	}
	if plan.modelTable {
		if err := writePoolMixModelRole(townRoot, stamp, plan.modelRole); err != nil {
			return "", err
		}
	}
	return stamp, nil
}

// setRoleAgent points a role at an agent, making the map a file without one
// needs.
func setRoleAgent(ts *config.TownSettings, role, agent string) {
	if ts.RoleAgents == nil {
		ts.RoleAgents = make(map[string]string, 1)
	}
	ts.RoleAgents[role] = agent
}

// loadPoolPatrolConfig reads the daemon patrol config, nil when the town has
// none: a mix may be the first thing to give the town a second seat, and an
// absent file is a section to make rather than a failure.
func loadPoolPatrolConfig(townRoot string) (*config.DaemonPatrolConfig, error) {
	pc, err := config.LoadDaemonPatrolConfig(config.DaemonPatrolConfigPath(townRoot))
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return pc, nil
}

// poolMixModelTable is settings/models.json, the table gt-models.sh owns and
// appends: the profile each role should run, and what each profile is. gt pool
// mix writes roles.polecat and nothing else of it, so the table's own reader
// compares the mix's agent against a role that names it. The dashboard's
// reader (dashboard_models.go) reads the same file for the two keys its panel
// prints.
type poolMixModelTable struct {
	Version     int                       `json:"version"`
	Profiles    map[string]poolMixProfile `json:"profiles"`
	Default     string                    `json:"default"`
	Roles       map[string]*string        `json:"roles"`
	Escalations []json.RawMessage         `json:"escalations,omitempty"`
}

// poolMixProfile is one profile of the models table: the agent preset it runs.
type poolMixProfile struct {
	Preset    string   `json:"preset"`
	OMBackend []string `json:"om_backend,omitempty"`
}

// polecatRoleFor is the table's polecat role for agent: the profile whose
// preset runs it, or nil for the table's default profile, whose role is spelled
// null. found is false when no profile runs that preset, and then there is no
// role the table would read as this agent.
func (t *poolMixModelTable) polecatRoleFor(agent string) (role *string, found bool) {
	for name, profile := range t.Profiles {
		if profile.Preset != agent {
			continue
		}
		if name == t.Default {
			return nil, true
		}
		name := name
		return &name, true
	}
	return nil, false
}

// poolMixModelTablePath is the town's model table.
func poolMixModelTablePath(townRoot string) string {
	return filepath.Join(townRoot, "settings", "models.json")
}

// poolMixModelTableRel names the table the way a message does.
func poolMixModelTableRel(townRoot string) string {
	return config.SourcePathRel(townRoot, poolMixModelTablePath(townRoot))
}

// loadPoolMixModelTable reads the model table, nil when the town has none. A
// table that does not parse is a refusal before anything is written: the run
// would otherwise write the seats and then fail to keep the table in step with
// them.
func loadPoolMixModelTable(townRoot string) (*poolMixModelTable, error) {
	path := poolMixModelTablePath(townRoot)
	data, err := os.ReadFile(path) //nolint:gosec // G304: internal config path
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var table poolMixModelTable
	if err := config.DecodeJSONFile(path, data, &table); err != nil {
		return nil, err
	}
	return &table, nil
}

// writePoolMixModelRole pins the table's polecat role under the file's lock,
// keeping a backup first. A table that is gone is not created here: gt-models.sh
// init owns the table's shape, and a mix that minted one would put a second
// writer on a file the operator's model choices live in.
func writePoolMixModelRole(townRoot, stamp string, role *string) error {
	path := poolMixModelTablePath(townRoot)
	err := config.UpdateConfigJSON(path, 0o600, func(table *poolMixModelTable, exists bool) error {
		if !exists {
			return fmt.Errorf("%s is gone: gt-models.sh init is what creates the model table", path)
		}
		if err := backupConfigFile(path, stamp); err != nil {
			return err
		}
		if table.Roles == nil {
			table.Roles = make(map[string]*string, 1)
		}
		table.Roles[constants.RolePolecat] = role
		return nil
	})
	if err != nil {
		return fmt.Errorf("writing the model table's polecat role: %w", err)
	}
	return nil
}

// poolMixOpenBeadsFunc reads a rig's open beads, the board a pinned bead's
// agent:<name> label is found on.
type poolMixOpenBeadsFunc func(rigPath string) ([]*beads.Issue, error)

// poolMixOpenBeadsFor is the open-bead read a caller in townRoot wants: one bd
// query per rig, against that rig's own database.
func poolMixOpenBeadsFor(townRoot string) poolMixOpenBeadsFunc {
	return func(rigPath string) ([]*beads.Issue, error) {
		out, err := beads.RunBdJSONAllowStale(rigPath, "list", "--status=open", "--json", "--limit=0")
		if err != nil {
			return nil, err
		}
		return parseSpecReady(out)
	}
}

// poolMixPinnedAgents is the agents the town has pinned work for: the
// scheduled_slings entries that name one, and the open beads carrying an
// agent:<name> label. A mix that leaves one of them without a seat would leave
// its work undispatchable (gt-lwlva).
//
// Every registered rig with a database is walked, parked ones included: a park
// read that fails is treated as parked everywhere else, and a scan that skipped
// it would drop every pin in the town silently, which is the outcome the pinned
// half of this command exists to prevent. A park is a pause, and the beads keep
// their agent label until it ends.
func poolMixPinnedAgents(townRoot string, pc *config.DaemonPatrolConfig, openBeads poolMixOpenBeadsFunc) ([]string, error) {
	pinned := map[string]bool{}
	if pc != nil && pc.Patrols != nil && pc.Patrols.ScheduledSlings != nil {
		for _, entry := range pc.Patrols.ScheduledSlings.Entries {
			if agent := strings.TrimSpace(entry.Agent); agent != "" {
				pinned[agent] = true
			}
		}
	}
	names, err := knownRigNames(townRoot)
	if err != nil && !errors.Is(err, config.ErrNotFound) {
		return nil, fmt.Errorf("reading the rigs pinned work lives in: %w", err)
	}
	for _, name := range names {
		rigPath := filepath.Join(townRoot, name)
		if _, statErr := os.Stat(rigPath); statErr != nil {
			continue
		}
		if !hasBeadsDatabase(beads.ResolveBeadsDir(rigPath)) {
			continue
		}
		issues, err := openBeads(rigPath)
		if err != nil {
			// A board that cannot be read is not a town with no pinned work:
			// reading it as one is what loses a pinned bead its seat silently.
			return nil, fmt.Errorf("reading %s's open beads: %w", name, err)
		}
		for _, issue := range issues {
			for _, agent := range poolMixLabelAgents(issue) {
				pinned[agent] = true
			}
		}
	}
	out := make([]string, 0, len(pinned))
	for agent := range pinned {
		out = append(out, agent)
	}
	sort.Strings(out)
	return out, nil
}

// poolMixLabelAgents is the agents a bead's agent:<name> labels pin it to, the
// labels the dispatcher reads for the same purpose (specdispatch.PinnedAgents).
func poolMixLabelAgents(issue *beads.Issue) []string {
	if issue == nil {
		return nil
	}
	var out []string
	for _, label := range issue.Labels {
		agent, ok := strings.CutPrefix(label, specdispatch.AgentLabelPrefix)
		if !ok {
			continue
		}
		if agent = strings.TrimSpace(agent); agent != "" {
			out = append(out, agent)
		}
	}
	return out
}
