package cmd

import (
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

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/workspace"
)

// gt pool reads and changes the town's polecat seats. The seat map
// (polecat_pool.seats) is one entry per agent: a cap, an optional daily closed
// window and optional polecat names (gt-mukgk). gt pool show prints each seat
// with its occupancy and whether it is open; gt pool set changes only the
// agents it names and validates the whole resulting map before writing.
//
// Until the two-seat keys are retired (gt-mukgk.7), a town with no seat map
// still runs polecat_pool.agent/max_seats and spec_dispatch's hooked_agent/
// max_hooked. show prints those seats, and set seeds the map from them so a
// first set does not drop a running seat. set-second and set-total change the
// two-seat keys directly; gt pool mix is retired and prints its gt pool set
// replacement.
//
// max_total bounds live polecats across every seat. An absent max_total, like a
// written 0, is no cap.

var poolCmd = &cobra.Command{
	Use:     "pool",
	GroupID: GroupConfig,
	Short:   "Show and change the polecat seat caps",
	RunE:    requireSubcommand,
}

var poolShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print each seat with its cap, occupancy and open or closed state",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		townRoot, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		return runPoolShowAt(cmd.OutOrStdout(), townRoot, time.Now())
	},
}

var (
	poolSetTotalCap int
	poolSetRemove   []string
)

var poolSetCmd = &cobra.Command{
	Use:   "set <agent>=<cap>...",
	Short: "Set seat caps and the total, leaving the other seats alone",
	Long: `Set the cap of each named seat in polecat_pool.seats. Every other seat, and
every key gt pool does not own, keeps its value. A cap of 0 closes the seat but
keeps its window and names.

gt pool set claude-haiku=2 deepseek-flash=3
gt pool set claude-haiku=0 --remove old-model --total 8

The whole resulting map is validated with the seat loader before anything is
written, so an unknown agent, a name in two seats or a window without a timezone
fails the run and leaves the file as it was.

--total N sets polecat_pool.max_total, the cap on live polecats across every
seat; 0 removes the key. --remove <agent> drops that seat from the map and may
be repeated.

The dispatcher reads the file on every tick, so a change holds from the next
tick with no daemon restart.

A town whose file has no polecat_pool.seats still runs the two-seat keys. The
first set seeds the map from the seats the dispatcher is running, so a set on
one agent does not drop the others (gt-mukgk).

The file is written atomically and the previous copy is kept beside it as
<file>.bak-<timestamp>.`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		changes, err := parsePoolSeatCaps(args)
		if err != nil {
			return err
		}
		totalSet := cmd.Flags().Changed("total")
		if totalSet && poolSetTotalCap < 0 {
			return fmt.Errorf("total %d: want a whole number, 0 or more (0 removes the cap)", poolSetTotalCap)
		}
		if len(changes) == 0 && len(poolSetRemove) == 0 && !totalSet {
			return fmt.Errorf("nothing to change: name <agent>=<cap>, --total N or --remove <agent>")
		}
		townRoot, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		return runPoolSetSeats(cmd.OutOrStdout(), townRoot, changes, poolSetRemove, poolSetTotalCap, totalSet)
	},
}

var poolSetTotalCmd = &cobra.Command{
	Use:   "set-total <n>",
	Short: "Cap live polecats across every seat; 0 removes the cap",
	Long: `Set polecat_pool.max_total, the cap on live polecats across every seat: the
pool seat and the second seat together. It is the cap beside the per-agent
ones, for a town whose two agents are each inside their own cap and still
exceed the number of polecats wanted (5 live where 3 was intended).

n above 0 sets the cap; 0 removes the key, because an absent max_total is no
total cap and a written 0 would say the same thing. The spec dispatcher skips a
dispatch and gt sling refuses one while the town is at its cap, whichever
agents the live polecats run on.

The change takes effect at the dispatcher's next tick and on the next sling.
The file is written atomically and the previous copy is kept beside it as
<file>.bak-<timestamp>. Keys gt pool does not own are left as they are.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 0 {
			return fmt.Errorf("cap %q: want a whole number, 0 or more (0 removes the cap)", args[0])
		}
		townRoot, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		return runPoolSetTotal(cmd.OutOrStdout(), townRoot, n)
	},
}

var poolSetSecondCmd = &cobra.Command{
	Use:   "set-second <agent> <n>",
	Short: "Set the second seat's agent and cap together; 0 closes the seat",
	Long: `Set the second seat's agent (spec_dispatch hooked_agent) and cap (max_hooked)
in one write. gt pool set only moves the cap of a seat in the seat map, so this
is the verb for changing which agent a town with no seat map runs: two
claude-sonnet seats and two deepseek-flash seats is gt pool set-second
deepseek-flash 2 once the seat map replaces the two-seat keys (gt-mukgk).

<agent> must be a known preset: a built-in one, one in the town's
settings/agents.json, or a custom agent in settings/config.json (gt config agent
list names them). n above 0 sets the cap; 0 closes the seat but still records
the agent. An n above polecat_pool.max_total is refused, since the town could
never fill the seat: raise the total with gt pool set-total first.

The dispatcher runs one seat per agent, so an agent that already holds the pool
seat leaves the second seat idle; gt pool show and this command say so.

The change takes effect at the dispatcher's next tick, like the other seat:
the dispatcher reads this block on every run, so no daemon restart is needed.
The patrol config is the "daemon" section of settings/config.json on a two-file
town and mayor/daemon.json on a five-file one. It is written atomically and the
previous copy is kept beside it as <file>.bak-<timestamp>. Keys gt pool does not
own are left as they are.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 0 {
			return fmt.Errorf("cap %q: want a whole number, 0 or more (0 closes the seat)", args[1])
		}
		townRoot, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		return runPoolSetSecond(cmd.OutOrStdout(), townRoot, args[0], n)
	},
}

