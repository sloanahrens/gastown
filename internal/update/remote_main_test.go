package update

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestRemoteMainSHA: the tip is the first field of the first ls-remote line;
// a failed run, a nonzero exit or an empty answer is an error, never a blank.
func TestRemoteMainSHA(t *testing.T) {
	t.Parallel()

	const tip = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
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
					t.Errorf("ran in dir %q, want the repo root passed as -C", dir)
				}
				return tt.output, tt.exit, tt.err
			}

			sha, err := RemoteMainSHA(context.Background(), run, "/town/repo")
			if (err != nil) != tt.wantErr {
				t.Fatalf("RemoteMainSHA() error = %v, wantErr %v", err, tt.wantErr)
			}
			if sha != tt.want {
				t.Errorf("RemoteMainSHA() sha = %q, want %q", sha, tt.want)
			}
			if gotName != "git" || strings.Join(gotArgs, " ") != "-C /town/repo ls-remote origin refs/heads/main" {
				t.Errorf("ran %s %v, want git -C /town/repo ls-remote origin refs/heads/main", gotName, gotArgs)
			}
		})
	}
}
