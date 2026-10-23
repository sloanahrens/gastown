package cmd

import (
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
// agents it names and validates the whole resulting map before writing; gt pool
// migrate converts a file that still holds the two-seat keys the map replaced.
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
	Long: `Set polecat_pool.max_total, the cap on live polecats across every seat. It is
the cap beside the per-agent ones, for a town whose agents are each inside their
own cap and still exceed the number of polecats wanted (5 live where 3 was
intended). gt pool set --total N does the same.

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

func init() {
	poolSetCmd.Flags().IntVar(&poolSetTotalCap, "total", 0, "Cap live polecats across every seat; 0 removes the cap")
	poolSetCmd.Flags().StringArrayVar(&poolSetRemove, "remove", nil, "Drop this agent's seat from the map; repeat for more")
	poolCmd.AddCommand(poolShowCmd, poolSetCmd, poolSetTotalCmd)
	rootCmd.AddCommand(poolCmd)
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
	return runPoolSeatMapShow(w, townRoot, ts, now, poolLiveFor(townRoot))
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
// closed with the next opening, and the town's total beneath them. A map with
// no seat says so, since the dispatcher then spawns nothing.
func renderPoolSeatMap(w io.Writer, townRoot string, v poolSeatMapView, live poolShowLive) {
	width := len("total")
	for _, s := range v.seats {
		if len(s.Agent) > width {
			width = len(s.Agent)
		}
	}
	for _, s := range v.seats {
		state := v.seatState(s)
		if s.PinnedOnly {
			state += ", pinned-only"
		}
		fmt.Fprintf(w, "%-*s  %s  %s\n", width, s.Agent, live.seatCount(s.Agent, s.Cap), state)
	}
	if len(v.seats) == 0 {
		fmt.Fprintln(w, "no seats: polecat_pool.seats is empty, so the dispatcher spawns nothing; gt pool set <agent>=<cap> adds one")
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
	count := fmt.Sprintf("%d/%d", l.live[agent], cap)
	if n := deadHookedCount(l.dead[agent]); n != "" {
		count += " (" + n + ")"
	}
	if n := parkedStaleCount(l.parked[agent]); n != "" {
		count += " (" + n + ")"
	}
	return count
}

// totalCount is the town's occupancy against max_total, which may be absent.
func (l poolShowLive) totalCount(max *int) string {
	cap := 0
	if max != nil {
		cap = *max
	}
	// The released parked seats are outside the total, so the line names them:
	// a total that dropped for a seat released and not for a polecat finished
	// would otherwise read the same (gt-t7k92).
	released := ""
	if n := parkedStaleCount(l.parkedTotal); n != "" {
		released = " (" + n + ")"
	}
	switch {
	case l.ok && cap > 0:
		return fmt.Sprintf("%d/%d", l.total, cap) + released
	case l.ok:
		return fmt.Sprintf("%d live", l.total) + released
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
	seats := map[string]config.SeatConfig{}
	var maxTotal *int
	if ts.PolecatPool != nil {
		maxTotal = ts.PolecatPool.MaxTotal
		seats = copyPoolSeatMap(ts.PolecatPool.Seats)
	}
	before := copyPoolSeatMap(seats)
	for _, agent := range remove {
		if _, ok := seats[agent]; !ok {
			return fmt.Errorf("%q is not a seat: the seats are %s", agent, poolSeatNames(seats))
		}
		delete(seats, agent)
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

// poolShowLive is the occupancy show prints beside the caps, read from the same
// seat picture the dispatcher and the sling decide on (poolSeatSessions →
// specRosterFrom), so the page cannot disagree with the count a refusal names
// (gt-bc9b6). A picture that could not be read is no count at all: the page
// prints the caps without one rather than inventing an occupancy.
type poolShowLive struct {
	live map[string]int
	dead map[string]int
	// parked counts, per agent, the released parked seats, which are not in live.
	parked map[string]int
	// parkedTotal is the sum of parked: the released seats the total leaves out.
	parkedTotal int
	total       int
	ok          bool
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
	out := poolShowLive{live: r.Live, dead: r.DeadHooked, parked: r.ParkedStale, ok: true}
	for _, n := range r.Live {
		out.total += n
	}
	for _, n := range r.ParkedStale {
		out.parkedTotal += n
	}
	return out
}

// runPoolSetTotal writes polecat_pool.max_total: the cap on live polecats
// across every seat.
func runPoolSetTotal(w io.Writer, townRoot string, n int) error {
	err := writeTownSeats(townRoot, time.Now().UTC().Format("20060102T150405Z"), func(ts *config.TownSettings) error {
		setMaxTotal(ts, n)
		if ts.PolecatPool == nil {
			return nil
		}
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
// backup first.
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
	})
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