func init() {
	poolSetCmd.Flags().IntVar(&poolSetTotalCap, "total", 0, "Cap live polecats across every seat; 0 removes the cap")
	poolSetCmd.Flags().StringArrayVar(&poolSetRemove, "remove", nil, "Drop this agent's seat from the map; repeat for more")
	poolCmd.AddCommand(poolShowCmd, poolSetCmd, poolSetSecondCmd, poolSetTotalCmd)
	rootCmd.AddCommand(poolCmd)
}

// poolSeatKey names which of the two seats a change lands on.
type poolSeatKey int

const (
	poolSeat   poolSeatKey = iota // polecat_pool.agent, max_seats
	secondSeat                    // spec_dispatch hooked_agent, max_hooked
)

// poolSeatView is the two seats as the files hold them, and as the
// dispatcher runs them. seats is specBudgetFromConfig's own list — the call
// the dispatcher makes each tick — so show cannot print a seat the dispatcher
// would not run and set cannot name a seat the dispatcher does not give an
// agent: the defaults and the one-seat-per-agent rule are the dispatcher's,
// not a second copy's.
type poolSeatView struct {
	poolAgent string
	poolCap   int // max_seats; 0 when the key is absent
	// hookedAgent is the agent the second seat runs, spec_dispatch's default
	// when hooked_agent is unset. hookedCap is max_hooked; 0 means closed.
	// preferHooked puts that seat first in the dispatcher's list.
	hookedAgent  string
	hookedCap    int
	preferHooked bool
	seats        []poolListedSeat
	// totalCap is max_total, the cap on live polecats across every seat, and
	// totalCapSet whether the file names the key: absent and 0 both read as no
	// cap, and show prints which of them the file holds.
	totalCap    int
	totalCapSet bool
}

// poolListedSeat is one seat as show prints it: the seat it came from, the
// agent that holds it, and the cap the dispatcher will run it at.
type poolListedSeat struct {
	seat  poolSeatKey
	agent string
	cap   int
}

func poolSeatsOf(ts *config.TownSettings, sd *config.SpecDispatchConfig) poolSeatView {
	v := poolSeatView{
		hookedAgent: defaultSpecHookedAgent,
	}
	if ts != nil && ts.PolecatPool != nil {
		v.poolAgent = ts.PolecatPool.Agent
		v.poolCap = ts.PolecatPool.MaxSeats
		v.totalCap = ts.PolecatPool.GetMaxTotal()
		v.totalCapSet = ts.PolecatPool.MaxTotal != nil
	}
	if sd != nil {
		if sd.HookedAgent != "" {
			v.hookedAgent = sd.HookedAgent
		}
		v.hookedCap = sd.MaxHooked
		v.preferHooked = sd.PreferHooked
	}
	for _, s := range specBudgetFromConfig(ts, sd).Seats {
		v.seats = append(v.seats, poolListedSeat{seat: v.seatHeldBy(s.Agent), agent: s.Agent, cap: s.Cap})
	}
	return v
}

// seatHeldBy names the seat one of the dispatcher's seats came from: the first
// seat in the dispatcher's own precedence whose agent it is. Every seat
// specBudgetFromConfig builds comes from one of these two names, so the
// fallback is unreachable; the pool seat is what an unnamed seat would be.
func (v poolSeatView) seatHeldBy(agent string) poolSeatKey {
	for _, k := range v.seatPrecedence() {
		if v.agentOn(k) == agent {
			return k
		}
	}
	return poolSeat
}

