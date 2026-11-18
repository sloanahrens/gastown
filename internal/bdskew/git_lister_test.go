package bdskew

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// testGitRepo is the bare clone path the git lister tests read.
const testGitRepo = "/town/beads/.repo.git"

// TestPreRollGuardGitListerListsTree: ListTree asks git for exactly the
// ls-tree call the guard names and returns the base names it prints.
func TestPreRollGuardGitListerListsTree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reason string
		dir    string
		out    string
		want   []string
	}{
		{
			reason: "the migrations directory of the pinned commit",
			dir:    MigrationsDir,
			out: "internal/storage/schema/migrations/0066_add_events_journal_actor.up.sql\x00" +
				"internal/storage/schema/migrations/0067_create_samples.up.sql\x00" +
				"internal/storage/schema/migrations/0067_create_samples.down.sql\x00",
			want: []string{
				"0066_add_events_journal_actor.up.sql",
				"0067_create_samples.up.sql",
				"0067_create_samples.down.sql",
			},
		},
		{
			reason: "a second directory prints full paths and base names come back",
			dir:    "internal/storage/schema",
			out:    "internal/storage/schema/schema.txt\x00",
			want:   []string{"schema.txt"},
		},
		{
			reason: "empty NUL-separated records are not entries",
			dir:    MigrationsDir,
			out: "internal/storage/schema/migrations/a.up.sql\x00" +
				"\x00" +
				"internal/storage/schema/migrations/b.up.sql\x00",
			want: []string{"a.up.sql", "b.up.sql"},
		},
		{
			reason: "a name holding a newline comes back as one unquoted entry",
			dir:    MigrationsDir,
			out:    "internal/storage/schema/migrations/a\nb.up.sql\x00",
			want:   []string{"a\nb.up.sql"},
		},
		{
			reason: "a name holding a tab and a non-ASCII byte comes back as itself",
			dir:    MigrationsDir,
			out:    "internal/storage/schema/migrations/café\tx.up.sql\x00",
			want:   []string{"café\tx.up.sql"},
		},
		{
			reason: "a dir whose name holds dots is a relative tree path",
			dir:    "internal/storage/.schema",
			out:    "internal/storage/.schema/schema.txt\x00",
			want:   []string{"schema.txt"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()
			var gotDir string
			var gotArgs []string
			g := GitLister{Repo: testGitRepo, Run: func(_ context.Context, dir string, args ...string) ([]byte, error) {
				gotDir, gotArgs = dir, append([]string(nil), args...)
				return []byte(tt.out), nil
			}}
			got, err := g.ListTree(context.Background(), testPinnedCommit, tt.dir)
			if err != nil {
				t.Fatalf("ListTree returned error %v", err)
			}
			wantArgs := []string{"-C", testGitRepo, "ls-tree", "--name-only", "-z", testPinnedCommit, tt.dir + "/"}
			if !slices.Equal(gotArgs, wantArgs) {
				t.Errorf("git args = %q, want %q", gotArgs, wantArgs)
			}
			if gotDir != testGitRepo {
				t.Errorf("git dir = %q, want %q", gotDir, testGitRepo)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("ListTree = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPreRollGuardGitListerRefuses: a lister that cannot run, a value that
// cannot reach git's argv, a failed git and a listing with no entry are all
// errors, never a listing that reads as level zero.
func TestPreRollGuardGitListerRefuses(t *testing.T) {
	t.Parallel()
	runErr := errors.New("bdskew test: git exited 128")
	tests := []struct {
		reason    string
		hasRun    bool
		repo      string
		commit    string
		dir       string
		out       string
		runErr    error
		wantErr   error
		wantCalls int
	}{
		{
			reason: "no git runner", repo: testGitRepo,
			commit: testPinnedCommit, dir: MigrationsDir,
			wantErr: ErrNoRunner,
		},
		{
			reason: "no repository", hasRun: true, repo: "",
			commit: testPinnedCommit, dir: MigrationsDir,
			wantErr: ErrNoRepo,
		},
		{
			reason: "an empty commit", hasRun: true, repo: testGitRepo,
			commit: "", dir: MigrationsDir,
			wantErr: ErrBadCommit,
		},
		{
			reason: "an abbreviated commit", hasRun: true, repo: testGitRepo,
			commit: testPinnedCommit[:7], dir: MigrationsDir,
			wantErr: ErrBadCommit,
		},
		{
			reason: "an uppercase commit", hasRun: true, repo: testGitRepo,
			commit: strings.ToUpper(testPinnedCommit), dir: MigrationsDir,
			wantErr: ErrBadCommit,
		},
		{
			reason: "an option-shaped commit", hasRun: true, repo: testGitRepo,
			commit: "--upload-pack=sh", dir: MigrationsDir,
			wantErr: ErrBadCommit,
		},
		{
			reason: "an empty dir", hasRun: true, repo: testGitRepo,
			commit: testPinnedCommit, dir: "",
			wantErr: ErrBadDir,
		},
		{
			reason: "an absolute dir", hasRun: true, repo: testGitRepo,
			commit: testPinnedCommit, dir: "/" + MigrationsDir,
			wantErr: ErrBadDir,
		},
		{
			reason: "an option-shaped dir", hasRun: true, repo: testGitRepo,
			commit: testPinnedCommit, dir: "--name-only",
			wantErr: ErrBadDir,
		},
		{
			reason: "a dir that is only parent traversal", hasRun: true, repo: testGitRepo,
			commit: testPinnedCommit, dir: "..",
			wantErr: ErrBadDir,
		},
		{
			reason: "a dir that starts with parent traversal", hasRun: true, repo: testGitRepo,
			commit: testPinnedCommit, dir: "../" + MigrationsDir,
			wantErr: ErrBadDir,
		},
		{
			reason: "a dir with parent traversal inside it", hasRun: true, repo: testGitRepo,
			commit: testPinnedCommit, dir: "internal/../../etc",
			wantErr: ErrBadDir,
		},
		{
			reason: "git fails", hasRun: true, repo: testGitRepo,
			commit: testPinnedCommit, dir: MigrationsDir,
			runErr: runErr, wantErr: runErr, wantCalls: 1,
		},
		{
			reason: "git lists no entry", hasRun: true, repo: testGitRepo,
			commit: testPinnedCommit, dir: MigrationsDir,
			out: "", wantErr: ErrEmptyListing, wantCalls: 1,
		},
		{
			reason: "git lists only empty records", hasRun: true, repo: testGitRepo,
			commit: testPinnedCommit, dir: MigrationsDir,
			out: "\x00\x00", wantErr: ErrEmptyListing, wantCalls: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()
			calls := 0
			var run func(context.Context, string, ...string) ([]byte, error)
			if tt.hasRun {
				run = func(context.Context, string, ...string) ([]byte, error) {
					calls++
					return []byte(tt.out), tt.runErr
				}
			}
			g := GitLister{Run: run, Repo: tt.repo}
			got, err := g.ListTree(context.Background(), tt.commit, tt.dir)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ListTree error = %v, want %v", err, tt.wantErr)
			}
			if len(got) != 0 {
				t.Errorf("ListTree = %q with error %v, want no names", got, err)
			}
			if calls != tt.wantCalls {
				t.Errorf("git calls = %d, want %d", calls, tt.wantCalls)
			}
		})
	}
}

// TestPreRollGuardLevelAtCommitOverGitLister: LevelAtCommit reads a bd commit's
// level through the GitLister's git tree, and a tree with no entry is an error.
func TestPreRollGuardLevelAtCommitOverGitLister(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reason  string
		out     string
		want    int
		wantErr error
	}{
		{
			reason: "the samples build is level 67",
			out: "internal/storage/schema/migrations/0066_add_events_journal_actor.up.sql\x00" +
				"internal/storage/schema/migrations/0067_create_samples.up.sql\x00" +
				"internal/storage/schema/migrations/0067_create_samples.down.sql\x00",
			want: 67,
		},
		{
			reason:  "a tree with no entry is an error, not level zero",
			wantErr: ErrEmptyListing,
		},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()
			l := GitLister{Repo: testGitRepo, Run: func(context.Context, string, ...string) ([]byte, error) {
				return []byte(tt.out), nil
			}}
			got, err := LevelAtCommit(context.Background(), l, testPinnedCommit)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("LevelAtCommit error = %v, want %v", err, tt.wantErr)
				}
				if got != 0 {
					t.Errorf("LevelAtCommit = %d with error %v, want 0", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("LevelAtCommit returned error %v", err)
			}
			if got != tt.want {
				t.Errorf("LevelAtCommit = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestPreRollGuardBeadsRigRepo: the beads rig's bare clone sits under the town
// root in the layout the bd pin's commits are read from.
func TestPreRollGuardBeadsRigRepo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reason   string
		townRoot string
		want     string
	}{
		{reason: "a town root", townRoot: "/town", want: "/town/beads/.repo.git"},
		{reason: "a town root with a trailing separator", townRoot: "/town/", want: "/town/beads/.repo.git"},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()
			if got := BeadsRigRepo(tt.townRoot); got != tt.want {
				t.Errorf("BeadsRigRepo(%q) = %q, want %q", tt.townRoot, got, tt.want)
			}
		})
	}
}
