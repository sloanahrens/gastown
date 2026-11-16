package git

import (
	"fmt"
	"strings"
	"testing"
)

// grepCallKey is the scripted key for the git grep call that counts paths.
func grepCallKey(paths ...string) string {
	return strings.Join(append([]string{"--literal-pathspecs", "grep", "--no-index", "-c", "-z", "", "--"}, paths...), " ")
}

// grepRecords is git grep -c -z output counting one line in each path.
func grepRecords(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString(p + "\x001\n")
	}
	return b.String()
}

// TestRunawayBeadAddedLinesSinceCountsTrackedAndUntracked: the added column of
// the tracked numstat adds to the line counts of the untracked files, and a
// binary file's "-" adds nothing.
func TestRunawayBeadAddedLinesSinceCountsTrackedAndUntracked(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"diff --numstat --no-renames --end-of-options main": ok("3\t0\tinternal/git/a.go\n1\t2\tinternal/git/b.go\n-\t-\tlogo.png\n"),
		"ls-files --others --exclude-standard -z":           ok("notes/empty.txt\x00notes/new.go\x00"),
		grepCallKey("notes/empty.txt", "notes/new.go"):      ok("notes/new.go\x002\n"),
	})
	got, err := NewGitWithExec(t.TempDir(), s.run).AddedLinesSince("main")
	if err != nil {
		t.Fatalf("AddedLinesSince: %v", err)
	}
	// 3 + 1 tracked, 0 for the binary, 2 untracked.
	if got != 6 {
		t.Errorf("AddedLinesSince = %d, want 6", got)
	}
	s.noUnscripted(t)
	want := []string{
		"diff --numstat --no-renames --end-of-options main",
		"ls-files --others --exclude-standard -z",
		grepCallKey("notes/empty.txt", "notes/new.go"),
	}
	if diff := strings.Join(s.sent(), "\n"); diff != strings.Join(want, "\n") {
		t.Errorf("git calls:\n%s\nwant:\n%s", diff, strings.Join(want, "\n"))
	}
}

// TestRunawayBeadAddedLinesSinceFailsWithoutCount: a git command that fails
// returns an error naming the command and no count, for each of the three
// commands the method runs.
func TestRunawayBeadAddedLinesSinceFailsWithoutCount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		answers map[string]reply
		wantCmd string
	}{
		{
			name:    "diff fails",
			answers: map[string]reply{"diff --numstat --no-renames --end-of-options main": fail(128, "fatal: bad revision 'main'")},
			wantCmd: "git diff --numstat --no-renames --end-of-options main",
		},
		{
			name: "ls-files fails",
			answers: map[string]reply{
				"diff --numstat --no-renames --end-of-options main": ok("2\t0\tx.go\n"),
				"ls-files --others --exclude-standard -z":           fail(128, "fatal: not a git repository"),
			},
			wantCmd: "git ls-files --others --exclude-standard",
		},
		{
			name: "grep fails",
			answers: map[string]reply{
				"diff --numstat --no-renames --end-of-options main": ok("2\t0\tx.go\n"),
				"ls-files --others --exclude-standard -z":           ok("y.txt\x00"),
				grepCallKey("y.txt"):                                fail(2, "fatal: cannot use --no-index outside a repository"),
			},
			wantCmd: "git grep --no-index -c",
		},
		{
			// Exit 1 alone is git grep's "no match", but a command that failed
			// behind a wrapper can leave the same status, so a message on
			// either stream makes it a failure, never a zero count.
			name: "grep exits 1 with a message on stderr",
			answers: map[string]reply{
				"diff --numstat --no-renames --end-of-options main": ok("2\t0\tx.go\n"),
				"ls-files --others --exclude-standard -z":           ok("y.txt\x00"),
				grepCallKey("y.txt"):                                fail(1, "error: sandbox exec failed"),
			},
			wantCmd: "git grep --no-index -c",
		},
		{
			name: "grep exits 1 with output on stdout",
			answers: map[string]reply{
				"diff --numstat --no-renames --end-of-options main": ok("2\t0\tx.go\n"),
				"ls-files --others --exclude-standard -z":           ok("y.txt\x00"),
				grepCallKey("y.txt"):                                {stdout: "y.txt\x001\n", code: 1},
			},
			wantCmd: "git grep --no-index -c",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newScripted(tc.answers)
			got, err := NewGitWithExec(t.TempDir(), s.run).AddedLinesSince("main")
			if err == nil {
				t.Fatalf("AddedLinesSince = %d, nil; want an error", got)
			}
			if got != 0 {
				t.Errorf("AddedLinesSince = %d with an error; want 0, never a partial count", got)
			}
			if !strings.Contains(err.Error(), tc.wantCmd) {
				t.Errorf("error %q does not name %q", err, tc.wantCmd)
			}
			s.noUnscripted(t)
		})
	}
}

