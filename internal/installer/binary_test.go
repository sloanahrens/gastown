package installer

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStaleCommit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"the binary commit is read", `{"stale": false, "binary_commit": "c1"}`, "c1"},
		{"a missing field is nothing", `{"stale": true}`, ""},
		{"junk is nothing", "not json", ""},
		{"an array is nothing", `["c1"]`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := staleCommit([]byte(tc.out)); got != tc.want {
				t.Errorf("staleCommit(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}

// The version fallback has two forms: with the ref before the commit, and,
// from a cwd outside a repo, with only "(dev: <commit>)" (gt-b5mpe).
func TestVersionCommit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"a line with the branch", "gt version 1.2.3 (dev: main@abc1234)\n", "abc1234"},
		{"a dev line outside a repo", "gt version 1.2.3 (dev: abc1234)\n", "abc1234"},
		{"a full hash", "gt version 1.2.3 (dev: main@abc1234def5678abc1234def5678abc1234def5)\n", "abc1234def5678abc1234def5678abc1234def5"},
		{"no parenthesised commit", "gt version 1.2.3\n", ""},
		{"a commit too short to be one", "gt version 1.2.3 (dev: main@abc12)", ""},
		{"an empty line", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := versionCommit([]byte(tc.out)); got != tc.want {
				t.Errorf("versionCommit(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}

// StaleBinary reads 'gt stale --json' first and 'gt version' only when that
// names nothing, and leaves a binary that is not executable alone.
func TestStaleBinary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		mode          os.FileMode
		staleOut      string
		staleErrOut   string
		staleErr      error
		versionOut    string
		versionErrOut string
		want          string
		wantCalls     []string
	}{
		{
			name:      "the stale read answers",
			mode:      0o755,
			staleOut:  `{"binary_commit": "c1111111"}`,
			want:      "c1111111",
			wantCalls: []string{"stale --json"},
		},
		{
			name:       "a stale read with no commit falls back",
			mode:       0o755,
			staleOut:   `{"stale": true}`,
			versionOut: "gt version 1.0.0 (dev: main@c2222222)\n",
			want:       "c2222222",
			wantCalls:  []string{"stale --json", "version"},
		},
		{
			name:       "a failing stale read falls back",
			mode:       0o755,
			staleErr:   errors.New("exit 1"),
			versionOut: "gt version 1.0.0 (dev: main@c2222222)\n",
			want:       "c2222222",
			wantCalls:  []string{"stale --json", "version"},
		},
		{
			// The version fallback reads both streams: a gt that says it on
			// stderr still answers, as the read before the port did.
			name:          "a version line on stderr answers",
			mode:          0o755,
			staleOut:      `{"stale": true}`,
			versionErrOut: "gt version 1.0.0 (dev: main@c2222222)\n",
			want:          "c2222222",
			wantCalls:     []string{"stale --json", "version"},
		},
		{
			// The stale read does not: that stream has to be the JSON and
			// nothing else.
			name:        "a stale answer on stderr does not count",
			mode:        0o755,
			staleErrOut: `{"binary_commit": "c1111111"}`,
			want:        "",
			wantCalls:   []string{"stale --json", "version"},
		},
		{
			name:      "neither read answers",
			mode:      0o755,
			staleOut:  "junk",
			want:      "",
			wantCalls: []string{"stale --json", "version"},
		},
		{
			name: "an unexecutable file is not run",
			mode: 0o644,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gt := filepath.Join(t.TempDir(), "gt")
			if err := os.WriteFile(gt, nil, tc.mode); err != nil {
				t.Fatal(err)
			}
			var calls []string
			run := func(dir, name string, args ...string) ([]byte, []byte, int) {
				if dir != "/rig" || name != gt {
					t.Errorf("ran %q from %q, want %q from /rig", name, dir, gt)
				}
				call := ""
				for i, arg := range args {
					if i > 0 {
						call += " "
					}
					call += arg
				}
				calls = append(calls, call)
				if args[0] == "stale" {
					if tc.staleErr != nil {
						return []byte(tc.staleOut), nil, 1
					}
					return []byte(tc.staleOut), []byte(tc.staleErrOut), 0
				}
				return []byte(tc.versionOut), []byte(tc.versionErrOut), 0
			}

			got := StaleBinary(run).Commit("/rig", gt)

			if got != tc.want {
				t.Errorf("Commit() = %q, want %q", got, tc.want)
			}
			if !reflect.DeepEqual(calls, tc.wantCalls) {
				t.Errorf("calls = %q, want %q", calls, tc.wantCalls)
			}
		})
	}
}
