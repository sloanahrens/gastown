package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/question"
	"github.com/steveyegge/gastown/internal/workspace"
)

// The overseer's questions for Sloan. The convention — the label, the body's
// sections, the [overseer] comment prefix — is internal/question's, and the
// dashboard's Questions pane and the daemon's question collector read the
// beads these commands write.

var (
	answerOverseer bool
	questionsJSON  bool
	askPriority    int
	askEffort      string
)

var askCmd = &cobra.Command{
	Use:     "ask <title>",
	GroupID: GroupComm,
	Short:   "File a question for Sloan and hold the work it blocks",
	Long: `File a question for Sloan as a bead in the town database, under the
overseer-question label, and hold the work it blocks until it is closed.

The question is a bead like any other, so --priority files it at bd's 0-4
(default 2) and a P0 question outranks one that can wait. --effort quick or
session records what answering takes, as an effort: label; a question filed
without one carries no tag.

The body comes from stdin, in these sections:

  ## Question
  What Sloan is asked to decide.
  ## Recommended answer
  What the overseer would do, so "yes" is an answer.
  ## Held work
  - gt-abc1            (a bead id; it leaves bd ready until the question closes)
  - stub: <title>      (work with no bead yet: a stub bead is made first)
  none
  ## Facts
  What Sloan needs to know to decide.

A body without a Question or a Recommended answer is refused. Held work and
Facts are optional. A held bead is made to depend on the question, so it stays
out of bd ready until the question is closed.

It prints the question's id and the command that answers it. Read the body from
a quoted heredoc, so backticks in it do not run.

Examples:
  gt ask --priority 1 --effort quick "Retry policy for landing" <<'EOF'
  ## Question
  Retry a rejected landing once, or hand it straight to a human?
  ## Recommended answer
  Once.
  ## Held work
  - gt-abc1
  ## Facts
  Two of the last ten rejections passed on a second run.
  EOF`,
	Args: cobra.ExactArgs(1),
	RunE: runAsk,
}

var answerCmd = &cobra.Command{
	Use:     "answer <question-id> [text]",
	GroupID: GroupComm,
	Short:   "Answer an overseer question with a comment on its bead",
	Long: `Comment on an overseer question. Without a flag the comment is Sloan's answer,
and the daemon wakes the overseer to act on it. With --overseer the comment
carries the "[overseer]" prefix: the overseer's own note, which is how a
question goes back to waiting on Sloan.

The text is the second argument, or stdin when there is none. gt answer does
not close the question; the overseer does, once it has acted.

Examples:
  gt answer hq-abc1 "yes, once"
  gt answer --overseer hq-abc1 "acted on it; closing"`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runAnswer,
}

var questionsCmd = &cobra.Command{
	Use:     "questions",
	GroupID: GroupComm,
	Short:   "List the open overseer questions as a priority queue",
	Long: `List every open overseer question as a priority queue: most urgent first and,
within a priority, the longest wait first. A row is the question's id, how
long it has waited, whether it awaits Sloan or Sloan has answered and the
overseer is acting, its priority, how many beads it holds, the effort tag it
was filed with, and its title. --json prints the dashboard's Questions payload
instead.`,
	Args: cobra.NoArgs,
	RunE: runQuestions,
}

func init() {
	askCmd.Flags().IntVar(&askPriority, "priority", question.Priority, "Priority to file the question at, bd's 0 (most urgent) to 4")
	askCmd.Flags().StringVar(&askEffort, "effort", "", "What answering takes: quick or session (a tag on the bead)")
	answerCmd.Flags().BoolVar(&answerOverseer, "overseer", false, `Comment as the overseer: prefix the text with "[overseer]"`)
	questionsCmd.Flags().BoolVar(&questionsJSON, "json", false, "Print the questions as JSON")
	rootCmd.AddCommand(askCmd, answerCmd, questionsCmd)
}

