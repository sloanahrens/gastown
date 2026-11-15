package polecat

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
)

// steeringBeads is the spawn's bead reader as a test steers it: a real fake
// store, with one Show call or every comments read failing on demand. Show
// calls are counted from 1; the launch makes exactly one, validateIssue's,
// which the task file also reads from.
type steeringBeads struct {
	beads.Client
	shows       int
	failShowAt  int // the Show call number that fails; 0 fails none
	showErr     error
	comments    []beads.Comment // when set, the answer to Comments
	commentsErr error
}

func (s *steeringBeads) Show(id string) (*beads.Issue, error) {
	s.shows++
	if s.failShowAt != 0 && s.shows == s.failShowAt {
		return nil, s.showErr
	}
	return s.Client.Show(id)
}

func (s *steeringBeads) Comments(id string) ([]beads.Comment, error) {
	if s.commentsErr != nil {
		return nil, s.commentsErr
	}
	if s.comments != nil {
		return s.comments, nil
	}
	return s.Client.Comments(id)
}

// taskFileLaunch is one sandbox launch of Toast and what it left behind: the
// SpawnSpec Prepare received, the warning lines the session manager wrote, and
// the launch error.
type taskFileLaunch struct {
	backend  *recordingSandboxBackend
	warnings []string
	err      error
}