// seatPrecedence is the order specBudgetFromConfig appends its seats in: the
// pool seat, then the second seat — the second seat first when prefer_hooked
// says so. An agent named twice keeps the seat that comes first (its dedup),
// so this order decides which seat a name holds, for a picture of the
// dispatcher's list and for a set on that name. The draw among free seats is
// random, so this order does not prefer a seat for dispatch.
func (v poolSeatView) seatPrecedence() []poolSeatKey {
	if v.preferHooked {
		return []poolSeatKey{secondSeat, poolSeat}
	}
	return []poolSeatKey{poolSeat, secondSeat}
}

// shadowedSecondSeat reports an open second seat the dispatcher does not run
// because its agent already holds an earlier seat: specBudgetFromConfig keeps
// one seat per agent, so the seats list omits it. Show names it anyway, so a
// second seat set to the pool's agent is not mistaken for the pool seat, or for
// a seat that is running. holder is the seat that keeps the agent.
func (v poolSeatView) shadowedSecondSeat() (holder poolSeatKey, shadowed bool) {
	if v.hookedCap <= 0 {
		return 0, false
	}
	for _, s := range v.seats {
		if s.agent == v.hookedAgent {
			return s.seat, s.seat != secondSeat
		}
	}
	return 0, false
}

// agentOn is the agent the seat runs: the empty string for a pool seat whose
// agent is unset, which no named agent can match.
func (v poolSeatView) agentOn(k poolSeatKey) string {
	switch k {
	case secondSeat:
		return v.hookedAgent
	}
	return v.poolAgent
}

func loadPoolSeats(townRoot string) (poolSeatView, error) {
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return poolSeatView{}, fmt.Errorf("loading town settings: %w", err)
	}
	sd, err := loadPoolSpecDispatch(townRoot)
	if err != nil {
		return poolSeatView{}, err
	}
	return poolSeatsOf(ts, sd), nil
}

// loadPoolSpecDispatch reads spec_dispatch the way the daemon does. Unlike
// loadSpecDispatchConfig it returns the read error: a file that does not parse
// must stop a seat change, not read as "no seats".
// loadPoolSpecDispatch is the one read of the patrol block gt pool reads and
// writes: readSpecDispatchConfig, so pool and the ticker cannot drift onto two
// readings of one file (gt-jdnab).
func loadPoolSpecDispatch(townRoot string) (*config.SpecDispatchConfig, error) {
	return readSpecDispatchConfig(townRoot)
}

// poolHasSeatMap reports whether the file uses the seat map. Until the two-seat
// keys are retired, a file without a map dispatches from those keys instead
// (specBudgetFor, gt-mukgk).
func poolHasSeatMap(ts *config.TownSettings) bool {
	return ts != nil && ts.PolecatPool != nil && len(ts.PolecatPool.Seats) > 0
}

func runPoolShow(w io.Writer, townRoot string) error {
	return runPoolShowAt(w, townRoot, time.Now())
}

// runPoolShowAt is runPoolShow with the clock the closed windows are read at.
func runPoolShowAt(w io.Writer, townRoot string, now time.Time) error {
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return fmt.Errorf("loading town settings: %w", err)
	}
	if poolHasSeatMap(ts) {
		return runPoolSeatMapShow(w, townRoot, ts, now, poolLiveFor(townRoot))
	}
	sd, err := loadPoolSpecDispatch(townRoot)
	if err != nil {
		return err
	}
	return renderPoolShow(w, townRoot, poolSeatsOf(ts, sd), poolLiveFor(townRoot))
}

// poolSeatMapView is polecat_pool.seats as show prints it: the loader's sorted
// seats, the total cap and the clock a closed window is read at.
type poolSeatMapView struct {
	seats    []config.Seat
	maxTotal *int
	timezone string
	now      time.Time
}

// poolSeatMapOf validates and sorts the file's seat map through config.Seats,
// with the known-agent check the town's load gate uses, so show cannot print a
// seat the next town start would refuse (gt-mukgk).
func poolSeatMapOf(ts *config.TownSettings, now time.Time) (poolSeatMapView, error) {
	seats, err := config.Seats(ts, config.SeatsKnownTo(ts))
	if err != nil {
		return poolSeatMapView{}, fmt.Errorf("reading the seat map: %w", err)
	}
	v := poolSeatMapView{seats: seats, now: now}
	if ts.PolecatPool != nil {
		v.maxTotal = ts.PolecatPool.MaxTotal
		v.timezone = ts.PolecatPool.Timezone
	}
	return v, nil
}

// runPoolSeatMapShow is the seat-map page with the occupancy passed in, so a
// test pins the wording against canned counts (docs/testing.md).
func runPoolSeatMapShow(w io.Writer, townRoot string, ts *config.TownSettings, now time.Time, live poolShowLive) error {
	v, err := poolSeatMapOf(ts, now)
	if err != nil {
		return err
	}
	renderPoolSeatMap(w, townRoot, v, live)
	return nil
}

