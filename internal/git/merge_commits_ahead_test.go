package git

import "testing"

// MergeCommitsAhead counts the merge commits in base..branch. A failed
// rev-list and an unreadable count are both errors: zero is the answer only
// when git said so, never when the read failed (gt-eyy06f).
func TestMergeCommitsAhead(t *testing.T) {
	t.Parallel()

	const call = "rev-list --count --merges origin/main..abc123"
	tests := []struct {
		name    string
		reply   reply
		want    int
		wantErr bool
	}{
		{name: "merges beyond the base", reply: ok("2\n"), want: 2},
		{name: "no merge beyond the base", reply: ok("0\n"), want: 0},
		{name: "rev-list fails", reply: fail(128, "fatal: bad revision"), wantErr: true},
		{name: "unreadable count", reply: ok("not a number\n"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := &Git{workDir: t.TempDir(), exec: newScripted(map[string]reply{call: tt.reply}).run}

			got, err := g.MergeCommitsAhead("origin/main", "abc123")
			if (err != nil) != tt.wantErr {
				t.Fatalf("MergeCommitsAhead error = %v, want an error: %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("MergeCommitsAhead = %d, want %d", got, tt.want)
			}
		})
	}
}
