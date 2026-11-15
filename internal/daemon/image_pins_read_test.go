package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/forgejo"
)

// The pins and tips below are fixtures, not the committed pins: each commit is
// a distinct sha, so a read that reports another pin's answer cannot pass.
const (
	pinsReadBDCommit     = "1111111111111111111111111111111111111111"
	pinsReadOMCommit     = "2222222222222222222222222222222222222222"
	pinsReadHMCommit     = "3333333333333333333333333333333333333333"
	pinsReadDevopsCommit = "4444444444444444444444444444444444444444"

	pinsReadBDTip     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pinsReadOMTip     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	pinsReadHMTip     = "cccccccccccccccccccccccccccccccccccccccc"
	pinsReadDevopsTip = "dddddddddddddddddddddddddddddddddddddddd"
	pinsReadOtherTip  = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	// pinsReadMainRef is the ls-remote ref the fake prints, written as git
	// prints it rather than from the reader's constant, so a reader that asks
	// for another ref fails.
	pinsReadMainRef = "refs/heads/main"
	// pinsReadOwner is the Forgejo owner the fixture URLs name, and the owner a
	// placeholder fixture expands to.
	pinsReadOwner = "owner"
)

// pinsReadURL is the forge URL a fixture pin names for repo.
func pinsReadURL(repo string) string {
	return "http://forgejo:3000/" + pinsReadOwner + "/" + repo + ".git"
}

// pinsReadFile is the image-pins file the reader is handed: the four sources,
// each named by urlOf and pinned at its fixture commit.
func pinsReadFile(urlOf func(repo string) string) []byte {
	return []byte("# the four external sources\n" +
		"bd " + urlOf("beads") + " " + pinsReadBDCommit + "\n" +
		"om " + urlOf("organic-mechanic") + " " + pinsReadOMCommit + "\n" +
		"hm " + urlOf("history-man") + " " + pinsReadHMCommit + "\n" +
		"devops " + urlOf("devops") + " " + pinsReadDevopsCommit + "\n")
}

// pinsReadLS is the ls-remote output that answers tip as the source's main.
func pinsReadLS(tip string) string { return tip + "\t" + pinsReadMainRef + "\n" }

// fakePinTips is the injected git call: one raw ls-remote output per source URL,
// or the error that read fails with. A URL with no entry is an error, so a test
// that drops one cannot pass by reading a default.
type fakePinTips struct {
	answers map[string]string
	errs    map[string]error
	asked   []string
}

func (f *fakePinTips) read(url string) ([]byte, error) {
	f.asked = append(f.asked, url)
	if err, ok := f.errs[url]; ok {
		return nil, err
	}
	out, ok := f.answers[url]
	if !ok {
		return nil, fmt.Errorf("fake git: no ls-remote answer for %s", url)
	}
	return []byte(out), nil
}

// pinsReadCompareCall is one comparison the reader asked for, as the API
// received it.
type pinsReadCompareCall struct {
	owner, repo, base, head string
}

// fakePinCompare is the injected Forgejo read: the commit count a comparison
// answers, keyed "base...head", and the calls it received. A comparison with no
// entry is an error for the same reason a missing git answer is.
type fakePinCompare struct {
	behind map[string]int
	nilFor map[string]bool
	errs   map[string]error
	calls  []pinsReadCompareCall
}

func (f *fakePinCompare) CompareCommits(_ context.Context, owner, repo, base, head string) (*forgejo.CompareInfo, error) {
	f.calls = append(f.calls, pinsReadCompareCall{owner: owner, repo: repo, base: base, head: head})
	key := pinsReadCompareKey(base, head)
	if err, ok := f.errs[key]; ok {
		return nil, err
	}
	if f.nilFor[key] {
		return nil, nil
	}
	n, ok := f.behind[key]
	if !ok {
		return nil, fmt.Errorf("fake compare: no answer for %s", key)
	}
	return &forgejo.CompareInfo{TotalCommits: n}, nil
}

// pinsReadCompareKey keys a comparison's fixture answer, the pin as base and
// the tip as head.
func pinsReadCompareKey(base, head string) string { return base + "..." + head }

// pinsReadHealthies is a git and a compare that answer every fixture source:
// each pin trails its tip by one commit.
func pinsReadHealthies() (*fakePinTips, *fakePinCompare) {
	git := &fakePinTips{answers: map[string]string{}, errs: map[string]error{}}
	cmp := &fakePinCompare{behind: map[string]int{}, nilFor: map[string]bool{}, errs: map[string]error{}}
	for _, p := range []struct{ repo, pin, tip string }{
		{"beads", pinsReadBDCommit, pinsReadBDTip},
		{"organic-mechanic", pinsReadOMCommit, pinsReadOMTip},
		{"history-man", pinsReadHMCommit, pinsReadHMTip},
		{"devops", pinsReadDevopsCommit, pinsReadDevopsTip},
	} {
		git.answers[pinsReadURL(p.repo)] = pinsReadLS(p.tip)
		cmp.behind[pinsReadCompareKey(p.pin, p.tip)] = 1
	}
	return git, cmp
}

