package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/workspace"
)

var metricsQuestionsDays int

func init() {
	metricsQuestionsCmd.Flags().IntVar(&metricsQuestionsDays, "days", 7, "Window in days for the review loop and time to land")
	metricsCmd.AddCommand(metricsQuestionsCmd)
}

var metricsQuestionsCmd = &cobra.Command{
	Use:   "questions",
	Short: "Print the ten-question status of how the town behaves, from its own files",
	Long: `Prints one line per question about the town: the review loop, time to land,
saturation, spend and backlog are computed from the review log, the hourly
snapshot, the landing ledger and the tracker's 15-minute git export; the rest
say what is held and what nobody records. Every number carries its n and
window. It reads files and runs read-only git; it writes nothing.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		town, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		return runMetricsQuestions(cmd.Context(), cmd.OutOrStdout(), questionsOSEnv(town), metricsQuestionsDays)
	},
}

// questionsEnv is every outside thing the status reads, so the command is a
// unit test over fakes and the real files are read in one place.
type questionsEnv struct {
	Now      func() time.Time
	ReadFile func(path string) ([]byte, error)
	Glob     func(pattern string) ([]string, error)
	// GitShow returns the content of spec ("HEAD:gt/events.jsonl") in the
	// repository at repo.
	GitShow func(ctx context.Context, repo, spec string) ([]byte, error)
	Town    string
}

func questionsOSEnv(town string) questionsEnv {
	return questionsEnv{
		Now:      time.Now,
		ReadFile: os.ReadFile,
		Glob:     filepath.Glob,
		GitShow: func(ctx context.Context, repo, spec string) ([]byte, error) {
			return exec.CommandContext(ctx, "git", "-C", repo, "show", spec).Output()
		},
		Town: town,
	}
}

// questionsEventShards are the tracker export's event files for the gt
// prefix: the live one and the older shards.
var questionsEventShards = []string{"events.jsonl", "events.2.jsonl", "events.3.jsonl", "events.4.jsonl", "events.5.jsonl", "events.6.jsonl", "events.7.jsonl"}

var questionsDatedDir = regexp.MustCompile(`/\d{4}-\d{2}-\d{2}$`)

func runMetricsQuestions(ctx context.Context, out io.Writer, env questionsEnv, days int) error {
	now := env.Now().UTC()
	since := now.AddDate(0, 0, -days)
	runtime := constants.TownRuntimePath(env.Town)
	var qs []metrics.Question
	var problems []string
	problem := func(what string, err error) { problems = append(problems, fmt.Sprintf("%s: %v", what, err)) }

	// 3: the review log.
	if b, err := env.ReadFile(filepath.Join(runtime, "landings", "reviews.jsonl")); err != nil {
		problem("review log", err)
		qs = append(qs, metrics.Question{N: 3, Title: "Review loop", Status: metrics.QuestionMissing, Detail: "review log unreadable"})
	} else {
		qs = append(qs, metrics.QuestionReviewLoop(b, since))
	}

	// 6, 8, 9: the last hourly snapshot.
	if b, err := env.ReadFile(filepath.Join(runtime, "metrics", "snapshots.jsonl")); err != nil {
		problem("snapshot log", err)
		for _, n := range []struct {
			n     int
			title string
		}{{6, "Saturation"}, {8, "Spend"}, {9, "Backlog"}} {
			qs = append(qs, metrics.Question{N: n.n, Title: n.title, Status: metrics.QuestionMissing, Detail: "snapshot log unreadable"})
		}
	} else {
		last := metrics.LastLine(b)
		qs = append(qs, metrics.QuestionSaturation(last), metrics.QuestionSpend(last), metrics.QuestionBacklog(last))
	}

	// 4: the newest dated ledger joined to the tracker export's events.
	qs = append(qs, questionTimeToLand(ctx, env, runtime, now, days, problem))

	qs = append(qs, metrics.QuestionsNotComputed()...)
	if err := metrics.RenderQuestions(out, qs); err != nil {
		return err
	}
	for _, p := range problems {
		if _, err := fmt.Fprintf(out, "note: %s\n", p); err != nil {
			return err
		}
	}
	return nil
}

func questionTimeToLand(ctx context.Context, env questionsEnv, runtime string, now time.Time, days int, problem func(string, error)) metrics.Question {
	missing := func(why string) metrics.Question {
		return metrics.Question{N: 4, Title: "Time to land", Status: metrics.QuestionMissing, Detail: why}
	}
	dirs, err := env.Glob(filepath.Join(runtime, "metrics", "20*"))
	if err != nil || len(dirs) == 0 {
		return missing("no dated landing ledger under .runtime/metrics")
	}
	sort.Strings(dirs)
	var ledgerDir string
	for i := len(dirs) - 1; i >= 0; i-- {
		if questionsDatedDir.MatchString(dirs[i]) {
			ledgerDir = dirs[i]
			break
		}
	}
	if ledgerDir == "" {
		return missing("no dated landing ledger under .runtime/metrics")
	}
	csvData, err := env.ReadFile(filepath.Join(ledgerDir, "landings.csv"))
	if err != nil {
		problem("landing ledger", err)
		return missing("landing ledger unreadable")
	}
	landed, _, err := metrics.ParseLedgerLandings(csvData, "gt-")
	if err != nil {
		problem("landing ledger", err)
		return missing("landing ledger unusable")
	}
	repo := filepath.Join(env.Town, ".dolt-archive", "git")
	var events []byte
	for _, shard := range questionsEventShards {
		b, err := env.GitShow(ctx, repo, "HEAD:gt/"+shard)
		if err != nil {
			// A shard that does not exist yet is normal; an export with no shard
			// at all is not.
			continue
		}
		events = append(events, b...)
		events = append(events, '\n')
	}
	if len(events) == 0 {
		problem("tracker export", fmt.Errorf("no event shard readable in %s", repo))
		return missing("tracker export events unreadable")
	}
	q := metrics.QuestionTimeToLand(events, landed, now, days)
	q.Detail += fmt.Sprintf(" Ledger %s, gastown (gt-) daemon-route landings only.", filepath.Base(ledgerDir))
	return q
}
