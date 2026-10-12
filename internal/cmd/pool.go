package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/workspace"
)

// gt pool reads and changes the town's two polecat seats: the pool seat
// (polecat_pool.agent capped by max_seats, in settings/config.json) and the
// second seat (spec_dispatch hooked_agent capped by max_hooked, in the daemon
// patrol config). Both are cap-per-agent knobs a hand edit gets wrong, because
// a max_seats of 0 reads as uncapped to sling and as the dispatcher's default
// cap, so closing a seat means removing its keys, never writing a zero.

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
seat by removing its keys.

<agent> must be the pool agent (polecat_pool.agent) or the second seat's agent
(spec_dispatch hooked_agent, claude-sonnet when unset). When the pool has no
agent, the named agent becomes the pool agent. Any other agent is refused.

The pool seat takes effect at the next sling. The second seat is read when
the daemon starts, so a change to it needs a daemon kickstart.

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

func init() {
	poolCmd.AddCommand(poolShowCmd, poolSetCmd)
	rootCmd.AddCommand(poolCmd)
}

// poolSeatKey names which of the two seats a change lands on.
type poolSeatKey int

const (
	poolSeat   poolSeatKey = iota // polecat_pool.agent, max_seats
	secondSeat                    // spec_dispatch hooked_agent, max_hooked
)

// poolSeatView is the two seats as the files hold them.
type poolSeatView struct {
	poolAgent string
	poolCap   int // max_seats; 0 when the key is absent
	// hookedAgent is the agent the second seat runs, spec_dispatch's default
	// when hooked_agent is unset. hookedCap is max_hooked; 0 means closed.
	hookedAgent string
	hookedCap   int
}

func poolSeatsOf(ts *config.TownSettings, sd *config.SpecDispatchConfig) poolSeatView {
	v := poolSeatView{hookedAgent: defaultSpecHookedAgent}
	if ts != nil && ts.PolecatPool != nil {
		v.poolAgent = ts.PolecatPool.Agent
		v.poolCap = ts.PolecatPool.MaxSeats
	}
	if sd != nil {
		if sd.HookedAgent != "" {
			v.hookedAgent = sd.HookedAgent
		}
		v.hookedCap = sd.MaxHooked
	}
	return v
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
func loadPoolSpecDispatch(townRoot string) (*config.SpecDispatchConfig, error) {
	pc, err := config.LoadDaemonPatrolConfig(config.DaemonPatrolConfigPath(townRoot))
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if pc.Patrols == nil {
		return nil, nil
	}
	return pc.Patrols.SpecDispatch, nil
}

func runPoolShow(w io.Writer, townRoot string) error {
	v, err := loadPoolSeats(townRoot)
	if err != nil {
		return err
	}
	open := false
	if v.poolAgent != "" {
		open = true
		capText := fmt.Sprintf("cap %d", v.poolCap)
		if v.poolCap <= 0 {
			capText = fmt.Sprintf("uncapped for sling, cap %d for the dispatcher (max_seats unset)", defaultSpecSeatCap)
		}
		fmt.Fprintf(w, "pool seat    %s  %s\n", v.poolAgent, capText)
	}
	if v.hookedCap > 0 {
		open = true
		fmt.Fprintf(w, "second seat  %s  cap %d\n", v.hookedAgent, v.hookedCap)
	}
	if !open {
		fmt.Fprintln(w, "no open seat: polecat_pool.agent is unset and spec_dispatch max_hooked is not above 0")
	}
	fmt.Fprintf(w, "read: %s (polecat_pool.agent, polecat_pool.max_seats), %s (daemon.patrols.spec_dispatch.hooked_agent, max_hooked)\n",
		config.TownSettingsPath(townRoot), config.DaemonPatrolConfigPath(townRoot))
	return nil
}

// poolSeatFor picks the seat a set of agent to n lands on, or the refusal
// naming both seats. A cap above 0 for an agent holding no seat opens the pool
// seat when the pool has none; closing needs a seat to close.
func poolSeatFor(v poolSeatView, agent string, n int) (poolSeatKey, error) {
	switch {
	case v.poolAgent == agent:
		return poolSeat, nil
	case v.hookedAgent == agent:
		return secondSeat, nil
	case v.poolAgent == "" && n > 0:
		return poolSeat, nil
	}
	pool := "none"
	if v.poolAgent != "" {
		pool = v.poolAgent
	}
	return 0, fmt.Errorf("%q holds no seat: the pool seat is %s (polecat_pool.agent) and the second seat is %s (spec_dispatch hooked_agent)",
		agent, pool, v.hookedAgent)
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
	case poolSeat:
		path := config.TownSettingsPath(townRoot)
		err = config.UpdateConfigJSON(path, 0o600, func(ts *config.TownSettings, exists bool) error {
			if !exists {
				*ts = *config.NewTownSettings()
			}
			if err := backupConfigFile(config.WritePath(path), stamp); err != nil {
				return err
			}
			setPoolSeat(ts, agent, n)
			return ts.PolecatPool.Validate()
		})
	case secondSeat:
		path := config.DaemonPatrolConfigPath(townRoot)
		err = config.UpdateConfigJSON(path, 0o644, func(pc *config.DaemonPatrolConfig, exists bool) error {
			if !exists {
				pc.Type, pc.Version = "daemon-patrol-config", config.CurrentDaemonPatrolConfigVersion
			}
			if err := backupConfigFile(config.WritePath(path), stamp); err != nil {
				return err
			}
			setSecondSeat(pc, n)
			return nil
		})
	}
	if err != nil {
		return fmt.Errorf("writing the %s: %w", seatName(seat), err)
	}
	if n == 0 {
		fmt.Fprintf(w, "closed the %s (%s)\n", seatName(seat), agent)
	} else {
		fmt.Fprintf(w, "%s: %s cap %d\n", seatName(seat), agent, n)
	}
	if seat == secondSeat {
		fmt.Fprintln(w, "spec_dispatch is read at daemon start: kickstart the daemon for this to take effect")
	}
	return nil
}

func seatName(k poolSeatKey) string {
	if k == secondSeat {
		return "second seat"
	}
	return "pool seat"
}

// setPoolSeat opens the pool seat on agent at n, or closes it for n of 0 by
// removing both keys: a max_seats of 0 reads as uncapped to sling and as the
// default cap to the dispatcher, so a zero is never written.
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

// setSecondSeat sets spec_dispatch's max_hooked to n, removing it for 0. A
// spec_dispatch section made here is enabled: the ticker is on only while the
// section is absent or says so, and a section without "enabled" turns it off.
func setSecondSeat(pc *config.DaemonPatrolConfig, n int) {
	if n == 0 && (pc.Patrols == nil || pc.Patrols.SpecDispatch == nil) {
		return
	}
	if pc.Patrols == nil {
		pc.Patrols = &config.PatrolsConfig{}
	}
	if pc.Patrols.SpecDispatch == nil {
		pc.Patrols.SpecDispatch = &config.SpecDispatchConfig{Enabled: true}
	}
	pc.Patrols.SpecDispatch.MaxHooked = n
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
