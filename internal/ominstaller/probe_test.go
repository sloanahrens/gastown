package ominstaller

import (
	"context"
	"errors"
	"testing"
)

// fakeExec answers every command with out and err, and records the commands it
// was asked to run. No process starts.
type fakeExec struct {
	out string
	err error
}

func (f *fakeExec) config() Config {
	return Config{Exec: func(_ context.Context, _ Command) (string, string, error) {
		return f.out, "", f.err
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

func TestGitRepoFailsClosed(t *testing.T) {
	t.Parallel()
	cfg := (&fakeExec{err: errors.New("exit status 128")}).config()

	if _, err := cfg.gitRepo(context.Background(), "/clone"); err == nil {
		t.Error("gitRepo returned no error when git could not run, want an error")
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

func TestInstalledCommitIsUnknownWhenOmCannotRun(t *testing.T) {
	t.Parallel()
	cfg := (&fakeExec{err: errors.New("exit status 127")}).config()

	if got := cfg.installedCommit(context.Background(), "/no/such/om"); got != "" {
		t.Errorf("installedCommit = %q, want \"\" so the previous commit is recorded as unknown", got)
	}
}
