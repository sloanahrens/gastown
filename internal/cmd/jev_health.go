package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/health"
	"github.com/steveyegge/gastown/internal/workspace"
)

// errJevHealthLedger marks a usage ledger the command could not read, so a
// test names the failure by identity instead of matching this package's prose.
var errJevHealthLedger = errors.New("reading the usage ledger")

// newJevHealthCmd builds the subcommand over the town lookup and the clock it
// reads, which the test supplies so a fixture is judged at a fixed instant.
func newJevHealthCmd(findTown func() (string, error), now func() time.Time) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Show whether each Jev battery is collecting data",
		Long:  jevHealthLong(),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			err := jevHealthInTown(cmd.OutOrStdout(), findTown, asJSON, now())
			if _, ok := IsSilentExit(err); ok {
				// The exit status is the verdict; cobra must not print it as an error.
				cmd.SilenceUsage, cmd.SilenceErrors = true, true
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print the rows as JSON")
	return cmd
}

func init() {
	jevCmd.AddCommand(newJevHealthCmd(workspace.FindFromCwdOrError, time.Now))
}

// jevHealthLong states the rules, rendering each threshold from the constant
// that applies it so the help cannot drift from the code.
func jevHealthLong() string {
	return fmt.Sprintf(`Judge every Jev battery from the files it writes, one row per battery: when
it last passed, its lines in the last 24 hours against the median of the seven
days before, its calls in the usage ledger, the share of its last day's lines
that carry no answers, and how many of its log lines did not parse.

A battery is ok; disabled, when the town has not turned it on under
patrols.<name>; idle by design, for the batteries only an event writes
(progress, failcause, and targets while scheduled_slings is off), where a
quiet log is expected; stale, for a continuous battery more than %s
behind, or for a landing battery the town landed past without a line;
late, for a weekly battery more than %s behind; low, when a battery with
a median of %d or more lines a day wrote under %s of it; unanswered,
when over %s of its last day's lines carry no answers; unparseable, when
its log holds lines and none of them parses; or unknown, when a landing
battery has no landing clock to be judged against.

The exit status is 1 when any enabled battery that is not idle by design is
stale, late, low, unanswered, unparseable or unknown, and 0 otherwise, so one
command answers whether Jev is collecting today. --json prints the same rows.

Examples:
  gt jev health
  gt jev health --json`,
		briefDuration(health.StaleAfter), briefDuration(health.LateAfter),
		health.LowMedian, shareText(health.LowShare), shareText(health.UnansweredShare))
}

// briefDuration renders a threshold in the help's words.
func briefDuration(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	case d%time.Minute == 0:
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	}
	return d.String()
}

// shareText renders a fraction in the help's words.
func shareText(share float64) string {
	switch share {
	case 0.25:
		return "a quarter"
	case 0.5:
		return "half"
	}
	return fmt.Sprintf("%.0f%%", share*100)
}

// jevHealthInTown resolves the town through findTown and prints the report.
func jevHealthInTown(out io.Writer, findTown func() (string, error), asJSON bool, now time.Time) error {
	townRoot, err := findTown()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	return jevHealthReport(out, townRoot, asJSON, now)
}

// jevHealthDoc is the --json document: the same rows the table prints.
type jevHealthDoc struct {
	Batteries []health.Row `json:"batteries"`
}

// jevHealthReport reads the town's patrol config, Jev logs, cursors and usage
// ledger, then prints and judges every battery.
func jevHealthReport(out io.Writer, townRoot string, asJSON bool, now time.Time) error {
	cfg, err := daemon.ReadPatrolConfig(townRoot)
	if err != nil {
		return fmt.Errorf("reading the patrol config: %w", err)
	}
	ledger, err := jevHealthLedger(townRoot, now)
	if err != nil {
		return err
	}
	inputs, err := jevHealthInputs(townRoot)
	if err != nil {
		return err
	}
	rows := health.Report(jevHealthConfig(cfg), inputs, ledger, now)
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(jevHealthDoc{Batteries: rows}); err != nil {
			return err
		}
	} else if err := writeJevHealthTable(out, rows); err != nil {
		return err
	}
	if health.ExitFails(rows) {
		return NewSilentExit(1)
	}
	return nil
}

