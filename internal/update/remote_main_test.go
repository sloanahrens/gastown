package update

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestRemoteMainSHA: the tip is the first field of the first ls-remote line,
// read through the bare repository's --git-dir; a failed run, a nonzero exit
// or an empty answer is an error that names the bare repository, never a blank.
func TestRemoteMainSHA(t *testing.T) {
	t.Parallel()

	const tip = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	const bare = "/town/gastown/.repo.git"
	tests := []struct {
		name    string
		output  string
		exit    int
		err     error
		want    string
		wantErr bool
	}{
		{"one line answers the tip", tip + "\trefs/heads/main\n", 0, nil, tip, false},
		{"first line wins", tip + "\trefs/heads/main\nother\trefs/heads/main\n", 0, nil, tip, false},
		{"nonzero exit is an error", tip + "\trefs/heads/main\n", 2, nil, "", true},
		{"empty answer is an error", "\n", 0, nil, "", true},
		{"blank answer is an error", "", 0, nil, "", true},
		{"runner error is an error", "", -1, errors.New("git is not installed"), "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var gotName string
			var gotArgs []string
			run := func(_ context.Context, dir string, _ []string, name string, args ...string) (string, int, error) {
				gotName, gotArgs = name, args
				if dir != "" {
					t.Errorf("ran in dir %q, want the bare repository passed as --git-dir", dir)
				}
				return tt.output, tt.exit, tt.err
			}

			sha, err := RemoteMainSHA(context.Background(), run, bare)
			if (err != nil) != tt.wantErr {
				t.Fatalf("RemoteMainSHA() error = %v, wantErr %v", err, tt.wantErr)
			}
			if sha != tt.want {
				t.Errorf("RemoteMainSHA() sha = %q, want %q", sha, tt.want)
			}
			if err != nil && !strings.Contains(err.Error(), bare) {
				t.Errorf("RemoteMainSHA() error = %q, want it to name the bare repository %s", err, bare)
			}
			if gotName != "git" || strings.Join(gotArgs, " ") != "--git-dir "+bare+" ls-remote origin refs/heads/main" {
				t.Errorf("ran %s %v, want git --git-dir %s ls-remote origin refs/heads/main", gotName, gotArgs, bare)
			}
		})
	}
}
