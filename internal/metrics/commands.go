package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The command layer of the town metrics script, ported from the top of
// scripts/town_metrics/cli.py: main, the four steps it runs (extract, mango,
// backtest, report) and all, which runs them in that order. One invocation
// wires the town once and every step reads that wiring, so a run of all costs
// one town read, as the Python's main did.

// The subcommands main accepts, named as the usage line names them.
const (
	CommandExtract  = "extract"
	CommandMango    = "mango"
	CommandBacktest = "backtest"
	CommandReport   = "report"
	CommandAll      = "all"
)

// usageText is main's answer to an argv that names no command. It is kept
// verbatim from the Python so the command that replaces scripts/town-metrics
// answers an operator the same way.
const usageText = "usage: town-metrics extract|mango|backtest|report|all   (run under `nice`, on demand only)"

// mangoBinEnv names the mango binary find_mango takes as given.
const mangoBinEnv = "MANGO_BIN"

// mangoSummaryName is the per-target counts the mango section leaves beside
// its text for the backtest to read.
const mangoSummaryName = "mango-summary.json"

// Commands is one invocation of the command layer: the day it reports, the
// wiring that reads the town, and the seams the steps after the extract read
// through.
type Commands struct {
	// Today is the day the run reports, as YYYY-MM-DD; it names the extract's
	// output directory and the digest.
	Today string
	// Town is the town root; empty is TOWN_METRICS_TOWN, else the home
	// directory's gt.
	Town string
	// Wire builds the day's town; nil wires the host's real env.
	Wire func(ctx context.Context) (RealTown, error)
	// Env reads the files the mango section, backtest and report read; nil is
	// OSEnv over the town Wire returned.
	Env Env
	// Mango finds the mango binary for a town; nil is FindMango over the
	// host's environment and PATH.
	Mango func(town string) string
	// Out takes the receipt lines; nil is the process's stdout.
	Out io.Writer
}

// withDefaults fills each seam the caller left unset with the host's, so the
// zero Commands runs the real town.
func (c Commands) withDefaults() Commands {
	if c.Wire == nil {
		c.Wire = func(ctx context.Context) (RealTown, error) {
			return RealEnv(ctx, RealEnvOptions{Today: c.Today, Town: c.Town, Run: OSRunnerDir(DefaultAllowEnv)})
		}
	}
	if c.Mango == nil {
		c.Mango = func(town string) string { return FindMango(town, MangoLookup{}) }
	}
	if c.Out == nil {
		c.Out = os.Stdout
	}
	return c
}

// Main is cli.main: it runs argv's one subcommand and returns 2 for an argv
// that names no command, 0 otherwise, with a failed command returned as an
// error naming the step. The usage line goes to Out, so a test reads the same
// answer an operator sees.
func Main(ctx context.Context, argv []string, c Commands) (int, error) {
	c = c.withDefaults()
	if len(argv) != 1 || !isCommand(argv[0]) {
		return 2, c.receipt("%s", usageText)
	}
	return 0, Run(ctx, argv[0], c)
}

// Run executes one named subcommand. It is the whole command layer main
// dispatches to, and the seam a cobra subcommand calls with its own name.
func Run(ctx context.Context, command string, c Commands) error {
	c = c.withDefaults()
	if !isCommand(command) {
		return fmt.Errorf("metrics: %q is not a subcommand", command)
	}
	real, env, err := c.wire(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}
	if command == CommandExtract || command == CommandAll {
		if err := c.runExtract(ctx, real); err != nil {
			return fmt.Errorf("extract: %w", err)
		}
	}
	if command == CommandMango || command == CommandAll {
		if err := c.runMango(ctx, real, env); err != nil {
			return fmt.Errorf("mango: %w", err)
		}
	}
	if command == CommandBacktest || command == CommandAll {
		if err := c.runBacktest(real, env); err != nil {
			return fmt.Errorf("backtest: %w", err)
		}
	}
	if command == CommandReport || command == CommandAll {
		if err := c.runReport(real, env); err != nil {
			return fmt.Errorf("report: %w", err)
		}
	}
	return nil
}