// renderPoolSeatMap prints one line per agent: live over cap, then open or
// closed with the next opening, and the town's total beneath them.
func renderPoolSeatMap(w io.Writer, townRoot string, v poolSeatMapView, live poolShowLive) {
	width := len("total")
	for _, s := range v.seats {
		if len(s.Agent) > width {
			width = len(s.Agent)
		}
	}
	for _, s := range v.seats {
		fmt.Fprintf(w, "%-*s  %s  %s\n", width, s.Agent, live.seatCount(s.Agent, s.Cap), v.seatState(s))
	}
	fmt.Fprintf(w, "%-*s  %s\n", width, "total", live.totalCount(v.maxTotal))
	keys := "polecat_pool.seats, max_total"
	if v.timezone != "" {
		keys += ", timezone " + v.timezone
	}
	fmt.Fprintf(w, "read: %s (%s)\n", config.SourcePathRel(townRoot, config.TownSettingsPath(townRoot)), keys)
}

// seatCount is a seat's occupancy. A picture that could not be read is no
// count, so the line names the cap alone rather than inventing a zero (gt-bc9b6).
func (l poolShowLive) seatCount(agent string, cap int) string {
	if !l.ok {
		return fmt.Sprintf("cap %d", cap)
	}
	return fmt.Sprintf("%d/%d", l.live[agent], cap)
}

// totalCount is the town's occupancy against max_total, which may be absent.
func (l poolShowLive) totalCount(max *int) string {
	cap := 0
	if max != nil {
		cap = *max
	}
	switch {
	case l.ok && cap > 0:
		return fmt.Sprintf("%d/%d", l.total, cap)
	case l.ok:
		return fmt.Sprintf("%d live", l.total)
	case cap > 0:
		return fmt.Sprintf("cap %d", cap)
	default:
		return "no cap"
	}
}

// seatState is open, closed by a cap of 0, or closed by the window with the
// next opening printed so an operator knows when work will move.
func (v poolSeatMapView) seatState(s config.Seat) string {
	if s.Cap == 0 {
		return "closed (cap 0)"
	}
	if s.Window != nil && s.Window.Closed(v.now) {
		return "closed until " + s.Window.OpensAt(v.now).Format("15:04")
	}
	return "open"
}

// poolSeatCap is one <agent>=<cap> pair of gt pool set.
type poolSeatCap struct {
	Agent string
	Cap   int
}

// parsePoolSeatCaps reads the set arguments. Every argument is a pair, so a
// mistyped one is refused rather than read as a seat name.
func parsePoolSeatCaps(args []string) ([]poolSeatCap, error) {
	out := make([]poolSeatCap, 0, len(args))
	seen := map[string]bool{}
	for _, arg := range args {
		agent, count, ok := strings.Cut(arg, "=")
		agent = strings.TrimSpace(agent)
		if !ok || agent == "" {
			return nil, fmt.Errorf("set %q: want <agent>=<cap>, as in gt pool set claude-haiku=2", arg)
		}
		n, err := strconv.Atoi(strings.TrimSpace(count))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("cap %q: want a whole number, 0 or more (0 closes the seat)", count)
		}
		if seen[agent] {
			return nil, fmt.Errorf("set names %s twice", agent)
		}
		seen[agent] = true
		out = append(out, poolSeatCap{Agent: agent, Cap: n})
	}
	return out, nil
}

// runPoolSetSeats writes the named changes to polecat_pool.seats and
// max_total. The whole resulting map is validated before the write, so a
// refusal leaves the file as it was.
func runPoolSetSeats(w io.Writer, townRoot string, changes []poolSeatCap, remove []string, total int, totalSet bool) error {
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return fmt.Errorf("loading town settings: %w", err)
	}
	removing := map[string]bool{}
	for _, agent := range remove {
		if agent == "" {
			return fmt.Errorf("--remove: agent name is empty")
		}
		removing[agent] = true
	}
	for _, c := range changes {
		if removing[c.Agent] {
			return fmt.Errorf("%s is both set and removed", c.Agent)
		}
	}
	needSeats := len(changes) > 0 || len(remove) > 0
	seats, maxTotal, err := poolSeatMapForSet(townRoot, ts, needSeats)
	if err != nil {
		return err
	}
	before := copyPoolSeatMap(seats)
	for _, agent := range remove {
		if _, ok := seats[agent]; !ok {
			return fmt.Errorf("%q is not a seat: the seats are %s", agent, poolSeatNames(seats))
		}
		delete(seats, agent)
	}
	// An empty map hands the town back to the two-seat keys it is about to
	// replace; a cap of 0 closes the same seat without that detour (gt-mukgk).
	if len(remove) > 0 && len(seats) == 0 {
		legacy, err := poolLegacySeatsInForce(townRoot, ts)
		if err != nil {
			return err
		}
		if legacy {
			return fmt.Errorf("removing every seat would hand dispatch back to the two-seat keys: set the last seat's cap to 0 instead")
		}
	}
	for _, c := range changes {
		seat := seats[c.Agent]
		seat.Cap = c.Cap
		seats[c.Agent] = seat
	}
	if totalSet {
		if total == 0 {
			maxTotal = nil
		} else {
			n := total
			maxTotal = &n
		}
	}
	if err := validatePoolSeatMap(ts, seats, maxTotal); err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	err = writeTownSeats(townRoot, stamp, func(ts *config.TownSettings) error {
		if ts.PolecatPool == nil {
			ts.PolecatPool = &config.PolecatPool{}
		}
		ts.PolecatPool.Seats = seats
		if totalSet {
			setMaxTotal(ts, total)
		}
		return ts.PolecatPool.Validate()
	})
	if err != nil {
		return fmt.Errorf("writing the seat map: %w", err)
	}
	printPoolSeatChanges(w, before, changes, remove, total, totalSet)
	return nil
}