// questionStore is the part of the beads client the question commands use.
type questionStore interface {
	questionReads
	Create(opts beads.CreateOptions) (*beads.Issue, error)
	Show(id string) (*beads.Issue, error)
	Update(id string, opts beads.UpdateOptions) error
	AddDependency(issue, dependsOn string) error
	AddComment(id, comment string) error
}

// openQuestionStore is the town database's client: a question is filed there,
// and a reader that ran from a rig worktree would see that rig's beads alone.
func openQuestionStore() (questionStore, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, fmt.Errorf("finding the town root: %w", err)
	}
	return beads.New(beads.ResolveBeadsDir(townRoot)), nil
}

func runAsk(cmd *cobra.Command, args []string) error {
	opts := question.AskOptions{Priority: askPriority, Effort: askEffort}
	return askQuestion(cmd.InOrStdin(), cmd.OutOrStdout(), args[0], opts, openQuestionStore)
}

// askQuestion is gt ask: it reads and checks the body and the flags before it
// opens the town database, so a refused question touches nothing.
func askQuestion(in io.Reader, out io.Writer, title string, opts question.AskOptions, open func() (questionStore, error)) error {
	body, err := readQuestionInput(in, "the question body")
	if err != nil {
		return err
	}
	ask, err := question.ParseAsk(title, body, opts)
	if err != nil {
		return fmt.Errorf("gt ask: %w", err)
	}
	store, err := open()
	if err != nil {
		return err
	}
	id, holdErr := fileQuestion(store, ask)
	if id == "" {
		return holdErr
	}
	fmt.Fprintln(out, id)
	fmt.Fprintf(out, "answer with: %s\n", answerCommand(id))
	return holdErr
}

// fileQuestion files the question bead and holds the work it names. It returns
// the question's id as soon as the bead exists, with an error naming every
// hold that did not take: the question is filed either way, and the id is what
// the operator needs to finish the hold by hand.
func fileQuestion(s questionStore, ask question.Ask) (string, error) {
	issue, err := s.Create(beads.CreateOptions{
		Title:       ask.Title,
		Labels:      ask.Labels(),
		Priority:    ask.Priority,
		Description: ask.Body,
	})
	if err != nil {
		return "", fmt.Errorf("gt ask: filing the question: %w", err)
	}
	id, body := issue.ID, ask.Body
	var failed []string
	for _, h := range ask.Holds {
		held := h.ID
		if h.Stub != "" {
			// The stub is the question's held work, so it is filed where the
			// question was: it is the thing the priority is about.
			stub, err := s.Create(beads.CreateOptions{
				Title:       h.Stub,
				Priority:    ask.Priority,
				Description: fmt.Sprintf("Work held by overseer question %s; replace this stub with the real description.", id),
			})
			if err != nil {
				failed = append(failed, fmt.Sprintf("stub bead %q was not created (file it by hand and run: bd dep add <stub> %s): %v", h.Stub, id, err))
				continue
			}
			held = stub.ID
		}
		if err := s.AddDependency(held, id); err != nil {
			failed = append(failed, fmt.Sprintf("%s is not held by %s: %v", held, id, err))
			continue
		}
		if h.Stub == "" {
			continue
		}
		// The page lists held ids from the description, so the stub line is
		// pointed at the new bead, and only once the hold is in place.
		rewritten := question.ReplaceStub(body, h.Stub, held)
		if err := s.Update(id, beads.UpdateOptions{Description: &rewritten}); err != nil {
			failed = append(failed, fmt.Sprintf("the stub line for %s was not rewritten in %s (the hold is in place; the page will not list it): %v", held, id, err))
			continue
		}
		body = rewritten
	}
	if len(failed) > 0 {
		return id, fmt.Errorf("gt ask: question %s was filed, but:\n  %s", id, strings.Join(failed, "\n  "))
	}
	return id, nil
}