// jevHealthConfig reads whether the town turned on each battery's patrol. Every
// jev_* key is opt-in in daemon.IsPatrolEnabled, so an unset key reads off
// there and nowhere else.
func jevHealthConfig(cfg *daemon.DaemonPatrolConfig) health.Config {
	out := health.Config{
		Enabled:         make(map[string]bool, len(health.Batteries)),
		ScheduledSlings: daemon.IsPatrolEnabled(cfg, "scheduled_slings"),
	}
	for _, b := range health.Batteries {
		out.Enabled[b.ConfigKey] = daemon.IsPatrolEnabled(cfg, b.ConfigKey)
	}
	return out
}

// jevHealthInputs reads every battery's files, keyed by battery name.
func jevHealthInputs(townRoot string) (map[string]health.Input, error) {
	inputs := make(map[string]health.Input, len(health.Batteries))
	for _, b := range health.Batteries {
		in, err := jevHealthInput(townRoot, b)
		if err != nil {
			return nil, err
		}
		inputs[b.Name] = in
	}
	return inputs, nil
}

// jevHealthInput reads one battery's cursor and log.
func jevHealthInput(townRoot string, b health.Battery) (health.Input, error) {
	var in health.Input
	if b.CursorPath != nil {
		t, ok, err := jevFileModTime(b.CursorPath(townRoot))
		if err != nil {
			return health.Input{}, err
		}
		in.Cursor, in.HasCursor = t, ok
	}
	data, present, err := jevReadFile(b.LogPath(townRoot))
	if err != nil {
		return health.Input{}, err
	}
	in.Log, in.Unparsed = health.ParseLog(data)
	in.LogPresent = present
	return in, nil
}

// jevHealthLedger counts every caller's calls in the last day from the usage
// ledger, where a missing ledger is no calls and not an error.
func jevHealthLedger(townRoot string, now time.Time) (map[string]int, error) {
	rep, err := jev.ReadSpend(jev.UsageLedgerPath(townRoot), now.Add(-health.Window), now, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errJevHealthLedger, err)
	}
	calls := make(map[string]int, len(rep.ByCaller))
	for caller, spend := range rep.ByCaller {
		calls[caller] = spend.Calls
	}
	return calls, nil
}

// jevReadFile reads a file a battery may not have written yet, where absent is
// empty and any other error fails the command rather than reading a battery as
// quiet. present is false only when the file does not exist, so a caller can
// tell a battery that never wrote from one whose file holds nothing readable.
func jevReadFile(path string) (data []byte, present bool, err error) {
	data, err = os.ReadFile(path) //nolint:gosec // G304: the path is the town's Jev runtime directory
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", path, err)
	}
	return data, true, nil
}

// jevFileModTime is a file's modification time, and false when it is absent.
func jevFileModTime(path string) (time.Time, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("reading %s: %w", path, err)
	}
	return info.ModTime(), true, nil
}

// writeJevHealthTable prints one row per battery in table order.
func writeJevHealthTable(w io.Writer, rows []health.Row) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BATTERY\tENABLED\tCADENCE\tLAST PASS\tLINES 24H\tMEDIAN 7D\tLEDGER 24H\tNO ANSWERS\tUNPARSED\tSTATUS\tREASON")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%.0f%%\t%d\t%s\t%s\n",
			r.Name, yesNo(r.Enabled), r.Cadence, lastPassCell(r), r.Lines24, r.Median7,
			r.LedgerCalls, r.NoAnswerShare*100, r.Unparsed, r.Status, r.Reason)
	}
	return tw.Flush()
}

// lastPassCell prints a row's last pass in UTC, or "-" when it has none.
func lastPassCell(r health.Row) string {
	if r.LastPass == nil {
		return "-"
	}
	return r.LastPass.UTC().Format("2006-01-02 15:04Z")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