// poolSeatMapForSet is the map a set starts from. A file with a map gives it up
// wholesale. A file without one still runs the two-seat keys, so the first set
// seeds the map from the seats the dispatcher runs: a set on one agent must not
// drop the others (gt-mukgk).
func poolSeatMapForSet(townRoot string, ts *config.TownSettings, needSeats bool) (map[string]config.SeatConfig, *int, error) {
	var seats map[string]config.SeatConfig
	var maxTotal *int
	if ts.PolecatPool != nil {
		maxTotal = ts.PolecatPool.MaxTotal
		if len(ts.PolecatPool.Seats) > 0 {
			seats = copyPoolSeatMap(ts.PolecatPool.Seats)
		}
	}
	if !needSeats || seats != nil {
		return seats, maxTotal, nil
	}
	sd, err := loadPoolSpecDispatch(townRoot)
	if err != nil {
		return nil, nil, err
	}
	seats = map[string]config.SeatConfig{}
	for _, s := range specBudgetFromConfig(ts, sd).Seats {
		seats[s.Agent] = config.SeatConfig{Cap: s.Cap}
	}
	return seats, maxTotal, nil
}

// poolLegacySeatsInForce reports whether the file still runs the two seats the
// map replaces. It reads the patrol config only when a caller needs the answer,
// which is the one write that would empty the map.
func poolLegacySeatsInForce(townRoot string, ts *config.TownSettings) (bool, error) {
	sd, err := loadPoolSpecDispatch(townRoot)
	if err != nil {
		return false, err
	}
	return poolLegacySeatsSet(ts, sd), nil
}

// copyPoolSeatMap copies a seat map, names included, so a plan cannot rewrite
// the entries the caller loaded.
func copyPoolSeatMap(in map[string]config.SeatConfig) map[string]config.SeatConfig {
	out := make(map[string]config.SeatConfig, len(in))
	for agent, seat := range in {
		if seat.Names != nil {
			seat.Names = append([]string(nil), seat.Names...)
		}
		out[agent] = seat
	}
	return out
}

// poolLegacySeatsSet reports whether the file still configures the two seats
// the map replaces.
func poolLegacySeatsSet(ts *config.TownSettings, sd *config.SpecDispatchConfig) bool {
	if ts != nil && ts.PolecatPool != nil && ts.PolecatPool.Agent != "" {
		return true
	}
	return sd != nil && (sd.HookedAgent != "" || sd.MaxHooked > 0)
}

// validatePoolSeatMap runs the resulting map through the dispatcher's loader.
func validatePoolSeatMap(ts *config.TownSettings, seats map[string]config.SeatConfig, maxTotal *int) error {
	probe := *ts
	pool := config.PolecatPool{}
	if ts.PolecatPool != nil {
		pool = *ts.PolecatPool
	}
	pool.Seats, pool.MaxTotal = seats, maxTotal
	probe.PolecatPool = &pool
	if _, err := config.Seats(&probe, config.SeatsKnownTo(&probe)); err != nil {
		return fmt.Errorf("the seat map this would write is invalid: %w", err)
	}
	return nil
}