// TestImagePinsReadTipsAndBehind drives the read over the four pins of the
// fixture file: each row's source answers its own tip and commit count, so the
// read must report that pin's tip and count and address that pin's repository
// and commits.
func TestImagePinsReadTipsAndBehind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		pin    string
		tip    string
		repo   string
		behind int
	}{
		{"bd", pinsReadBDCommit, pinsReadBDTip, "beads", 12},
		{"om", pinsReadOMCommit, pinsReadOMTip, "organic-mechanic", 7},
		{"hm", pinsReadHMCommit, pinsReadHMTip, "history-man", 21},
		{"devops", pinsReadDevopsCommit, pinsReadDevopsTip, "devops", 34},
	}
	git := &fakePinTips{answers: map[string]string{}, errs: map[string]error{}}
	cmp := &fakePinCompare{behind: map[string]int{}}
	for _, tt := range tests {
		git.answers[pinsReadURL(tt.repo)] = pinsReadLS(tt.tip)
		cmp.behind[pinsReadCompareKey(tt.pin, tt.tip)] = tt.behind
	}

	reads, err := ReadImagePinTips(context.Background(), pinsReadFile(pinsReadURL), nil, git.read, cmp)
	require.NoError(t, err)

	require.Len(t, reads, len(tests))
	for i, tt := range tests {
		want := ImagePinRead{Name: tt.name, Pin: tt.pin, Tip: tt.tip, Behind: tt.behind}
		assert.Equal(t, want, reads[i], "%s pin read", tt.name)
		wantCall := pinsReadCompareCall{owner: pinsReadOwner, repo: tt.repo, base: tt.pin, head: tt.tip}
		assert.Equal(t, wantCall, cmp.calls[i], "%s pin comparison", tt.name)
	}
	assert.Equal(t, []string{
		pinsReadURL("beads"), pinsReadURL("organic-mechanic"),
		pinsReadURL("history-man"), pinsReadURL("devops"),
	}, git.asked, "each source's tip is read once, in the file's order")
}

// TestImagePinsReadAtTipSkipsCompare: a pin whose commit is its source's tip is
// read as zero behind, and the compare API is asked about the other three only.
func TestImagePinsReadAtTipSkipsCompare(t *testing.T) {
	t.Parallel()
	git, cmp := pinsReadHealthies()
	git.answers[pinsReadURL("beads")] = pinsReadLS(pinsReadBDCommit)

	reads, err := ReadImagePinTips(context.Background(), pinsReadFile(pinsReadURL), nil, git.read, cmp)
	require.NoError(t, err)

	require.Len(t, reads, 4)
	assert.Equal(t, ImagePinRead{Name: "bd", Pin: pinsReadBDCommit, Tip: pinsReadBDCommit, Behind: 0}, reads[0],
		"a pin at its tip is zero behind")
	require.Len(t, cmp.calls, 3, "the pin at its tip is not compared")
	for _, call := range cmp.calls {
		assert.NotEqual(t, pinsReadBDCommit, call.base, "the bd pin is at its tip")
	}
}

// TestImagePinsReadExpandsTheForgeOwner: a file written with {owner} is expanded
// with the owner the resolver names, and the comparison addresses the expanded
// owner's repository.
func TestImagePinsReadExpandsTheForgeOwner(t *testing.T) {
	t.Parallel()
	git, cmp := pinsReadHealthies()
	placeholderURL := func(repo string) string {
		return "http://forgejo:3000/" + config.ForgeOwnerPlaceholder + "/" + repo + ".git"
	}
	ownerOf := func() (string, error) { return pinsReadOwner, nil }

	reads, err := ReadImagePinTips(context.Background(), pinsReadFile(placeholderURL), ownerOf, git.read, cmp)
	require.NoError(t, err)

	require.Len(t, reads, 4)
	require.Len(t, cmp.calls, 4)
	for i, call := range cmp.calls {
		assert.Equal(t, pinsReadOwner, call.owner, "comparison %d names the expanded owner", i)
	}
	assert.Equal(t, pinsReadBDTip, reads[0].Tip, "the expanded URL's source is still the one read")
}

// TestImagePinsReadLeavesTheOwnerResolverUncalled: a file with no {owner}
// placeholder is read without asking the resolver, so a reader with no town to
// resolve a forge.owner from still reads it. The resolver answers a usable owner
// and counts its calls, so a read that asks it early fails on the count alone.
func TestImagePinsReadLeavesTheOwnerResolverUncalled(t *testing.T) {
	t.Parallel()
	git, cmp := pinsReadHealthies()
	calls := 0
	ownerOf := func() (string, error) {
		calls++
		return pinsReadOwner, nil
	}

	reads, err := ReadImagePinTips(context.Background(), pinsReadFile(pinsReadURL), ownerOf, git.read, cmp)
	require.NoError(t, err)

	require.Len(t, reads, 4)
	assert.Equal(t, 0, calls, "the file holds no %s placeholder, so the resolver is not asked", config.ForgeOwnerPlaceholder)
}

