package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/hostload"
	"github.com/steveyegge/gastown/internal/hostprobe"
	"github.com/steveyegge/gastown/internal/workspace"
)

// hostLoadFlags are `gt host load`'s flags, read by runHostLoad.
type hostLoadFlags struct {
	sample bool
	every  time.Duration
	out    string
}

// hostLoadDeps is what the command takes from the host: the town it reports
// on, the prober, the file --sample appends to, and the stream the table
// prints on. A test supplies fakes, so no test reads the live town or starts a
// process.
type hostLoadDeps struct {
	findTownRoot func() (string, error)
	newProber    func(townRoot string) *hostload.Prober
	openOut      func(path string) (io.WriteCloser, error)
	out          io.Writer
}

// realHostLoadDeps is hostLoadDeps on this host.
func realHostLoadDeps() hostLoadDeps {
	return hostLoadDeps{
		findTownRoot: workspace.FindFromCwdOrError,
		newProber:    hostload.NewProber,
		openOut: func(path string) (io.WriteCloser, error) {
			return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		},
		out: os.Stdout,
	}
}

// newHostLoadCmd builds `gt host load`.
func newHostLoadCmd(d hostLoadDeps) *cobra.Command {
	var flags hostLoadFlags
	cmd := &cobra.Command{
		Use:   "load",
		Short: "Show CPU by owner, or record it over time",
		Long: `Print the CPU each owner used over one sample window: crew and bird
sessions by their worktrees, the landing gates, the installer, the daemon,
Dolt, each VM, the dashboard and everything else. The line below the table
carries the load averages, one timed bd call, the dispatcher's last tick and
the town's seat count.

  gt host load --sample --every 10s --out load.jsonl

samples instead: one JSON line per interval, appended to --out, so a slowdown
can be attributed afterwards instead of argued about from one ps snapshot.

A process that starts and exits between samples is not counted: a sample reads
one interval, it does not trace the host.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runHostLoad(cmd, d, flags)
		},
	}
	cmd.Flags().BoolVar(&flags.sample, "sample", false, "Keep sampling instead of printing one table")
	cmd.Flags().DurationVar(&flags.every, "every", hostload.DefaultEvery, "How long to wait between samples")
	cmd.Flags().StringVar(&flags.out, "out", "", "File to append one JSON line per sample to")
	return cmd
}

func runHostLoad(cmd *cobra.Command, d hostLoadDeps, flags hostLoadFlags) error {
	// The flags are checked before the town is, so a mistyped sampler is
	// refused before it reads the host.
	if !flags.sample {
		if flags.out != "" {
			return errors.New("host load: --out holds samples, so it needs --sample")
		}
		if cmd.Flags().Changed("every") {
			return errors.New("host load: --every sets the sample interval, so it needs --sample")
		}
	} else {
		if flags.out == "" {
			return errors.New("host load: --sample needs --out <file>: a sample written nowhere is lost")
		}
		if flags.every <= 0 {
			return fmt.Errorf("host load: --every %s must be positive", flags.every)
		}
	}
	townRoot, err := d.findTownRoot()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	p := d.newProber(townRoot)
	if !flags.sample {
		writeHostLoadReport(d.out, p.Sample(cmd.Context()))
		return nil
	}
	f, err := d.openOut(flags.out)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// SIGINT and SIGTERM end a sampling run cleanly: a sampler started by hand
	// is stopped with Ctrl-C, and one started beside the daemon with a signal.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rec := &hostload.Recorder{
		Interval: flags.every,
		Sample:   p.Sample,
		Append: func(line []byte) error {
			_, err := f.Write(line)
			return err
		},
	}
	return rec.Run(ctx)
}

// writeHostLoadReport prints one sample: a header stating what it measured and
// what it cannot, the owner table, and the probes beside it.
func writeHostLoadReport(w io.Writer, s hostload.Sample) {
	fmt.Fprintf(w, "CPU by owner over the last %s, sampled at %s (percent of one core, summed per owner).\n",
		hostload.SampleWindow, s.At.Format(time.RFC3339))
	fmt.Fprintln(w, "A process that started and exited between samples is not counted.")
	fmt.Fprintln(w)
	if s.Owners.Err != "" {
		fmt.Fprintf(w, "owners: %s\n", s.Owners.Err)
	} else {
		writeHostLoadRows(w, s.Owners.Value)
	}
	fmt.Fprintf(w, "\n%s\n", hostLoadFacts(s))
}

// writeHostLoadRows prints the owner table, largest first.
func writeHostLoadRows(w io.Writer, rows []hostload.Row) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "no process used CPU in the sample window")
		return
	}
	fmt.Fprintf(w, "%-24s %8s %7s\n", "OWNER", "CPU%", "PROCS")
	for _, r := range rows {
		fmt.Fprintf(w, "%-24s %8.1f %7d\n", r.Owner, r.CPU, r.Procs)
	}
}

// hostLoadFacts renders the probes beside the table, each as its reading or
// the error that stopped it.
func hostLoadFacts(s hostload.Sample) string {
	parts := []string{
		hostLoadValue("load", s.Load, func(l hostload.Load) string {
			return fmt.Sprintf("%.2f %.2f %.2f", l.One, l.Five, l.Fifteen)
		}),
		hostLoadValue("bd", s.BDLatencyMS, func(ms float64) string { return fmt.Sprintf("%.0f ms", ms) }),
		hostLoadValue("tick", s.TickMS, func(ms float64) string { return fmt.Sprintf("%.0f ms", ms) }),
		hostLoadValue("seats", s.Seats, strconv.Itoa),
	}
	return strings.Join(parts, " · ")
}

// hostLoadValue renders one probe: its reading, or the error that stopped it.
func hostLoadValue[T any](name string, f hostprobe.Field[T], render func(T) string) string {
	if f.Err != "" {
		return name + ": " + f.Err
	}
	return name + " " + render(f.Value)
}

func init() {
	hostCmd.AddCommand(newHostLoadCmd(realHostLoadDeps()))
}
