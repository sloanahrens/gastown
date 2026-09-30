package plugin

import (
	"reflect"
	"testing"
)

// TestParseRawLog checks the blob history read out of `git log --raw
// --no-abbrev`: both sides of a change count, the all-zero id of an add or
// delete does not, and commit-header and blank lines are skipped.
func TestParseRawLog(t *testing.T) {
	t.Parallel()
	const (
		v1 = "1111111111111111111111111111111111111111"
		v2 = "2222222222222222222222222222222222222222"
		v3 = "3333333333333333333333333333333333333333"
		zz = "0000000000000000000000000000000000000000"
	)
	out := "\n" +
		":100644 100644 " + v1 + " " + v2 + " M\tplugins/p/plugin.md\n" +
		"\n" +
		":000000 100755 " + zz + " " + v3 + " A\tplugins/p/run.sh\n" +
		":100644 000000 " + v1 + " " + zz + " D\tplugins/old/plugin.md\n" +
		"not a raw line\n"
	want := blobHistory{
		"plugins/p/plugin.md":   {v1: true, v2: true},
		"plugins/p/run.sh":      {v3: true},
		"plugins/old/plugin.md": {v1: true},
	}
	if got := parseRawLog([]byte(out)); !reflect.DeepEqual(got, want) {
		t.Errorf("parseRawLog = %v, want %v", got, want)
	}
}

// TestGitBlobID pins gitBlobID to git's own object id: `git hash-object`
// of "hello\n" is ce0136250e...
func TestGitBlobID(t *testing.T) {
	t.Parallel()
	if got, want := gitBlobID([]byte("hello\n")), "ce013625030ba8dba906f756967f9e9ca394464a"; got != want {
		t.Errorf("gitBlobID(hello) = %s, want %s", got, want)
	}
}