// isCommand reports whether name is one of main's subcommands.
func isCommand(name string) bool {
	switch name {
	case CommandExtract, CommandMango, CommandBacktest, CommandReport, CommandAll:
		return true
	}
	return false
}

// wire builds the day's town and the env the later steps read: the Env the
// caller named, else the host's over the town just wired. The env's Town is the
// wired one, so a caller that supplies only file seams still reads the town the
// extract read.
func (c Commands) wire(ctx context.Context) (RealTown, Env, error) {
	real, err := c.Wire(ctx)
	if err != nil {
		return RealTown{}, Env{}, err
	}
	env := c.Env
	if env.ReadFile == nil {
		env = OSEnv(real.Town)
	}
	env.Town = real.Town
	return real, env, nil
}

// runExtract is main's extract step: the extract over the wired town with its
// caches saved, and the receipt naming what it wrote.
func (c Commands) runExtract(ctx context.Context, real RealTown) error {
	result, err := Extract(ctx, real.Extract)
	if err != nil {
		return err
	}
	if err := SaveCaches(real.Extract, real.Town); err != nil {
		return err
	}
	return c.receipt("extract: %d landings, %d weekly rows -> %s",
		len(result.Landings), len(result.Weekly), real.Extract.OutDir)
}

// runMango is main's mango step: the findings section written into the
// extract's output directory, with the receipt naming whether mango ran. A
// host without mango says so in the section's first line, which is the receipt.
func (c Commands) runMango(ctx context.Context, real RealTown, env Env) error {
	text, err := WriteMangoSection(ctx, env, real.Extract.OutDir, c.Mango(real.Town), MangoSectionOptions{})
	if err != nil {
		return err
	}
	line := "section written"
	if strings.HasPrefix(text, "mango binary") {
		line = firstLine(text)
	}
	return c.receipt("mango: %s", line)
}

// runBacktest is main's backtest step: the section written into the extract's
// output directory, which the report reads back.
func (c Commands) runBacktest(real RealTown, env Env) error {
	if _, err := WriteBacktest(env, real.Extract.OutDir, real.DocsDir); err != nil {
		return err
	}
	return c.receipt("backtest: section written")
}

// runReport is main's report step: the digest rebuilt from the day's extract,
// with the receipt naming the file it wrote.
func (c Commands) runReport(real RealTown, env Env) error {
	path, err := WriteReport(env, real)
	if err != nil {
		return err
	}
	return c.receipt("report: %s", path)
}

// receipt writes one receipt line to the command's Out, and reports a write
// that failed rather than losing it: a receipt nobody read is a run whose
// output the operator never saw.
func (c Commands) receipt(format string, args ...any) error {
	_, err := fmt.Fprintf(c.Out, format+"\n", args...)
	return err
}

// WriteReport is cli's report command: the digest rebuilt from the extract
// LoadResult reads back, returned as the path written.
func WriteReport(env Env, real RealTown) (string, error) {
	result, err := LoadResult(env, real.Extract.OutDir)
	if err != nil {
		return "", err
	}
	return Report(env, ReportInput{
		OutDir:  real.Extract.OutDir,
		DocsDir: real.DocsDir,
		Today:   real.Extract.Today,
		Weekly:  result.Weekly,
		Footer:  result.Footer,
	})
}

// WriteMangoSection is cli.write_mango_section: the mango findings section
// rendered into outDir and returned. An empty binary is a host without mango,
// which the section says and still writes.
func WriteMangoSection(ctx context.Context, env Env, outDir, binary string, o MangoSectionOptions) (string, error) {
	text, err := MangoSection(ctx, env, outDir, binary, o)
	if err != nil {
		return "", err
	}
	if err := writeText(outDir, reportMangoName, text); err != nil {
		return "", err
	}
	return text, nil
}

