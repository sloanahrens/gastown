package git

import "testing"

// HasObject answers true for an object git holds and false, with no error, for
// one it does not (cat-file -e exits 1). Any other exit is an error, so an
// unreadable repository is never read as a missing object.
func TestHasObject(t *testing.T) {
	t.Parallel()

	const sha = "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name    string
		reply   reply
		want    bool
		wantErr bool
	}{
		{name: "held", reply: ok(""), want: true},
		{name: "missing", reply: fail(1, "")},
		{name: "repository unreadable", reply: fail(128, "fatal: not a git repository"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newScripted(map[string]reply{"cat-file -e " + sha: tt.reply})
			g := &Git{workDir: t.TempDir(), exec: s.run}

			got, err := g.HasObject(sha)
			if (err != nil) != tt.wantErr {
				t.Fatalf("HasObject error = %v, want an error: %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("HasObject = %v, want %v", got, tt.want)
			}
		})
	}
}