// poolSeatNames names a map's seats for a refusal.
func poolSeatNames(seats map[string]config.SeatConfig) string {
	names := make([]string, 0, len(seats))
	for agent := range seats {
		names = append(names, agent)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// printPoolSeatChanges reports each write, then when it lands. The dispatcher
// reads the file every tick, so nothing has to be restarted.
func printPoolSeatChanges(w io.Writer, before map[string]config.SeatConfig, changes []poolSeatCap, remove []string, total int, totalSet bool) {
	for _, agent := range remove {
		fmt.Fprintf(w, "removed %s\n", agent)
	}
	for _, c := range changes {
		if _, existed := before[c.Agent]; existed {
			fmt.Fprintf(w, "%s: cap %d\n", c.Agent, c.Cap)
		} else {
			fmt.Fprintf(w, "%s: cap %d (new seat)\n", c.Agent, c.Cap)
		}
	}
	if totalSet {
		if total == 0 {
			fmt.Fprintln(w, "total cap removed (max_total)")
		} else {
			fmt.Fprintf(w, "total: cap %d live polecats across every seat\n", total)
		}
	}
	fmt.Fprintln(w, "takes effect at the dispatcher's next tick; no restart is needed")
}

// renderPoolShow prints one page: each open seat with its agent and cap, the
// occupancy beside it, and the town's total. The occupancy is a parameter rather
// than a read here so the page's wording can be pinned against a seat picture a
// test builds (docs/testing.md).
func renderPoolShow(w io.Writer, townRoot string, v poolSeatView, live poolShowLive) error {
	for _, s := range v.seats {
		fmt.Fprintf(w, "%-13s%s  %s%s\n", seatName(s.seat), s.agent, v.capText(s), live.seatNote(s.agent, s.cap))
	}
	if holder, ok := v.shadowedSecondSeat(); ok {
		fmt.Fprintf(w, "%-13s%s  cap %d, not run: the %s already holds %s (one seat per agent)\n",
			seatName(secondSeat), v.hookedAgent, v.hookedCap, seatName(holder), v.hookedAgent)
	}
	fmt.Fprintf(w, "%-13s%s%s\n", "total", v.totalText(), live.totalNote(v.totalCap))
	if len(v.seats) == 0 {
		fmt.Fprintln(w, "no open seat: polecat_pool has no agent, and spec_dispatch max_hooked is not above 0")
	}
	fmt.Fprintf(w, "read: %s (polecat_pool.agent, polecat_pool.max_seats, polecat_pool.max_total), %s (patrols.spec_dispatch.hooked_agent, max_hooked)\n",
		config.TownSettingsPath(townRoot), patrolConfigLocation(townRoot))
	return nil
}

// patrolConfigLocation names where the daemon's patrol config really lives:
// `settings/config.json (section "daemon")` on a two-file town, where
// mayor/daemon.json no longer exists, and mayor/daemon.json on a five-file
// one. It is the text only; the loader and the writer resolve the same path.
func patrolConfigLocation(townRoot string) string {
	return config.SourcePathRel(townRoot, config.DaemonPatrolConfigPath(townRoot))
}

// poolShowLive is the occupancy show prints beside the caps, read from the same
// seat picture the dispatcher and the sling decide on (poolSeatSessions →
// specRosterFrom), so the page cannot disagree with the count a refusal names
// (gt-bc9b6). A picture that could not be read is no count at all: the page
// prints the caps without one rather than inventing an occupancy.
type poolShowLive struct {
	live  map[string]int
	dead  map[string]int
	total int
	ok    bool
}

func poolLiveFor(townRoot string) poolShowLive {
	return poolLiveForWith(townRoot, func(pool *config.PolecatPool) ([]poolSession, error) {
		return poolSeatSessions(newPoolSessionLister(), townRoot, pool)
	})
}

// poolLiveForWith is poolLiveFor with the town's seat read passed in, the way
// the rest of the pool's reads take their collaborators: a test builds the count
// from canned sessions and canned beads reads.
func poolLiveForWith(townRoot string, readSeats func(pool *config.PolecatPool) ([]poolSession, error)) poolShowLive {
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return poolShowLive{}
	}
	// A town whose rig registry is unreadable has no polecats to count, and the
	// seat picture needs it: the read stops here rather than listing the tmux
	// server for a count it could not complete (the dead-hooked half of the
	// picture enumerates polecat directories per rig).
	rigs, err := knownRigNames(townRoot)
	if err != nil || len(rigs) == 0 {
		return poolShowLive{}
	}
	sessions, err := readSeats(ts.PolecatPool)
	if err != nil {
		return poolShowLive{}
	}
	r := specRosterFrom(sessions, ts)
	out := poolShowLive{live: r.Live, dead: r.DeadHooked, ok: true}
	for _, n := range r.Live {
		out.total += n
	}
	return out
}

// seatNote is one seat's occupancy: the polecats the dispatcher counts on that
// seat's agent, over the seat's cap, with the seat's dead-hooked occupants
// named. A seat that reads full with fewer live sessions than its number then
// says why (gt-tldj4). Empty for a seat with no agent, which has no polecats to
// count.
func (l poolShowLive) seatNote(agent string, cap int) string {
	if !l.ok || agent == "" {
		return ""
	}
	note := fmt.Sprintf("  live %d/%d", l.live[agent], cap)
	if n := deadHookedCount(l.dead[agent]); n != "" {
		note += " (" + n + ")"
	}
	return note
}

// totalNote is the town's occupancy: every seat's polecats together, the number
// max_total bounds. Without a total cap there is no denominator to print, so
// the count stands alone.
func (l poolShowLive) totalNote(totalCap int) string {
	if !l.ok {
		return ""
	}
	if totalCap > 0 {
		return fmt.Sprintf("  live %d/%d", l.total, totalCap)
	}
	return fmt.Sprintf("  live %d", l.total)
}

// totalText is the total cap as show prints it: the cap both the dispatcher
// and sling read, or that the town has none. A file that names max_total 0 is
// named as the file, not as a read that fell back, even though both mean no
// cap — set-total 0 removes the key, so a 0 on the page is one someone wrote.
func (v poolSeatView) totalText() string {
	switch {
	case v.totalCap > 0:
		return fmt.Sprintf("cap %d (max_total): live polecats across every seat", v.totalCap)
	case v.totalCapSet:
		return "no cap (max_total 0)"
	default:
		return "no cap (max_total unset)"
	}
}

// capText is how a seat's cap is printed, naming the default a read fell back
// to when the key that would carry it is absent. The number is the cap the
// dispatcher runs the seat at, so a seat is never printed at a cap the
// dispatcher would not spend.
func (v poolSeatView) capText(s poolListedSeat) string {
	switch {
	case s.seat == poolSeat && v.poolCap <= 0:
		return fmt.Sprintf("uncapped for sling, cap %d for the dispatcher (max_seats unset)", s.cap)
	default:
		return fmt.Sprintf("cap %d", s.cap)
	}
}

// printSeatChange reports a seat write and, for the second seat, when it lands.
// The ticker reads every seat from the files per tick, so the write holds from
// the next tick it runs and nothing has to be restarted (gt-jdnab).
func printSeatChange(w io.Writer, seat poolSeatKey, agent string, n int) {
	if n == 0 {
		fmt.Fprintf(w, "closed the %s (%s)\n", seatName(seat), agent)
	} else {
		fmt.Fprintf(w, "%s: %s cap %d\n", seatName(seat), agent, n)
	}
	if seat == secondSeat {
		fmt.Fprintln(w, "takes effect at the dispatcher's next tick; no restart is needed")
	}
}

// runPoolSetSecond writes the second seat's agent and cap in one patrol-config
// write. The agent is checked against the presets the town can launch, and the
// cap against max_total, before anything is written.
func runPoolSetSecond(w io.Writer, townRoot, agent string, n int) error {
	if agent == "" {
		return fmt.Errorf("agent name is empty")
	}
	if err := checkKnownAgent(townRoot, agent); err != nil {
		return err
	}
	v, err := loadPoolSeats(townRoot)
	if err != nil {
		return err
	}
	if v.totalCap > 0 && n > v.totalCap {
		return fmt.Errorf("cap %d is above polecat_pool.max_total %d, so the town could never fill the seat: raise the total with gt pool set-total, or lower the cap", n, v.totalCap)
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	err = writePatrolSeats(townRoot, stamp, func(pc *config.DaemonPatrolConfig) {
		setSecondSeatAgent(pc, agent, n)
	})
	if err != nil {
		return fmt.Errorf("writing the %s: %w", seatName(secondSeat), err)
	}
	printSeatChange(w, secondSeat, agent, n)
	// The dispatcher keeps one seat per agent: say so when the new agent already
	// holds an earlier seat, since the second seat then never runs.
	if after, err := loadPoolSeats(townRoot); err == nil {
		if holder, shadowed := after.shadowedSecondSeat(); shadowed {
			fmt.Fprintf(w, "warning: the %s already runs %s and the dispatcher keeps one seat per agent, so the second seat will not run\n",
				seatName(holder), agent)
		}
	}
	return nil
}

// checkKnownAgent refuses an agent the town cannot launch, naming the ones it
// can: the built-in presets, the town's settings/agents.json and the custom
// agents in settings/config.json (what gt config agent list prints).
func checkKnownAgent(townRoot, agent string) error {
	registry, regErr := config.LoadAgentRegistryFor(townRoot, "")
	known := map[string]bool{}
	for _, name := range registry.Names() {
		known[name] = true
	}
	if ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot)); err == nil {
		for name := range ts.Agents {
			known[name] = true
		}
	}
	if known[agent] {
		return nil
	}
	names := make([]string, 0, len(known))
	for name := range known {
		names = append(names, name)
	}
	sort.Strings(names)
	msg := fmt.Sprintf("unknown agent %q: the known presets are %s", agent, strings.Join(names, ", "))
	if regErr != nil {
		msg += fmt.Sprintf(" (settings/agents.json was not fully read: %v)", regErr)
	}
	return errors.New(msg)
}

