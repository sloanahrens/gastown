package land

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// podPinDoc is the pod-engine doc's text for these tests: one row cites
// internal/daemon/dolt.go by line, and the rest of the doc is prose.
const podPinDoc = "The inventory pins its sites.\n" +
	"`internal/daemon/dolt.go:205` starts the external container.\n"

// podDocRepo is the landing's repository for a feed run: the rig's om threshold
// at base, and the pod-engine doc at base, which it returns as doc and counts
// the reads of. docErr, when set, is what reading the doc returns instead.
type podDocRepo struct {
	baseRepo
	doc      string
	docErr   error
	docReads int
}

func (p *podDocRepo) ShowFileAtRev(ref, path string) (string, error) {
	if path != podEngineDocFile {
		return p.baseRepo.ShowFileAtRev(ref, path)
	}
	p.docReads++
	if p.docErr != nil {
		return "", p.docErr
	}
	return p.doc, nil
}

// TestFollowupGroupPinned: a group is pinned when the pod-engine doc pins the
// file one of its findings' work belongs to. A test file's work belongs to the
// source it tests, so a group on that test file is pinned too; a group with
// one pinned finding among others is pinned; a group with no pinned file, and
// a group read against an empty doc, are not (gt-nlw44e).
func TestFollowupGroupPinned(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		doc      string
		findings []FollowupCandidate
		want     bool
	}{
		{"finding in a pinned file", podPinDoc, cands("internal/daemon/dolt.go"), true},
		{"finding in an unpinned file", podPinDoc, cands("internal/daemon/other.go"), false},
		{"test file of a pinned file", podPinDoc, cands("internal/daemon/dolt_test.go"), true},
		{"one pinned finding among unpinned ones", podPinDoc, cands("internal/daemon/other.go", "internal/daemon/dolt.go"), true},
		{"longer path that ends with a pinned path", podPinDoc, cands("x/internal/daemon/dolt.go"), false},
		{"finding with no path", podPinDoc, cands(""), false},
		{"pinned path against an empty doc", "", cands("internal/daemon/dolt.go"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			groups := []followupGroup{{Package: "internal/daemon", Findings: tc.findings}}
			markPinnedGroups(groups, tc.doc)
			if got := groups[0].Pinned; got != tc.want {
				t.Fatalf("Pinned = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFollowupGroupPinnedReadsDocOnce: a feed run reads the pod-engine doc exactly
// once, however many groups it files, and every group is checked against that
// one read (gt-nlw44e).
func TestFollowupGroupPinnedReadsDocOnce(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{
		minorAt("internal/daemon/dolt.go", 205, "swap the nil check"),
		minorAt("internal/land/note.go", 260, "name the error"),
	})
	repo := &podDocRepo{baseRepo: baseRepo{omJSON: `{"threshold": 0.6}`}, doc: podPinDoc}
	f.repo = repo
	f.run()
	if repo.docReads != 1 {
		t.Fatalf("the feed read the pod-engine doc %d times, want once", repo.docReads)
	}
	if len(f.fake.created) != 2 {
		t.Fatalf("filed %d beads for findings in two packages, want two", len(f.fake.created))
	}
}

// TestFollowupBodyPinned: a group whose file the pod-engine doc pins files a
// body whose Constraints name the doc as the third file its fix edits, with the
// -update-lines command that rewrites a drifted line; a group the doc does not
// pin files a body without that sentence (gt-w1dc6n).
func TestFollowupBodyPinned(t *testing.T) {
	t.Parallel()
	const pinned = "docs/design/pod-engine.md pins this file by line: edit it too, line numbers only, by running go test ./internal/podengine -update-lines; it is the third file this bead may touch, with the source file and its test file"
	cases := []struct {
		name    string
		finding Finding
		files   []string // the target branch's files the body is linted against
		want    bool
	}{
		{
			name:    "pinned file",
			finding: minorAt("internal/daemon/dolt.go", 205, "swap the nil check"),
			// The doc the Constraints now name is a file the branch holds, so the
			// body is linted as the dispatcher will read it (target.go).
			files: []string{"internal/daemon/dolt.go", "internal/daemon/dolt_test.go", podEngineDocFile},
			want:  true,
		},
		{
			name:    "unpinned file",
			finding: minorAt("internal/daemon/other.go", 9, "name the error"),
			files:   []string{"internal/daemon/other.go", "internal/daemon/other_test.go"},
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFollowupFixture(t, 0.8, []Finding{tc.finding})
			f.repo = &podDocRepo{baseRepo: baseRepo{omJSON: `{"threshold": 0.6}`}, doc: podPinDoc}
			f.run()
			if len(f.fake.created) != 1 {
				t.Fatalf("filed %d beads, want one", len(f.fake.created))
			}
			created := f.fake.created[0]
			constraints := specdispatch.Sections(created.Body)["constraints"]
			if got := strings.Contains(constraints, pinned); got != tc.want {
				t.Errorf("the Constraints carry the pod-engine sentence = %v, want %v:\n%s", got, tc.want, constraints)
			}
			spec := specdispatch.Spec{ID: "gt-filed-0", Title: created.Title, Type: "task", Status: "open",
				Description: created.Body, Target: followupLintTarget(tc.files...)}
			if v := specdispatch.Lint(spec, specdispatch.LoadTemplate("")); !v.Clean() {
				t.Errorf("the body does not lint clean against %v: %s\n%s", tc.files, v.Line(spec.ID), created.Body)
			}
		})
	}
}

// TestFollowupGroupPinnedLogsUnreadableDoc: a doc that cannot be read logs one line
// and the feed files as it would without the doc. A doc absent at the base is
// no pins and no line, because a repository without the doc has nothing to
// pin (gt-nlw44e).
func TestFollowupGroupPinnedLogsUnreadableDoc(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		docErr    error
		wantLines int
	}{
		{"unreadable doc logs one line", errors.New("object not found at base"), 1},
		{"doc absent at base logs nothing", git.ErrNotAtRef, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFollowupFixture(t, 0.8, []Finding{minorAt("internal/land/note.go", 260, "name the error")})
			f.repo = &podDocRepo{baseRepo: baseRepo{omJSON: `{"threshold": 0.6}`}, docErr: tc.docErr}
			log := f.run()
			if got := strings.Count(log, podEngineDocFile); got != tc.wantLines {
				t.Fatalf("log names the pod-engine doc on %d line(s), want %d:\n%s", got, tc.wantLines, log)
			}
			if len(f.fake.created) != 1 {
				t.Fatalf("filed %d beads with the doc unreadable, want the feed to file as usual", len(f.fake.created))
			}
		})
	}
}
