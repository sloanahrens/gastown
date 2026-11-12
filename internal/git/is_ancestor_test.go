package git

import "testing"

// IsAncestor reads merge-base --is-ancestor's answers apart by exit status:
// 0 is an ancestor, 1 is not, and any other failure is an error, never a
// quiet "not an ancestor". A fatal git error that printed nothing carries the
// text "exit status 128", which contains "exit status 1", so matching the
// error text would read that fatal error as no.
func TestIsAncestor(t *testing.T) {
	t.Parallel()

	const call = "merge-base --is-ancestor abc123 def456"
	tests := []struct {
		name    string
		reply   reply
		want    bool
		wantErr bool
	}{
		{name: "ancestor", reply: ok(""), want: true},
		{name: "not an ancestor", reply: fail(1, "")},
		{name: "repository unreadable", reply: fail(128, "fatal: not a git repository"), wantErr: true},
		{name: "fatal exit with no stderr", reply: fail(128, ""), wantErr: true},
		{name: "commit name bad", reply: fail(128, "fatal: Not a valid object name abc123"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := &Git{workDir: t.TempDir(), exec: newScripted(map[string]reply{call: tt.reply}).run}

			got, err := g.IsAncestor("abc123", "def456")
			if (err != nil) != tt.wantErr {
				t.Fatalf("IsAncestor error = %v, want an error: %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("IsAncestor = %v, want %v", got, tt.want)
			}
		})
	}
}
