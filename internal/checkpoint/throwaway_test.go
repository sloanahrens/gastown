package checkpoint

import (
	"strings"
	"testing"
)

func TestIsThrowawayPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		want bool
	}{
		// The gt-h1tq/gt-ozo4 report's own file.
		{"livecheck scratch test", "internal/util/zz_livecheck_test.go", true},
		{"zz prefix at root", "zz_probe.go", true},
		{"zz infix", "internal/foo/bar_zz_probe.go", true},
		{"tmp copy", "internal/util/client_tmp.go", true},
		{"tmp without a boundary", "client_tmpbak.go", false},
		{"tmp suffix", "internal/util/client_tmp", true},
		{"tmp infix", "client_tmp_v2.go", true},
		{".tmp extension", "scratch.tmp", true},
		{".temp extension", "notes.temp", true},
		{"emacs backup", "internal/util/client.go~", true},
		{"bak", "config.bak", true},
		{"orig", "patch.orig", true},
		{"rej", "patch.rej", true},
		{"vim swap", ".client.go.swp", true},
		{"vim swap swo", "client.swo", true},
		{"emacs lock", ".#client.go", true},
		{"emacs autosave", "#client.go#", true},
		{"scratch dir", "scratch/notes.md", true},
		{"tmp dir", "cmd/tmp/probe.go", true},
		{"scratchpad dir", "scratchpad/report.txt", true},
		{"dir segment case-insensitive", "Scratch/notes.md", true},

		// Real work must never be swept up.
		{"go source", "internal/checkpoint/throwaway.go", false},
		{"test source", "internal/checkpoint/throwaway_test.go", false},
		{"go template", "foo_tmpl.go", false},
		{"tmpl dir", "internal/tmpl/render.go", false},
		{"markdown", "docs/design/notes.md", false},
		{"nested real dir", "internal/daemon/checkpoint_dog.go", false},
		{"dotfile", ".gitignore", false},
		{"name containing tmp without boundary", "attempt.go", false},
		{"empty", "", false},
		{"root-level go file", "main.go", false},

		// Directory segments only count when they are a whole segment.
		{"tmp as a name prefix", "tmpdir/file.go", false},
		{"tmp inside a segment", "internal/attmp/file.go", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsThrowawayPath(tt.path); got != tt.want {
				t.Errorf("IsThrowawayPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestThrowawayPaths(t *testing.T) {
	t.Parallel()
	got := ThrowawayPaths([]string{
		"internal/util/client.go",
		"internal/util/zz_probe_test.go",
		"internal/util/zz_probe_test.go", // duplicate
		"scratch/notes.md",
		"internal/util/server.go_tmp",
	})
	want := []string{"internal/util/zz_probe_test.go", "scratch/notes.md", "internal/util/server.go_tmp"}
	if len(got) != len(want) {
		t.Fatalf("ThrowawayPaths() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ThrowawayPaths()[%d] = %q, want %q (all: %#v)", i, got[i], want[i], got)
		}
	}
}

// TestAddedThrowawayPaths_ReportsOnlyAdditions pins the boundary of the rule: a
// repository that already tracks a matching name keeps working, because a
// modification is real work and only an addition is something the branch brings
// to the target.
func TestAddedThrowawayPaths_ReportsOnlyAdditions(t *testing.T) {
	t.Parallel()
	r := newFakeRepo(t)
	// Tracked on the base branch, name notwithstanding.
	base := r.commit("tracked fixture", "zz_fixture_test.go", "package main\n")

	r.checkoutNew("polecat/garnet/gt-ozo4")
	// Modify the tracked file: not an addition, so not reported.
	r.commit("real work on the tracked fixture", "zz_fixture_test.go", "package main\n\n// real work\n")
	r.commit("real work", "internal/util/client.go", "package util\n")
	r.commit(WIPCommitPrefix, "internal/util/zz_livecheck_test.go", "package util\n")
	r.commit(WIPCommitPrefix, "notes.tmp", "scratch\n")

	found, err := addedThrowawayPaths(r.run, fakeDir, base, "HEAD")
	if err != nil {
		t.Fatalf("addedThrowawayPaths: %v", err)
	}
	want := []string{"internal/util/zz_livecheck_test.go", "notes.tmp"}
	if strings.Join(found, "|") != strings.Join(want, "|") {
		t.Fatalf("addedThrowawayPaths() = %#v, want %#v", found, want)
	}
}

// The listing must ask git for additions only, with rename detection off and
// NUL-terminated paths; the integration tier pins what git does with them.
func TestAddedThrowawayPaths_AsksForAdditionsWithoutRenames(t *testing.T) {
	t.Parallel()
	r := newFakeRepo(t)
	base := r.headCommit()
	r.checkoutNew("polecat/garnet/gt-ozo4")
	r.commit(WIPCommitPrefix, "notes.tmp", "scratch\n")

	if _, err := addedThrowawayPaths(r.run, fakeDir, base, "HEAD"); err != nil {
		t.Fatal(err)
	}
	want := "diff --name-only --no-renames --diff-filter=A -z " + base + " HEAD"
	var got []string
	for _, c := range r.calls {
		got = append(got, strings.Join(c, " "))
	}
	if len(got) != 2 || got[1] != want {
		t.Errorf("calls = %q, want merge-base then %q", got, want)
	}
}

func TestAddedThrowawayPaths_CleanBranch(t *testing.T) {
	t.Parallel()
	r := newFakeRepo(t)
	base := r.headCommit()
	r.checkoutNew("polecat/garnet/gt-ozo4")
	r.commit("real work (gt-ozo4)", "internal/util/client.go", "package util\n")

	found, err := addedThrowawayPaths(r.run, fakeDir, base, "HEAD")
	if err != nil {
		t.Fatalf("addedThrowawayPaths: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("addedThrowawayPaths() = %#v, want none", found)
	}
}

// TestAddedThrowawayPaths_CommittedDeletionClears pins the remediation the
// refusal prescribes: once the deletion is committed the file is gone from
// HEAD's tree and the answer goes quiet. An uncommitted deletion changes
// nothing, because the answer reads HEAD's tree, never the working tree.
func TestAddedThrowawayPaths_CommittedDeletionClears(t *testing.T) {
	t.Parallel()
	r := newFakeRepo(t)
	base := r.headCommit()
	r.checkoutNew("polecat/garnet/gt-ozo4")
	r.commit(WIPCommitPrefix, "internal/util/zz_livecheck_test.go", "package util\n")

	found, err := addedThrowawayPaths(r.run, fakeDir, base, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0] != "internal/util/zz_livecheck_test.go" {
		t.Fatalf("before the deletion: %#v, want the scratch file reported", found)
	}

	r.remove("remove throwaway files", "internal/util/zz_livecheck_test.go")
	found, err = addedThrowawayPaths(r.run, fakeDir, base, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("addedThrowawayPaths() = %#v, want none after the deletion was committed", found)
	}
}

// TestAddedThrowawayPaths_UnresolvableBaseFailsClosed is the fail-closed
// contract: an answer the caller cannot get is not the empty answer. The
// submit gate refuses on this error rather than landing the branch unchecked.
func TestAddedThrowawayPaths_UnresolvableBaseFailsClosed(t *testing.T) {
	t.Parallel()
	r := newFakeRepo(t)

	if _, err := addedThrowawayPaths(r.run, fakeDir, "origin/main", "HEAD"); err == nil {
		t.Fatal("addedThrowawayPaths with an unresolvable baseRef returned nil error, want failure")
	}
}

func TestAddedThrowawayPaths_KeepsWhitespaceAndNewlinesInPaths(t *testing.T) {
	t.Parallel()
	r := newFakeRepo(t)
	base := r.headCommit()
	r.checkoutNew("polecat/garnet/gt-ozo4")
	r.commit(WIPCommitPrefix, "scratch/notes copy.md", "scratch\n")
	r.commit(WIPCommitPrefix, "scratch/two\nlines.md", "scratch\n")
	r.commit(WIPCommitPrefix, "zz_trailing ", "scratch\n")

	found, err := addedThrowawayPaths(r.run, fakeDir, base, "HEAD")
	if err != nil {
		t.Fatalf("addedThrowawayPaths: %v", err)
	}
	want := []string{"scratch/notes copy.md", "scratch/two\nlines.md", "zz_trailing "}
	if strings.Join(found, "|") != strings.Join(want, "|") {
		t.Fatalf("addedThrowawayPaths() = %#v, want %#v", found, want)
	}
}

func TestSplitNullSeparated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a\x00", []string{"a"}},
		{"a b\x00c\nd\x00", []string{"a b", "c\nd"}},
	}
	for _, tt := range tests {
		got := splitNullSeparated(tt.in)
		if strings.Join(got, "|") != strings.Join(tt.want, "|") || len(got) != len(tt.want) {
			t.Errorf("splitNullSeparated(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