func seatName(k poolSeatKey) string {
	switch k {
	case secondSeat:
		return "second seat"
	}
	return "pool seat"
}

// runPoolSetTotal writes polecat_pool.max_total: the cap on live polecats
// across every seat. It lands on the pool's own seat's section, so a seat set
// is not the only command that reaches it.
func runPoolSetTotal(w io.Writer, townRoot string, n int) error {
	err := writeTownSeats(townRoot, time.Now().UTC().Format("20060102T150405Z"), func(ts *config.TownSettings) error {
		setMaxTotal(ts, n)
		return ts.PolecatPool.Validate()
	})
	if err != nil {
		return fmt.Errorf("writing the total cap: %w", err)
	}
	if n == 0 {
		fmt.Fprintln(w, "total cap removed (max_total): live polecats across every seat are uncapped again")
	} else {
		fmt.Fprintf(w, "total: cap %d live polecats across every seat\n", n)
	}
	return nil
}

// setMaxTotal sets the town's total cap, or removes max_total for 0: an absent
// key is no cap and a written 0 reads the same, so a zero is never written.
func setMaxTotal(ts *config.TownSettings, n int) {
	if ts.PolecatPool == nil {
		if n == 0 {
			return
		}
		ts.PolecatPool = &config.PolecatPool{}
	}
	if n == 0 {
		ts.PolecatPool.MaxTotal = nil
		return
	}
	ts.PolecatPool.MaxTotal = &n
}

