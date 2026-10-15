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

// gt pool reads and changes the town's three polecat seats: the pool seat
// (polecat_pool.agent capped by max_seats, in settings/config.json), the pro
// seat (polecat_pool.pro_agent capped by pro_max, which takes only beads
// carrying pro_label), and the second seat (spec_dispatch hooked_agent capped
// by max_hooked, in the daemon patrol config: the "daemon" section of
// settings/config.json on a two-file town, mayor/daemon.json on a five-file
// one). Each is a cap-per-agent knob a
// hand edit gets wrong, and the two seats in the settings file disagree on
// what a zero means: max_seats of 0 reads as uncapped to sling and as the
// dispatcher's default cap, so closing the pool seat means removing its keys,
// while pro_max of 0 is the closed pro seat, since an absent pro_max is the
// dispatcher's default of 1.
//
// gt pool set-second sets the second seat's agent and cap together: set takes
// only the agent a seat already holds, so the second seat's agent was otherwise
// a hand edit of the patrol config.
//
// It also reads and sets the cap beside the seats rather than on one: max_total
// bounds live polecats across every seat (gt pool set-total). An absent
// max_total, like a written 0, is no cap.

var poolCmd = &cobra.Command{
	Use:     "pool",
	GroupID: GroupConfig,
	Short:   "Show and change the polecat seat caps",
	RunE:    requireSubcommand,
}

var poolShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print each open polecat seat with its agent and cap",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		townRoot, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		return runPoolShow(cmd.OutOrStdout(), townRoot)
	},
}

var poolSetCmd = &cobra.Command{
	Use:   "set <agent> <n>",
	Short: "Set an agent's seat cap; 0 closes the seat",
	Long: `Set the cap of the seat <agent> holds. n above 0 sets the cap; 0 closes the
seat. Closing the pool seat removes its keys, because a max_seats of 0 reads as
uncapped to sling and as the dispatcher's default cap; closing the pro seat
writes pro_max 0, because an absent pro_max is the dispatcher's default of 1.

<agent> must be the pool agent (polecat_pool.agent), the pro seat's agent
(polecat_pool.pro_agent, deepseek-pro when unset), or the second seat's agent
(spec_dispatch hooked_agent, claude-sonnet when unset; the daemon patrol
config, which is the "daemon" section of settings/config.json on a two-file
town and mayor/daemon.json on a five-file one). When the pool has no
agent, an agent holding none of those seats becomes the pool agent; the pro
seat's agent is never that agent, so naming it sets the pro seat whether or
not the pro seat is open. Any other agent is refused.

All three seats take effect at the dispatcher's next tick, which reads them from
the files on every run: no daemon restart or kickstart is needed.

The file is written atomically and the previous copy is kept beside it as
<file>.bak-<timestamp>. Keys gt pool does not own are left as they are.`,
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
		return runPoolSet(cmd.OutOrStdout(), townRoot, args[0], n)
	},
}

