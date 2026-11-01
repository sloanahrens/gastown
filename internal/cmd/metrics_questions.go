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
	"strings"
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
		return runMetricsQuestionsIn(cmd.Context(), cmd.OutOrStdout(), workspace.FindFromCwdOrError, metricsQuestionsDays)
	},
}

// runMetricsQuestionsIn finds the town with find and prints the status with the
// real files. find is workspace.FindFromCwdOrError in the command and a fake in
// the tests.
func runMetricsQuestionsIn(ctx context.Context, out io.Writer, find func() (string, error), days int) error {
	town, err := find()
	if err != nil {
		return err
	}
	return runMetricsQuestions(ctx, out, questionsOSEnv(town), days)
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
	// GitList returns the names (repo-relative, "gt/events.jsonl") of the files
	// directly under dir at HEAD in the repository at repo.
	GitList func(ctx context.Context, repo, dir string) ([]string, error)
	Town    string
}

func questionsOSEnv(town string) questionsEnv {
	return questionsEnv{
		Now:      time.Now,
		ReadFile: os.ReadFile,
		Glob:     filepath.Glob,
		GitShow: func(ctx context.Context, repo, spec string) ([]byte, error) {
			return questionsGitShow(ctx, questionsExec, repo, spec)
		},
		GitList: func(ctx context.Context, repo, dir string) ([]string, error) {
			return questionsGitList(ctx, questionsExec, repo, dir)
		},
		Town: town,
	}
}

// questionsRunner runs a program and returns its stdout. The real one execs;
// the tests pass a fake, so no git runs in the unit tier.
type questionsRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func questionsExec(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// questionsGitShow returns the content of spec ("HEAD:gt/events.jsonl") in the
// repository at repo, read-only.
func questionsGitShow(ctx context.Context, run questionsRunner, repo, spec string) ([]byte, error) {
	return run(ctx, "git", "-C", repo, "show", spec)
}

// questionsGitList returns the repo-relative names of the files directly under
// dir at HEAD in the repository at repo, read-only.
func questionsGitList(ctx context.Context, run questionsRunner, repo, dir string) ([]string, error) {
	out, err := run(ctx, "git", "-C", repo, "ls-tree", "--name-only", "HEAD", dir+"/")
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// questionsEventShard matches the tracker export's event files for the gt
// prefix: events.jsonl and its numbered shards. They are listed from the
// export, so a new shard is read without a code change; the stage logic does
// not depend on their order.
var questionsEventShard = regexp.MustCompile(`^gt/events(\.\d+)?\.jsonl$`)

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
	if err != nil {
		problem("ledger directories", err)
		return missing("the ledger directories could not be listed")
	}
	if len(dirs) == 0 {
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
	names, err := env.GitList(ctx, repo, "gt")
	if err != nil {
		problem("tracker export", err)
		return missing("tracker export events unreadable")
	}
	var events []byte
	read, failed := 0, 0
	for _, name := range names {
		if !questionsEventShard.MatchString(name) {
			continue
		}
		b, err := env.GitShow(ctx, repo, "HEAD:"+name)
		if err != nil {
			failed++
			problem("tracker export "+name, err)
			continue
		}
		read++
		events = append(events, b...)
		events = append(events, '\n')
	}
	if read == 0 {
		return missing(fmt.Sprintf("tracker export events unreadable (%d event files listed, none readable)", failed))
	}
	q := metrics.QuestionTimeToLand(events, landed, now, days)
	q.Detail += fmt.Sprintf(" Ledger %s, gastown (gt-) daemon-route landings only.", filepath.Base(ledgerDir))
	return q
}
