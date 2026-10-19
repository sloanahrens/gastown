package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/attention/jevreplay"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	jevReplayOut        string
	jevReplayMaxSpend   float64
	jevReplayFloor      float64
	jevReplayUSDPerBTok float64
)

var attentionJevReplayCmd = &cobra.Command{
	Use:    "jev-replay",
	Short:  "Replay the attention history through Jev and report how it does",
	Hidden: true,
	Long: `Replay every item the retained attention files describe through the Jev
triage, oldest first, and print how needs-you compares with a proxy for what a
human did: an acked item needed them, an item that cleared with no ack did not.

The report gives precision and recall of needs-you, calibration by confidence
band, how many items the fail-safe rules forced to needs-you, and the spend
against --max-spend-usd. It reports twice: tier 2 labels by acks-history.jsonl,
tier 1 by the acks the queue files still held. The run stops when the spend
reaches the cap, and the report says how far it got.

Read-only on the attention directory. The report goes to stdout, and with
--out also to that file, which may not be under the attention directory. The
API key comes from the Jev key file (~/.config/gt/jev.env).`,
	Args: cobra.NoArgs,
	RunE: runAttentionJevReplay,
}

func init() {
	f := attentionJevReplayCmd.Flags()
	f.StringVar(&jevReplayOut, "out", "", "Also write the report to this file")
	f.Float64Var(&jevReplayMaxSpend, "max-spend-usd", 5, "Stop the run when the spend reaches this many US dollars")
	f.Float64Var(&jevReplayFloor, "confidence-floor", 0.7, "Confidence below which an answer is forced to needs-you")
	f.Float64Var(&jevReplayUSDPerBTok, "usd-per-billion-tokens", 42, "Price of input and output tokens, in US dollars per billion")
	attentionCmd.AddCommand(attentionJevReplayCmd)
}

// newJevReplayClassifier builds the Jev client; tests replace it.
var newJevReplayClassifier = func() (jevreplay.Classifier, error) { return jev.NewClient() }

func runAttentionJevReplay(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	if !(jevReplayFloor >= 0 && jevReplayFloor <= 1) {
		return fmt.Errorf("--confidence-floor must be from 0 to 1, not %v", jevReplayFloor)
	}
	if err := checkJevReplayOut(townRoot, jevReplayOut); err != nil {
		return err
	}
	c, err := newJevReplayClassifier()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts := jevreplay.Options{
		Floor:               jevReplayFloor,
		MaxSpendUSD:         jevReplayMaxSpend,
		USDPerBillionTokens: jevReplayUSDPerBTok,
	}
	return attentionJevReplay(ctx, cmd.OutOrStdout(), townRoot, c, opts, jevReplayOut)
}

// attentionJevReplay reads the queue, replays it and prints the report, then
// writes it to out when out is set. A refused key still prints what the run
// got to.
func attentionJevReplay(ctx context.Context, w io.Writer, townRoot string, c jevreplay.Classifier, opts jevreplay.Options, out string) error {
	snap, err := attention.Read(townRoot)
	if err != nil {
		return err
	}
	history, err := attention.ReadAcksHistory(townRoot)
	if err != nil {
		return err
	}
	rep, runErr := jevreplay.Run(ctx, c, snap, history, opts)
	if runErr != nil && rep.Items == 0 {
		return runErr
	}
	var text strings.Builder
	rep.Write(&text)
	if _, err := w.Write([]byte(text.String())); err != nil {
		return err
	}
	if out != "" {
		if err := os.WriteFile(out, []byte(text.String()), 0o600); err != nil {
			return fmt.Errorf("writing the report: %w", err)
		}
	}
	return runErr
}

// checkJevReplayOut refuses an --out path inside the attention directory, so
// a replay can never add to or replace a queue file.
func checkJevReplayOut(townRoot, out string) error {
	if out == "" {
		return nil
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(attention.Dir(townRoot))
	if err != nil {
		return err
	}
	// Compare through the nearest existing parent, so a symlinked path
	// cannot reach the directory under another spelling.
	abs, dir = resolveExisting(abs), resolveExisting(dir)
	rel, err := filepath.Rel(dir, abs)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("--out must not be under the attention directory: the replay is read-only on the queue")
	}
	return nil
}

// resolveExisting resolves symlinks in the longest existing prefix of p and
// appends the rest unchanged.
func resolveExisting(p string) string {
	rest := ""
	for cur := p; ; cur = filepath.Dir(cur) {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		if filepath.Dir(cur) == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
	}
}