var poolSetTotalCmd = &cobra.Command{
	Use:   "set-total <n>",
	Short: "Cap live polecats across every seat; 0 removes the cap",
	Long: `Set polecat_pool.max_total, the cap on live polecats across every seat: the
pool seat, the pro seat and the second seat together. It is the cap beside the
per-agent ones, for a town whose two agents are each inside their own cap and
still exceed the number of polecats wanted (5 live where 3 was intended).

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
in one write. gt pool set only moves the cap of an agent a seat already holds,
so this is the verb for changing which agent the second seat runs: two
claude-sonnet seats and two deepseek-flash seats is gt pool set claude-sonnet 2
for the pool seat and gt pool set-second deepseek-flash 2.

<agent> must be a known preset: a built-in one, one in the town's
settings/agents.json, or a custom agent in settings/config.json (gt config agent
list names them). n above 0 sets the cap; 0 closes the seat but still records
the agent. An n above polecat_pool.max_total is refused, since the town could
never fill the seat: raise the total with gt pool set-total first.

The dispatcher runs one seat per agent, so an agent that already holds the pool
seat or the pro seat leaves the second seat idle; gt pool show and this command
say so.

The change takes effect at the dispatcher's next tick, like the other two seats:
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
	poolCmd.AddCommand(poolShowCmd, poolSetCmd, poolSetSecondCmd, poolSetTotalCmd)
	rootCmd.AddCommand(poolCmd)
}

// poolSeatKey names which of the three seats a change lands on.
type poolSeatKey int

const (
	poolSeat   poolSeatKey = iota // polecat_pool.agent, max_seats
	secondSeat                    // spec_dispatch hooked_agent, max_hooked
	proSeat                       // polecat_pool.pro_agent, pro_max
)

// poolSeatView is the three seats as the files hold them, and as the
// dispatcher runs them. seats is specBudgetFromConfig's own list — the call
// the dispatcher makes each tick — so show cannot print a seat the dispatcher
// would not run and set cannot name a seat the dispatcher does not give an
// agent: the order, the defaults and the one-seat-per-agent rule are the
// dispatcher's, not a second copy's.
type poolSeatView struct {
	poolAgent string
	poolCap   int // max_seats; 0 when the key is absent
	// proAgent is the agent the pro seat runs (pro_agent, the pool's default
	// when unset; non-empty even when the pro seat is closed), and proMaxSet
	// whether the file names pro_max: an absent pro_max is the dispatcher's
	// default of 1, and 0 the closed pro seat.
	proAgent  string
	proMaxSet bool
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
		proAgent:    config.DefaultSeatRefillProAgent,
		hookedAgent: defaultSpecHookedAgent,
	}
	if ts != nil && ts.PolecatPool != nil {
		v.poolAgent = ts.PolecatPool.Agent
		v.poolCap = ts.PolecatPool.MaxSeats
		v.proAgent = ts.PolecatPool.GetProAgent()
		v.proMaxSet = ts.PolecatPool.ProMax != nil
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
// specBudgetFromConfig builds comes from one of these three names, so the
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
// pool seat, the pro seat, then the second seat — the second seat first when
// prefer_hooked says so. An agent named twice keeps the seat that comes first
// (its dedup), so this order decides which seat a name holds, for a picture of
// the dispatcher's list and for a set on that name.
func (v poolSeatView) seatPrecedence() []poolSeatKey {
	if v.preferHooked {
		return []poolSeatKey{secondSeat, poolSeat, proSeat}
	}
	return []poolSeatKey{poolSeat, proSeat, secondSeat}
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
	case proSeat:
		return v.proAgent
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

func runPoolShow(w io.Writer, townRoot string) error {
	v, err := loadPoolSeats(townRoot)
	if err != nil {
		return err
	}
	return renderPoolShow(w, townRoot, v, poolLiveFor(townRoot))
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
		fmt.Fprintln(w, "no open seat: polecat_pool has no agent, the pro seat is closed (no polecat_pool section, or pro_max 0), and spec_dispatch max_hooked is not above 0")
	}
	fmt.Fprintf(w, "read: %s (polecat_pool.agent, polecat_pool.max_seats, polecat_pool.pro_agent, polecat_pool.pro_max, polecat_pool.max_total), %s (patrols.spec_dispatch.hooked_agent, max_hooked)\n",
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
	case s.seat == proSeat && !v.proMaxSet:
		return fmt.Sprintf("cap %d (pro_max unset: the dispatcher's default)", s.cap)
	default:
		return fmt.Sprintf("cap %d", s.cap)
	}
}

// poolSeatFor picks the seat a set of agent to n lands on, or the refusal
// naming the seats. An agent's seat is the one the dispatcher gives its name:
// the first of the dispatcher's three names it is, which is the seat
// specBudgetFromConfig's dedup keeps when one name holds two of them. The pro
// agent is always the pro seat, open or closed, so naming it never reaches the
// line below that opens the pool seat (gt-riazi). An agent holding none of the
// three opens the pool seat on a cap above 0; closing needs a seat to close.
func poolSeatFor(v poolSeatView, agent string, n int) (poolSeatKey, error) {
	for _, k := range v.seatPrecedence() {
		if v.agentOn(k) == agent {
			return k, nil
		}
	}
	if v.poolAgent == "" && n > 0 {
		return poolSeat, nil
	}
	pool := "none"
	if v.poolAgent != "" {
		pool = v.poolAgent
	}
	return 0, fmt.Errorf("%q holds no seat: the pool seat is %s (polecat_pool.agent), the pro seat is %s (polecat_pool.pro_agent) and the second seat is %s (spec_dispatch hooked_agent)",
		agent, pool, v.proAgent, v.hookedAgent)
}

func runPoolSet(w io.Writer, townRoot, agent string, n int) error {
	if agent == "" {
		return fmt.Errorf("agent name is empty")
	}
	v, err := loadPoolSeats(townRoot)
	if err != nil {
		return err
	}
	seat, err := poolSeatFor(v, agent, n)
	if err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	switch seat {
	case poolSeat, proSeat:
		err = writeTownSeats(townRoot, stamp, func(ts *config.TownSettings) error {
			if seat == proSeat {
				setProSeat(ts, n)
			} else {
				setPoolSeat(ts, agent, n)
			}
			return ts.PolecatPool.Validate()
		})
	case secondSeat:
		err = writePatrolSeats(townRoot, stamp, func(pc *config.DaemonPatrolConfig) {
			setSecondSeat(pc, n)
		})
	}
	if err != nil {
		return fmt.Errorf("writing the %s: %w", seatName(seat), err)
	}
	printSeatChange(w, seat, agent, n)
	return nil
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
	case proSeat:
		return "pro seat"
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
// key is no cap and a written 0 reads the same, so a zero is never written
// (the same reason setPoolSeat removes max_seats rather than writing a 0).
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

// setPoolSeat opens the pool seat on agent at n, or closes it for n of 0 by
// removing both keys: a max_seats of 0 reads as uncapped to sling and as the
// default cap to the dispatcher, so a zero is never written. The write folds
// the retired spellings of both keys onto them (writeTownSeats), so a file that
// carried overflow_agent does not keep supplying the seat.
func setPoolSeat(ts *config.TownSettings, agent string, n int) {
	if n == 0 {
		if ts.PolecatPool != nil {
			ts.PolecatPool.Agent, ts.PolecatPool.MaxSeats = "", 0
		}
		return
	}
	if ts.PolecatPool == nil {
		ts.PolecatPool = &config.PolecatPool{}
	}
	ts.PolecatPool.Agent, ts.PolecatPool.MaxSeats = agent, n
}

// setProSeat sets the pro seat's cap, pro_max, which is the seat's whole
// state: the seat takes beads carrying pro_label, and pro_agent, pro_label and
// every other key are left as the file holds them. A zero closes the seat
// rather than an absent key doing it, because an absent pro_max is the
// dispatcher's default of 1 (config.PolecatPool.GetProMax).
func setProSeat(ts *config.TownSettings, n int) {
	if ts.PolecatPool == nil {
		ts.PolecatPool = &config.PolecatPool{}
	}
	ts.PolecatPool.ProMax = &n
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
// section made here is enabled: the ticker is on only while the section is
// absent or says so, and a section without "enabled" turns it off.
func specDispatchOf(pc *config.DaemonPatrolConfig) *config.SpecDispatchConfig {
	if pc.Patrols == nil {
		pc.Patrols = &config.PatrolsConfig{}
	}
	if pc.Patrols.SpecDispatch == nil {
		pc.Patrols.SpecDispatch = &config.SpecDispatchConfig{Enabled: true}
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
