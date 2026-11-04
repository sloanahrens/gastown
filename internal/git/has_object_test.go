package git

import "testing"

// HasObject answers true for a commit git holds and false, with no error, for
// one it does not (cat-file -e exits 1). An object that is not a commit, and
// any other failure, is an error: an unreadable repository is never read as a
// missing commit.
func TestHasObject(t *testing.T) {
	t.Parallel()

	const sha = "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name    string
		exists  reply
		kind    reply
		want    bool
		wantErr bool
	}{
		{name: "commit held", exists: ok(""), kind: ok("commit\n"), want: true},
		{name: "missing", exists: fail(1, "")},
		{name: "repository unreadable", exists: fail(128, "fatal: not a git repository"), wantErr: true},
		{name: "tree is not a commit", exists: ok(""), kind: ok("tree\n"), wantErr: true},
		{name: "type unreadable", exists: ok(""), kind: fail(128, "fatal: bad object"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newScripted(map[string]reply{
				"cat-file -e " + sha: tt.exists,
				"cat-file -t " + sha: tt.kind,
			})
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