// TestRunawayBeadAddedLinesSinceCleanCloneIsZero: a clone that adds nothing
// counts 0 and never runs git grep, which with no path would search the whole
// worktree.
func TestRunawayBeadAddedLinesSinceCleanCloneIsZero(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"diff --numstat --no-renames --end-of-options main": ok(""),
		"ls-files --others --exclude-standard -z":           ok(""),
	})
	got, err := NewGitWithExec(t.TempDir(), s.run).AddedLinesSince("main")
	if err != nil || got != 0 {
		t.Fatalf("AddedLinesSince = %d, %v; want 0, nil", got, err)
	}
	s.noUnscripted(t)
	for _, call := range s.sent() {
		if strings.HasPrefix(call, "--literal-pathspecs grep ") {
			t.Errorf("ran %q with no untracked file to count", call)
		}
	}
}

// TestRunawayBeadAddedLinesSinceBaseIsNotAnOption: a base that begins with "-"
// reaches git behind --end-of-options, so git refuses it instead of running it
// as an option.
func TestRunawayBeadAddedLinesSinceBaseIsNotAnOption(t *testing.T) {
	t.Parallel()
	const base = "--output=evil"
	call := "diff --numstat --no-renames --end-of-options " + base
	s := newScripted(map[string]reply{
		call: fail(128, "fatal: option '"+base+"' must come before non-option arguments"),
	})
	got, err := NewGitWithExec(t.TempDir(), s.run).AddedLinesSince(base)
	if err == nil {
		t.Fatalf("AddedLinesSince(%q) = %d, nil; want an error", base, got)
	}
	if got != 0 {
		t.Errorf("AddedLinesSince(%q) = %d with an error; want 0", base, got)
	}
	if !strings.Contains(err.Error(), call) {
		t.Errorf("error %q does not name %q", err, call)
	}
	s.noUnscripted(t)
	if !s.hasSent(call) {
		t.Errorf("git was sent %q; want %q", s.sent(), call)
	}
}

// TestRunawayBeadAddedLinesSinceUntrackedPathsAreLiteral: an untracked file
// whose name holds a glob character reaches git grep as itself, behind
// --literal-pathspecs, so it is not read as a pathspec matching other files.
func TestRunawayBeadAddedLinesSinceUntrackedPathsAreLiteral(t *testing.T) {
	t.Parallel()
	const path = "star*.txt"
	s := newScripted(map[string]reply{
		"diff --numstat --no-renames --end-of-options main": ok(""),
		"ls-files --others --exclude-standard -z":           ok(path + "\x00"),
		grepCallKey(path): ok(path + "\x002\n"),
	})
	got, err := NewGitWithExec(t.TempDir(), s.run).AddedLinesSince("main")
	if err != nil {
		t.Fatalf("AddedLinesSince: %v", err)
	}
	if got != 2 {
		t.Errorf("AddedLinesSince = %d, want 2", got)
	}
	s.noUnscripted(t)
	if !s.hasSent(grepCallKey(path)) {
		t.Errorf("git grep was not sent %q; an untracked path must be literal", grepCallKey(path))
	}
}

// TestRunawayBeadAddedLinesSincePathWithNewlineCounts: an untracked file whose
// name holds a newline still yields its count, because the name is cut at the
// NUL and not at the newline inside it.
func TestRunawayBeadAddedLinesSincePathWithNewlineCounts(t *testing.T) {
	t.Parallel()
	name := "we\nird.txt"
	s := newScripted(map[string]reply{
		"diff --numstat --no-renames --end-of-options main": ok(""),
		"ls-files --others --exclude-standard -z":           ok(name + "\x00"),
		grepCallKey(name): ok(name + "\x002\n"),
	})
	got, err := NewGitWithExec(t.TempDir(), s.run).AddedLinesSince("main")
	if err != nil || got != 2 {
		t.Fatalf("AddedLinesSince = %d, %v; want 2, nil", got, err)
	}
	s.noUnscripted(t)
}

// TestRunawayBeadAddedLinesSinceBatchesUntrackedPaths: more untracked paths
// than one argv may carry are counted in batches, and every batch's count adds
// to the total.
func TestRunawayBeadAddedLinesSinceBatchesUntrackedPaths(t *testing.T) {
	t.Parallel()
	names := make([]string, 0, grepBatchPaths+1)
	for i := 0; i <= grepBatchPaths; i++ {
		names = append(names, fmt.Sprintf("u/%d.txt", i))
	}
	first, second := names[:grepBatchPaths], names[grepBatchPaths:]
	s := newScripted(map[string]reply{
		"diff --numstat --no-renames --end-of-options main": ok(""),
		"ls-files --others --exclude-standard -z":           ok(strings.Join(names, "\x00") + "\x00"),
		grepCallKey(first...):                               ok(grepRecords(first)),
		grepCallKey(second...):                              ok(grepRecords(second)),
	})
	got, err := NewGitWithExec(t.TempDir(), s.run).AddedLinesSince("main")
	if err != nil {
		t.Fatalf("AddedLinesSince: %v", err)
	}
	if got != len(names) {
		t.Errorf("AddedLinesSince = %d, want %d", got, len(names))
	}
	s.noUnscripted(t)
	grepCalls := 0
	for _, call := range s.sent() {
		if strings.HasPrefix(call, "--literal-pathspecs grep ") {
			grepCalls++
		}
	}
	if grepCalls != 2 {
		t.Errorf("%d git grep calls for %d paths, want 2", grepCalls, len(names))
	}
}