// writeTownSeats changes the town settings under the file's lock, keeping a
// backup first. The retired pool-seat spellings are folded onto the keys that
// replaced them on the way out, because this writes the section that carries
// them: one left behind reopens on the next load the seat a close just removed
// (gt-aq7zo).
func writeTownSeats(townRoot, stamp string, change func(*config.TownSettings) error) error {
	path := config.TownSettingsPath(townRoot)
	return config.UpdateConfigJSON(path, 0o600, func(ts *config.TownSettings, exists bool) error {
		if !exists {
			*ts = *config.NewTownSettings()
		}
		if err := backupConfigFile(config.WritePath(path), stamp); err != nil {
			return err
		}
		return change(ts)
	}, config.FoldRetiredPoolSeatKeys())
}

// writePatrolSeats changes the daemon patrol config under the file's lock,
// keeping a backup first. The second seat lives there, not in the town settings.
func writePatrolSeats(townRoot, stamp string, change func(*config.DaemonPatrolConfig)) error {
	path := config.DaemonPatrolConfigPath(townRoot)
	return config.UpdateConfigJSON(path, 0o644, func(pc *config.DaemonPatrolConfig, exists bool) error {
		if !exists {
			pc.Type, pc.Version = "daemon-patrol-config", config.CurrentDaemonPatrolConfigVersion
		}
		if err := backupConfigFile(config.WritePath(path), stamp); err != nil {
			return err
		}
		change(pc)
		return nil
	})
}

// setSecondSeat sets spec_dispatch's max_hooked to n, removing it for 0.
func setSecondSeat(pc *config.DaemonPatrolConfig, n int) {
	if n == 0 && (pc.Patrols == nil || pc.Patrols.SpecDispatch == nil) {
		return
	}
	specDispatchOf(pc).MaxHooked = n
}

// setSecondSeatAgent sets the second seat's agent and cap together. Unlike
// setSecondSeat it makes the section for a cap of 0 too, so the agent is
// recorded for the day the seat is reopened.
func setSecondSeatAgent(pc *config.DaemonPatrolConfig, agent string, n int) {
	sd := specDispatchOf(pc)
	sd.HookedAgent, sd.MaxHooked = agent, n
}

// specDispatchOf returns pc's spec_dispatch section, making it when absent. A
// section made here says "enabled": true, so the file shows the dispatcher is
// on; a hand-written section without "enabled" is on too.
func specDispatchOf(pc *config.DaemonPatrolConfig) *config.SpecDispatchConfig {
	if pc.Patrols == nil {
		pc.Patrols = &config.PatrolsConfig{}
	}
	if pc.Patrols.SpecDispatch == nil {
		enabled := true
		pc.Patrols.SpecDispatch = &config.SpecDispatchConfig{Enabled: &enabled}
	}
	return pc.Patrols.SpecDispatch
}

// backupConfigFile copies path to path.bak-<stamp> before a write replaces it.
// A path that does not exist yet has nothing to keep.
func backupConfigFile(path, stamp string) error {
	data, err := os.ReadFile(path) //nolint:gosec // G304: internal config path
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("backing up %s: %w", path, err)
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	dst := filepath.Clean(path) + ".bak-" + stamp
	if err := os.WriteFile(dst, data, mode); err != nil {
		return fmt.Errorf("backing up %s: %w", path, err)
	}
	return nil
}