// TestImagePinsReadRefusals drives every branch that refuses: the read reports
// no pins and an error naming what failed, never a pin with a guessed answer.
func TestImagePinsReadRefusals(t *testing.T) {
	t.Parallel()
	placeholderFile := pinsReadFile(func(repo string) string {
		return "http://forgejo:3000/" + config.ForgeOwnerPlaceholder + "/" + repo + ".git"
	})
	// badURL is the fixture file with beads' URL replaced by raw.
	badURL := func(raw string) []byte {
		return pinsReadFile(func(repo string) string {
			if repo == "beads" {
				return raw
			}
			return pinsReadURL(repo)
		})
	}
	bdKey := pinsReadCompareKey(pinsReadBDCommit, pinsReadBDTip)

	tests := []struct {
		name    string
		file    []byte
		ownerOf func() (string, error)
		setup   func(*fakePinTips, *fakePinCompare)
		noGit   bool
		noCmp   bool
		want    string
	}{
		{name: "no git seam", noGit: true, want: "no git to read"},
		{name: "no compare seam", noCmp: true, want: "no forgejo compare"},
		{name: "an unparseable file", file: []byte("bd not-a-pin-line\n"), want: "image pins file: line 1"},
		{name: "a placeholder with no resolver", file: placeholderFile, want: "nothing was given to expand it"},
		{
			name: "a resolver that fails", file: placeholderFile,
			ownerOf: func() (string, error) { return "", errors.New("town has no forge.owner") },
			want:    "town has no forge.owner",
		},
		{
			name: "a resolver that names no usable owner", file: placeholderFile,
			ownerOf: func() (string, error) { return "not an owner!", nil },
			want:    `forge.owner: "not an owner!" is not a Forgejo owner name`,
		},
		{name: "a URL without an owner and repository", file: badURL("http://forgejo:3000/beads.git"), want: "exactly one owner and one repository"},
		{name: "a URL whose owner traverses", file: badURL("http://forgejo:3000/../beads.git"), want: "no usable owner"},
		{name: "a URL whose repository is empty", file: badURL("http://forgejo:3000/owner/.git"), want: "no usable repository"},
		{
			name: "a tip read that fails",
			setup: func(git *fakePinTips, _ *fakePinCompare) {
				git.errs[pinsReadURL("beads")] = errors.New("remote hung up")
			},
			want: "cannot read refs/heads/main",
		},
		{
			name:  "a tip answer with no main ref",
			setup: func(git *fakePinTips, _ *fakePinCompare) { git.answers[pinsReadURL("beads")] = "" },
			want:  "named no commit",
		},
		{
			name: "a tip answer that is not a sha",
			setup: func(git *fakePinTips, _ *fakePinCompare) {
				git.answers[pinsReadURL("beads")] = pinsReadLS("not-a-sha")
			},
			want: "not a full 40-character",
		},
		{
			name: "a tip answer naming another ref",
			setup: func(git *fakePinTips, _ *fakePinCompare) {
				git.answers[pinsReadURL("beads")] = pinsReadBDTip + "\trefs/heads/other\n"
			},
			want: "a line that is not",
		},
		{
			name: "a tip answer naming two commits",
			setup: func(git *fakePinTips, _ *fakePinCompare) {
				git.answers[pinsReadURL("beads")] = pinsReadLS(pinsReadBDTip) + pinsReadLS(pinsReadOtherTip)
			},
			want: "was answered 2 times",
		},
		{
			name: "a comparison that fails",
			setup: func(_ *fakePinTips, cmp *fakePinCompare) {
				cmp.errs[bdKey] = errors.New("forgejo: server error")
			},
			want: "cannot compare it with refs/heads/main",
		},
		{
			name:  "a comparison answer with no comparison",
			setup: func(_ *fakePinTips, cmp *fakePinCompare) { cmp.nilFor[bdKey] = true },
			want:  "answered no comparison",
		},
		{
			name:  "a comparison answer with a negative count",
			setup: func(_ *fakePinTips, cmp *fakePinCompare) { cmp.behind[bdKey] = -1 },
			want:  "which is not a count",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			git, cmp := pinsReadHealthies()
			file := tt.file
			if file == nil {
				file = pinsReadFile(pinsReadURL)
			}
			if tt.setup != nil {
				tt.setup(git, cmp)
			}
			var tipGit PinTipGit = git.read
			if tt.noGit {
				tipGit = nil
			}
			var compare ImagePinCompare = cmp
			if tt.noCmp {
				compare = nil
			}

			reads, err := ReadImagePinTips(context.Background(), file, tt.ownerOf, tipGit, compare)
			require.ErrorContains(t, err, tt.want)
			assert.Nil(t, reads, "a refused read reports no pins")
		})
	}
}
