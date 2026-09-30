package git

import (
	"reflect"
	"testing"
)

func TestStagedChangesParsesNameStatus(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"diff --cached --name-status --no-renames -z": ok("M\x00src/app.go\x00A\x00new file.txt\x00D\x00gone.txt\x00"),
	})
	got, err := newTestGit(t, s).StagedChanges()
	want := []StagedChange{{'M', "src/app.go"}, {'A', "new file.txt"}, {'D', "gone.txt"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("StagedChanges = %v, %v; want %v", got, err, want)
	}
}

func TestStagedChangesEmptyIndexDiff(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"diff --cached --name-status --no-renames -z": ok("")})
	if got, err := newTestGit(t, s).StagedChanges(); err != nil || len(got) != 0 {
		t.Fatalf("StagedChanges = %v, %v; want none", got, err)
	}
}

func TestStagedChangesFailsOutsideARepo(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"diff --cached --name-status --no-renames -z": fail(129, "error: unknown option `cached'")})
	if _, err := newTestGit(t, s).StagedChanges(); err == nil {
		t.Fatal("StagedChanges succeeded on a failed diff")
	}
}

func TestWriteTreeReturnsTheTreeID(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"write-tree": ok("4b825dc642cb6eb9a060e54bf8d69288fbee4904\n")})
	if got, err := newTestGit(t, s).WriteTree(); err != nil || got != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
		t.Fatalf("WriteTree = %q, %v", got, err)
	}
}