// WriteBacktest is cli.write_backtest: the weekly metrics backtested against
// docsDir's regime events and Sloan's answers, written into outDir and
// returned.
func WriteBacktest(env Env, outDir, docsDir string) (string, error) {
	if env.ReadFile == nil {
		return "", errors.New("write backtest: Env.ReadFile is unset")
	}
	result, err := LoadResult(env, outDir)
	if err != nil {
		return "", err
	}
	events, sloan := ReadBacktestInputs(env, docsDir)
	summary := readMangoSummary(env, filepath.Join(outDir, mangoSummaryName))
	text, err := BacktestSection(backtestRows(result.Weekly), events, ReplicatedFromSummary(summary), sloan)
	if err != nil {
		return "", err
	}
	if err := writeText(outDir, reportBacktestName, text); err != nil {
		return "", err
	}
	return text, nil
}

// writeText writes text and a trailing newline as dir/name, atomically.
func writeText(dir, name, text string) error {
	if err := writeAtomic(dir, name, func(w io.Writer) error {
		_, err := io.WriteString(w, text+"\n")
		return err
	}); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// backtestRows is []WeeklyMetrics as the backtest reads it: every weekly column
// except the labels, a figure or count the week has a value for, and nothing
// for one it does not, which is the Python's None.
func backtestRows(weekly []WeeklyMetrics) []WeeklyRow {
	out := make([]WeeklyRow, 0, len(weekly))
	for _, m := range weekly {
		row := WeeklyRow{Week: m.Week, Rig: m.Rig, Thin: m.Thin, Values: make(map[string]float64, len(WeeklyColumns))}
		for _, column := range WeeklyColumns {
			switch column {
			case "week", "rig", "thin", "dominant_model":
				continue
			}
			if v, ok := weeklyFigure(m, column); ok {
				if v != nil {
					row.Values[column] = *v
				}
				continue
			}
			if count, ok := weeklyCount(m, column); ok {
				row.Values[column] = float64(count)
			}
		}
		out = append(out, row)
	}
	return out
}

// readMangoSummary reads mango-summary.json, and names no target when the file
// is missing or does not parse: the Python's _load_json default, so a backtest
// still runs after a mango step that wrote nothing.
func readMangoSummary(env Env, path string) map[string]MangoSummary {
	summary := map[string]MangoSummary{}
	data, err := env.ReadFile(path)
	if err != nil {
		return summary
	}
	if err := json.Unmarshal(data, &summary); err != nil {
		// A file that does not parse is no targets, whatever it decoded before
		// it failed: a half-read summary is not one the backtest may read.
		return map[string]MangoSummary{}
	}
	return summary
}

// MangoLookup is FindMango's environment: the override, the built binary's
// existence test and the PATH search, so a case answers without the host's.
type MangoLookup struct {
	// Getenv reads MANGO_BIN; nil is the process's environment.
	Getenv func(string) string
	// Exists reports whether the town's built binary is there; nil is the
	// host's filesystem.
	Exists func(string) bool
	// LookPath finds mango on PATH; nil is the host's PATH.
	LookPath func(string) (string, error)
}

// FindMango is cli.find_mango: MANGO_BIN, else the binary built into the
// town's runtime, else mango on PATH, and "" when there is none.
func FindMango(town string, lookup MangoLookup) string {
	getenv := lookup.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if bin := getenv(mangoBinEnv); bin != "" {
		return bin
	}
	built := filepath.Join(town, ".runtime", "metrics", "bin", "mango")
	exists := lookup.Exists
	if exists == nil {
		exists = hostExists
	}
	if exists(built) {
		return built
	}
	lookPath := lookup.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	path, err := lookPath("mango")
	if err != nil {
		return ""
	}
	return path
}

// hostExists reports whether path exists.
func hostExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// firstLine is s up to its first newline.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