// answerCommand is the command that answers a question, as the page and gt ask
// show it.
func answerCommand(id string) string {
	return fmt.Sprintf(`gt answer %s "<answer>"`, id)
}

func runAnswer(cmd *cobra.Command, args []string) error {
	text := ""
	if len(args) == 2 {
		text = args[1]
	} else {
		in, err := readQuestionInput(cmd.InOrStdin(), "the answer")
		if err != nil {
			return err
		}
		text = in
	}
	store, err := openQuestionStore()
	if err != nil {
		return err
	}
	if err := answerQuestion(store, args[0], text, answerOverseer); err != nil {
		return err
	}
	who := "answer"
	if answerOverseer {
		who = "overseer note"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s added to %s\n", who, args[0])
	return nil
}

// answerQuestion comments on a question: Sloan's answer, or with asOverseer the
// overseer's own note. The comment's prefix is the whole difference, since both
// are authored by the same committer.
func answerQuestion(s questionStore, id, text string, asOverseer bool) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("gt answer: empty comment")
	}
	issue, err := s.Show(id)
	if err != nil {
		return fmt.Errorf("gt answer: reading %s: %w", id, err)
	}
	if !beads.HasLabel(issue, question.Label) {
		return fmt.Errorf("gt answer: %s is not an overseer question (no %s label)", id, question.Label)
	}
	hasPrefix := strings.HasPrefix(text, question.OverseerPrefix)
	switch {
	case asOverseer && !hasPrefix:
		text = question.OverseerPrefix + " " + text
	case !asOverseer && hasPrefix:
		// Sloan's answer that starts with the prefix would read as the
		// overseer's own note, and the question would go on waiting.
		return fmt.Errorf("gt answer: the text starts with %q, which marks an overseer note; use --overseer or reword it", question.OverseerPrefix)
	}
	if err := s.AddComment(id, text); err != nil {
		return fmt.Errorf("gt answer: commenting on %s: %w", id, err)
	}
	return nil
}

func runQuestions(cmd *cobra.Command, _ []string) error {
	store, err := openQuestionStore()
	if err != nil {
		return err
	}
	reader := newQuestionsReader(store)
	reader.all = true
	qs := reader.read(time.Now())
	if qs.Unavailable {
		return fmt.Errorf("gt questions: the questions could not be read")
	}
	return writeQuestions(cmd.OutOrStdout(), qs, questionsJSON)
}

// writeQuestions prints the queue as a table, or as the dashboard's JSON.
func writeQuestions(out io.Writer, qs *dashboard.Questions, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(qs)
	}
	if len(qs.Rows) == 0 {
		fmt.Fprintln(out, "no open questions")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tAGE\tSTATE\tPRI\tHELD\tEFFORT\tTITLE")
	for _, r := range qs.Rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\tP%d\t%d\t%s\t%s\n",
			r.ID, questionAge(r.AgeSec), r.State, r.Priority, r.HeldCount, questionEffort(r.Effort), r.Title)
	}
	return tw.Flush()
}

// questionAge is a question's age as the table prints it: "-" when the bead
// records no time it can be measured from.
func questionAge(sec *int64) string {
	if sec == nil {
		return "-"
	}
	d := time.Duration(*sec) * time.Second
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// questionEffort is a question's effort tag as the table prints it: "-" when
// the question carries none.
func questionEffort(effort string) string {
	if effort == "" {
		return "-"
	}
	return effort
}

// readQuestionInput reads what the command takes on stdin. A terminal is
// refused rather than waited on: gt ask and gt answer with nothing piped would
// otherwise sit silent until the operator guessed what they were waiting for.
func readQuestionInput(in io.Reader, what string) (string, error) {
	if f, ok := in.(*os.File); ok {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			return "", fmt.Errorf("%s is read from stdin; pipe it in or use a heredoc", what)
		}
	}
	data, err := io.ReadAll(in)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", what, err)
	}
	return string(data), nil
}
