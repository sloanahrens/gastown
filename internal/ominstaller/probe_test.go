package ominstaller

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/guard"
)

// fakeExec answers every command with out, stderr and err. No process starts.
type fakeExec struct {
	out    string
	stderr string
	err    error
}

func (f *fakeExec) config() Config {
	return Config{Exec: func(_ context.Context, _ Command) (string, string, error) {
		return f.out, f.stderr, f.err
	}}
}

const headSHAForTest = "0123456789abcdef0123456789abcdef01234567"

func TestGitDirtyCountsOnlyTrackedEdits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		out  string
		want bool
	}{
		{name: "a tracked edit is dirty", out: " M internal/ominstaller/resolve.go\n", want: true},
		{name: "a staged tracked edit is dirty", out: "M  probe.go\n", want: true},
		{name: "an untracked file alone is clean", out: "?? scratch/new.go\n", want: false},
		{name: "an untracked file beside a tracked edit is dirty", out: "?? new.go\n M old.go\n", want: true},
		{name: "an empty status is clean", out: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := (&fakeExec{out: tc.out}).config()
			got, err := cfg.gitDirty(context.Background(), "/clone")
			if err != nil {
				t.Fatalf("gitDirty: %v", err)
			}
			if got != tc.want {
				t.Errorf("gitDirty = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGitDirtyFailsClosed(t *testing.T) {
	t.Parallel()
	// git cannot run: the probe must return an error, so the install refuses
	// rather than reading the tree as clean.
	cfg := (&fakeExec{err: errors.New("exec: git: not found")}).config()

	dirty, err := cfg.gitDirty(context.Background(), "/clone")

	if err == nil {
		t.Fatalf("gitDirty returned no error (dirty=%v), want an error so the install is refused", dirty)
	}
}

func TestGitHeadRefusesAnUnreadableHead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		out     string
		execErr error
		wantSHA string
		wantErr bool
	}{
		{name: "a full commit name is returned", out: headSHAForTest + "\n", wantSHA: headSHAForTest},
		{name: "git printed nothing", out: "", wantErr: true},
		{name: "git printed a name that is not a commit", out: "HEAD\n", wantErr: true},
		{name: "git could not run", execErr: errors.New("exit status 128"), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := (&fakeExec{out: tc.out, err: tc.execErr}).config()
			sha, err := cfg.gitHead(context.Background(), "/clone")
			if (err != nil) != tc.wantErr {
				t.Fatalf("gitHead error = %v, want error %v", err, tc.wantErr)
			}
			if sha != tc.wantSHA {
				t.Errorf("gitHead = %q, want %q", sha, tc.wantSHA)
			}
		})
	}
}

// verdict names the state of a guard.Result for a table row.
func verdict(r guard.Result) string {
	switch {
	case r.IsPass():
		return "pass"
	case r.IsFail():
		return "fail"
	default:
		return "unknown"
	}
}

func TestGitRepo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		out    string
		stderr string
		err    error
		want   string
	}{
		{name: "inside a work tree passes", out: "true\n", want: "pass"},
		{name: "inside a git directory, not a work tree, fails", out: "false\n", want: "fail"},
		{
			name:   "outside every repository fails, and is not unknown",
			stderr: "fatal: not a git repository (or any of the parent directories): .git\n",
			err:    errors.New("exit status 128"),
			want:   "fail",
		},
		{name: "git could not run is unknown", err: errors.New("exec: git: not found"), want: "unknown"},
		{name: "git failed for another reason is unknown", stderr: "fatal: index file corrupt\n", err: errors.New("exit status 128"), want: "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := (&fakeExec{out: tc.out, stderr: tc.stderr, err: tc.err}).config()
			if got := verdict(cfg.gitRepo(context.Background(), "/clone")); got != tc.want {
				t.Errorf("gitRepo = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestVersionSHAParsesTheVersionLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		out  string
		want string
	}{
		{name: "seven hex characters", out: "om version 1a2b3c4\n", want: "1a2b3c4"},
		{name: "forty hex characters", out: "om version " + headSHAForTest + "\n", want: headSHAForTest},
		{name: "a line after other output", out: "warning: stale cache\nom version 1a2b3c4\n", want: "1a2b3c4"},
		{name: "six hex characters is too short", out: "om version 1a2b3c\n", want: ""},
		{name: "forty-one hex characters is too long", out: "om version " + headSHAForTest + "9\n", want: ""},
		{name: "uppercase hex is rejected", out: "om version 1A2B3C4\n", want: ""},
		{name: "another program's line is rejected", out: "gom version 1a2b3c4\n", want: ""},
		{name: "no output", out: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseVersionSHA(tc.out); got != tc.want {
				t.Errorf("parseVersionSHA = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInstalledCommitReadsTheVersionLine(t *testing.T) {
	t.Parallel()
	cfg := (&fakeExec{out: "om version 1a2b3c4\n"}).config()

	if got := cfg.installedCommit(context.Background(), "/home/op/go/bin/om"); got != "1a2b3c4" {
		t.Errorf("installedCommit = %q, want 1a2b3c4", got)
	}
}

func TestInstalledCommitIsUnknownWhenOmCannotRun(t *testing.T) {
	t.Parallel()
	cfg := (&fakeExec{err: errors.New("exit status 127")}).config()

	if got := cfg.installedCommit(context.Background(), "/no/such/om"); got != "" {
		t.Errorf("installedCommit = %q, want \"\" so the previous commit is recorded as unknown", got)
	}
}