// launchForTaskFile starts Toast on a recording sandbox backend with issue as
// its work bead, reading beads through db. The backend fails after Prepare, so
// the launch stops there with the SpawnSpec in hand.
func launchForTaskFile(t *testing.T, db beads.Client, issue string) *taskFileLaunch {
	t.Helper()
	town := t.TempDir()
	rigPath := filepath.Join(town, "gastown")
	settings := filepath.Join(rigPath, "settings")
	if err := os.MkdirAll(settings, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{"remote_url":"http://forgejo:3000/owner/gastown.git"}}}`
	if err := os.WriteFile(filepath.Join(settings, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	workDir, _, w := setupSessionBranchTestRepo(t)
	out := &taskFileLaunch{}
	m := &SessionManager{
		tmux: newFakeSessionTmux(),
		rig:  &rig.Rig{Name: "gastown", Path: rigPath},
		gits: w.opener(),
		warn: func(format string, args ...any) {
			out.warnings = append(out.warnings, fmt.Sprintf(format, args...))
		},
	}
	m.beadsAt = func(string) beads.Client { return db }
	out.backend = &recordingSandboxBackend{forgejo: "host.docker.internal", stopErr: errors.New("stop after prepare")}
	opts := SessionStartOptions{
		WorkDir:          workDir,
		Backend:          out.backend,
		Issue:            issue,
		RuntimeConfigDir: t.TempDir(),
	}
	out.err = m.launch("Toast", m.SessionName("Toast"), opts)
	return out
}

// slungBead files the work bead a sling hands the spawn: the fields are the
// caller's, with one note and one comment added.
func slungBead(t *testing.T, db *beadsfake.Fake, opts beads.CreateOptions) *beads.Issue {
	t.Helper()
	issue, err := db.Create(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(issue.ID, beads.UpdateOptions{AppendNotes: "first note"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(issue.ID, beads.UpdateOptions{AppendNotes: "second note"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddCommentAs(issue.ID, "daemon", "SP ESTIMATE v1 sp:8"); err != nil {
		t.Fatal(err)
	}
	return issue
}

// preparedSpawnSpec fails the test when the launch never reached Prepare: the
// task file is what Prepare's spec carries, so a start that stopped earlier
// proves nothing about it.
func preparedSpawnSpec(t *testing.T, out *taskFileLaunch) *session.SpawnSpec {
	t.Helper()
	if out.backend.got == nil {
		t.Fatalf("launch = %v; Prepare was never called, so no SpawnSpec was built", out.err)
	}
	return out.backend.got
}

// spawnBead is the title and description these tests sling: enough for a test
// to tell the file's bead apart from an empty file.
func spawnBead() beads.CreateOptions {
	return beads.CreateOptions{
		Title:       "Render the spawn's task file",
		Description: "The seat reads its task from TASK.md.",
	}
}

// The sandbox spawn carries the seat's whole assignment: the bead's title, its
// description, every acceptance box with its ticked state, the notes and the
// comments. A spec-dispatched bead keeps its boxes in the description's
// acceptance section and one shaped by hand in the acceptance field; both reach
// the file.
func TestSessionManagerTaskFileCarriesTheBeadsFields(t *testing.T) {
	t.Parallel()
	const acceptance = "## Acceptance\n\n- [x] the ticked box\n- [ ] the open box"
	tests := []struct {
		name string
		opts beads.CreateOptions
	}{
		{
			name: "the acceptance field",
			opts: func() beads.CreateOptions {
				opts := spawnBead()
				opts.Acceptance = acceptance
				return opts
			}(),
		},
		{
			name: "the description's acceptance section",
			opts: func() beads.CreateOptions {
				opts := spawnBead()
				opts.Description += "\n\n" + acceptance
				return opts
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := beadsfake.New()
			issue := slungBead(t, db, tt.opts)
			out := launchForTaskFile(t, db, issue.ID)
			if out.err == nil || !strings.Contains(out.err.Error(), "stop after prepare") {
				t.Fatalf("launch = %v, want the backend's stop error after Prepare", out.err)
			}
			spec := preparedSpawnSpec(t, out)
			text := string(spec.TaskFile)
			for _, want := range []string{
				"# TASK " + issue.ID + ": " + issue.Title,
				"The seat reads its task from TASK.md.",
				"- [x] the ticked box",
				"- [ ] the open box",
				"- first note",
				"- second note",
				"- 2026-01-02 03:04 daemon: SP ESTIMATE v1 sp:8",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("SpawnSpec.TaskFile = %q, want it to carry %q", text, want)
				}
			}
			// The two boxes must carry opposite ticks, not the same one twice.
			for _, notWant := range []string{"- [ ] the ticked box", "- [x] the open box"} {
				if strings.Contains(text, notWant) {
					t.Errorf("SpawnSpec.TaskFile = %q, want no %q", text, notWant)
				}
			}
			if got := len(out.warnings); got != 0 {
				t.Errorf("warnings = %q, want none for a readable bead", out.warnings)
			}
		})
	}
}

// The launch reads the work bead once, and the task file is built from that
// read: validateIssue keeps the issue it read and the spawn hands it on rather
// than reading the bead again (gt-8w5v21).
func TestSessionManagerTaskFileReadsTheWorkBeadOnce(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	issue := slungBead(t, db, spawnBead())
	reader := &steeringBeads{Client: db}
	out := launchForTaskFile(t, reader, issue.ID)
	if out.err == nil || !strings.Contains(out.err.Error(), "stop after prepare") {
		t.Fatalf("launch = %v, want the backend's stop error after Prepare", out.err)
	}
	spec := preparedSpawnSpec(t, out)
	if !strings.Contains(string(spec.TaskFile), issue.Title) {
		t.Errorf("SpawnSpec.TaskFile = %q, want the bead's title %q", spec.TaskFile, issue.Title)
	}
	if reader.shows != 1 {
		t.Errorf("Show calls = %d, want 1: the launch reads the work bead once", reader.shows)
	}
}

// A bead that cannot be read refuses the launch before any session exists: the
// validation read is the only one the launch makes, so there is no second read
// whose failure could have left the task file empty instead.
func TestSessionManagerTaskFileRefusesAnUnreadableWorkBead(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	issue := slungBead(t, db, spawnBead())
	reader := &steeringBeads{Client: db, failShowAt: 1, showErr: errors.New("dolt is unavailable")}
	out := launchForTaskFile(t, reader, issue.ID)
	if !errors.Is(out.err, ErrIssueInvalid) {
		t.Fatalf("launch = %v, want ErrIssueInvalid", out.err)
	}
	if out.backend.got != nil {
		t.Errorf("Prepare was called for a bead the launch could not read: %+v", out.backend.got)
	}
	if reader.shows != 1 {
		t.Errorf("Show calls = %d, want 1", reader.shows)
	}
	if got := len(out.warnings); got != 0 {
		t.Errorf("warnings = %q, want none: the refusal is the launch's error", out.warnings)
	}
}

// A comments read that fails empties the file too: a seat must not read a file
// that silently omits the comments its bead carries.
func TestSessionManagerTaskFileSurvivesACommentsFailure(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	issue := slungBead(t, db, spawnBead())
	reader := &steeringBeads{Client: db, commentsErr: errors.New("dolt is unavailable")}
	out := launchForTaskFile(t, reader, issue.ID)
	if out.err == nil || !strings.Contains(out.err.Error(), "stop after prepare") {
		t.Fatalf("launch = %v, want the backend's stop error after Prepare", out.err)
	}
	spec := preparedSpawnSpec(t, out)
	if len(spec.TaskFile) != 0 {
		t.Errorf("SpawnSpec.TaskFile = %q, want it empty", spec.TaskFile)
	}
	if got := len(out.warnings); got != 1 || !strings.Contains(out.warnings[0], issue.ID) {
		t.Errorf("warnings = %q, want one naming the bead %s", out.warnings, issue.ID)
	}
}

// A seat with no work bead gets no task file and costs no read.
func TestSessionManagerTaskFileIsAbsentWithoutAWorkBead(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	reader := &steeringBeads{Client: db}
	out := launchForTaskFile(t, reader, "")
	if out.err == nil || !strings.Contains(out.err.Error(), "stop after prepare") {
		t.Fatalf("launch = %v, want the backend's stop error after Prepare", out.err)
	}
	spec := preparedSpawnSpec(t, out)
	if len(spec.TaskFile) != 0 {
		t.Errorf("SpawnSpec.TaskFile = %q, want it empty", spec.TaskFile)
	}
	if reader.shows != 0 {
		t.Errorf("Show calls = %d, want none for a seat with no work bead", reader.shows)
	}
	if got := len(out.warnings); got != 0 {
		t.Errorf("warnings = %q, want none", out.warnings)
	}
}

// A comment whose time the store records in a shape that does not parse keeps
// its author and text; only the time is left off that line.
func TestSessionManagerTaskFileKeepsACommentWithAnUnreadableTime(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	issue := slungBead(t, db, spawnBead())
	reader := &steeringBeads{
		Client:   db,
		comments: []beads.Comment{{Author: "daemon", Text: "no date", CreatedAt: "not a time"}},
	}
	out := launchForTaskFile(t, reader, issue.ID)
	if out.err == nil || !strings.Contains(out.err.Error(), "stop after prepare") {
		t.Fatalf("launch = %v, want the backend's stop error after Prepare", out.err)
	}
	spec := preparedSpawnSpec(t, out)
	text := string(spec.TaskFile)
	if !strings.Contains(text, "- daemon: no date") {
		t.Errorf("SpawnSpec.TaskFile = %q, want the comment's author and text", text)
	}
	if got := len(out.warnings); got != 0 {
		t.Errorf("warnings = %q, want none", out.warnings)
	}
}