// TestRunawayBeadAddedLinesSinceSplitsBatchAtByteBound: names whose total
// length passes the byte bound are split over calls, not left on one argv.
func TestRunawayBeadAddedLinesSinceSplitsBatchAtByteBound(t *testing.T) {
	t.Parallel()
	long := 20 << 10
	a := strings.Repeat("a", long) + ".txt"
	b := strings.Repeat("b", long) + ".txt"
	if len(a)+len(b)+2 <= grepBatchBytes {
		t.Fatalf("test names total %d bytes, not over the %d-byte bound", len(a)+len(b)+2, grepBatchBytes)
	}
	s := newScripted(map[string]reply{
		"diff --numstat --no-renames --end-of-options main": ok(""),
		"ls-files --others --exclude-standard -z":           ok(a + "\x00" + b + "\x00"),
		grepCallKey(a): ok(grepRecords([]string{a})),
		grepCallKey(b): ok(grepRecords([]string{b})),
	})
	got, err := NewGitWithExec(t.TempDir(), s.run).AddedLinesSince("main")
	if err != nil {
		t.Fatalf("AddedLinesSince: %v", err)
	}
	if got != 2 {
		t.Errorf("AddedLinesSince = %d, want 2", got)
	}
	s.noUnscripted(t)
}

// TestRunawayBeadAddedLinesSinceEmptyUntrackedFileCountsZero: git grep exits 1
// when no line matched, which for untracked files means they are empty, so the
// count is 0 and not an error.
func TestRunawayBeadAddedLinesSinceEmptyUntrackedFileCountsZero(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"diff --numstat --no-renames --end-of-options main": ok("4\t1\tx.go\n"),
		"ls-files --others --exclude-standard -z":           ok("empty.txt\x00"),
		grepCallKey("empty.txt"):                            fail(1, ""),
	})
	got, err := NewGitWithExec(t.TempDir(), s.run).AddedLinesSince("main")
	if err != nil {
		t.Fatalf("AddedLinesSince: %v", err)
	}
	if got != 4 {
		t.Errorf("AddedLinesSince = %d, want 4", got)
	}
	s.noUnscripted(t)
}

// TestRunawayBeadAddedLinesSinceRejectsBadOutput: output the method cannot read
// is an error, not a count built from the lines it happened to parse.
func TestRunawayBeadAddedLinesSinceRejectsBadOutput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		answers map[string]reply
	}{
		{
			name: "numstat line lacks its fields",
			answers: map[string]reply{
				"diff --numstat --no-renames --end-of-options main": ok("3\t0\tx.go\nnot numstat\n"),
			},
		},
		{
			name: "numstat added field is not a number",
			answers: map[string]reply{
				"diff --numstat --no-renames --end-of-options main": ok("many\t0\tx.go\n"),
			},
		},
		{
			name: "numstat line lacks its path",
			answers: map[string]reply{
				"diff --numstat --no-renames --end-of-options main": ok("3\t0\tx.go\n4\t0\n"),
			},
		},
		{
			name: "grep line has no count",
			answers: map[string]reply{
				"diff --numstat --no-renames --end-of-options main": ok(""),
				"ls-files --others --exclude-standard -z":           ok("x.txt\x00"),
				grepCallKey("x.txt"):                                ok("x.txt\n"),
			},
		},
		{
			name: "grep count is not a number",
			answers: map[string]reply{
				"diff --numstat --no-renames --end-of-options main": ok(""),
				"ls-files --others --exclude-standard -z":           ok("x.txt\x00"),
				grepCallKey("x.txt"):                                ok("x.txt\x00lots\n"),
			},
		},
		{
			name: "grep line has an empty path",
			answers: map[string]reply{
				"diff --numstat --no-renames --end-of-options main": ok(""),
				"ls-files --others --exclude-standard -z":           ok("x.txt\x00"),
				grepCallKey("x.txt"):                                ok("\x005\n"),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newScripted(tc.answers)
			got, err := NewGitWithExec(t.TempDir(), s.run).AddedLinesSince("main")
			if err == nil {
				t.Fatalf("AddedLinesSince = %d, nil; want an error", got)
			}
			if got != 0 {
				t.Errorf("AddedLinesSince = %d with an error; want 0", got)
			}
			s.noUnscripted(t)
		})
	}
}
