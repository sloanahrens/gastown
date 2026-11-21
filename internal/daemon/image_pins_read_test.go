package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemonimage"
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

// pinsReadPinURL is the forge URL a fixture pin names for repo.
func pinsReadPinURL(repo string) string {
	return "http://forgejo:3000/" + pinsReadOwner + "/" + repo + ".git"
}

// pinsReadPinLines is the fixture file's four pin lines, in the order the file
// names them, so a structural fixture changes only the line it tests.
func pinsReadPinLines() []string {
	return []string{
		"bd " + pinsReadPinURL("beads") + " " + pinsReadBDCommit,
		"om " + pinsReadPinURL("organic-mechanic") + " " + pinsReadOMCommit,
		"hm " + pinsReadPinURL("history-man") + " " + pinsReadHMCommit,
		"devops " + pinsReadPinURL("devops") + " " + pinsReadDevopsCommit,
	}
}

// pinsReadFixture is the image-pins file the reader is handed: a comment line,
// then the four sources, each named by urlOf and pinned at its fixture commit.
// urlOf exists so the placeholder fixture writes {owner} where the file does.
func pinsReadFixture(urlOf func(repo string) string) []byte {
	return []byte("# the four external sources\n" +
		"bd " + urlOf("beads") + " " + pinsReadBDCommit + "\n" +
		"om " + urlOf("organic-mechanic") + " " + pinsReadOMCommit + "\n" +
		"hm " + urlOf("history-man") + " " + pinsReadHMCommit + "\n" +
		"devops " + urlOf("devops") + " " + pinsReadDevopsCommit + "\n")
}

// pinsReadFixtureWithBDURL is the fixture file with the bd line's URL replaced
// by raw, for the rows that drive one pin's URL refusal.
func pinsReadFixtureWithBDURL(raw string) []byte {
	return pinsReadFixture(func(repo string) string {
		if repo == "beads" {
			return raw
		}
		return pinsReadPinURL(repo)
	})
}

// pinsReadFileWithLines is the fixture's four pin lines changed by mutate, so a
// structural fixture writes only the line it tests.
func pinsReadFileWithLines(mutate func([]string) []string) []byte {
	return []byte(strings.Join(mutate(append([]string(nil), pinsReadPinLines()...)), "\n") + "\n")
}

// pinsReadOtherStates is what the reader must report for the fixture's om, hm
// and devops pins when bd is the only pin that fails: each read its own tip, one
// commit behind.
func pinsReadOtherStates() []ImagePinState {
	return []ImagePinState{
		{Name: "om", Pin: pinsReadOMCommit, Tip: pinsReadOMTip, Behind: 1},
		{Name: "hm", Pin: pinsReadHMCommit, Tip: pinsReadHMTip, Behind: 1},
		{Name: "devops", Pin: pinsReadDevopsCommit, Tip: pinsReadDevopsTip, Behind: 1},
	}
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
// entry is an error for the same reason a missing git answer is. A head at
// BDSamplesFloor answers floor instead: every bd read asks it.
type fakePinCompare struct {
	behind map[string]int
	nilFor map[string]bool
	errs   map[string]error
	floor  int
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
		if head == daemonimage.BDSamplesFloor {
			return &forgejo.CompareInfo{TotalCommits: f.floor}, nil
		}
		return nil, fmt.Errorf("fake compare: no answer for %s", key)
	}
	return &forgejo.CompareInfo{TotalCommits: n}, nil
}

// pinsReadCompareKey keys a comparison's fixture answer, the pin as base and
// the tip as head.
func pinsReadCompareKey(base, head string) string { return base + "..." + head }

// pinsReadPathCountCall is one path-limited count the reader asked for, as the
// seam received it.
type pinsReadPathCountCall struct {
	url, pin, tip string
	paths         []string
}

// fakePinPathCount is the injected path-limited count: the answer every call
// gets, and the calls it received. Only the devops pin carries an input list, so
// a call for another pin is visible in calls.
type fakePinPathCount struct {
	behind int
	err    error
	calls  []pinsReadPathCountCall
}

func (f *fakePinPathCount) count(url, pin, tip string, paths []string) (int, error) {
	f.calls = append(f.calls, pinsReadPathCountCall{url: url, pin: pin, tip: tip, paths: paths})
	return f.behind, f.err
}

// pinsReadPinsBeforeDevops is what the reader must report for the fixture's bd,
// om and hm pins, each one commit behind, beside a devops state under test.
func pinsReadPinsBeforeDevops() []ImagePinState {
	return []ImagePinState{
		{Name: "bd", Pin: pinsReadBDCommit, Tip: pinsReadBDTip, Behind: 1},
		{Name: "om", Pin: pinsReadOMCommit, Tip: pinsReadOMTip, Behind: 1},
		{Name: "hm", Pin: pinsReadHMCommit, Tip: pinsReadHMTip, Behind: 1},
	}
}

// pinsReadAllHealthy is a git, a compare and a path count that answer every
// fixture source: each pin trails its tip by one commit, the devops pin counted
// through its inputs and the other three through the compare. The compare
// answers nothing for devops, so a reader that compared it would fail.
func pinsReadAllHealthy() (*fakePinTips, *fakePinCompare, *fakePinPathCount) {
	git := &fakePinTips{answers: map[string]string{}, errs: map[string]error{}}
	cmp := &fakePinCompare{behind: map[string]int{}, nilFor: map[string]bool{}, errs: map[string]error{}}
	for _, p := range []struct{ repo, pin, tip string }{
		{"beads", pinsReadBDCommit, pinsReadBDTip},
		{"organic-mechanic", pinsReadOMCommit, pinsReadOMTip},
		{"history-man", pinsReadHMCommit, pinsReadHMTip},
	} {
		git.answers[pinsReadPinURL(p.repo)] = pinsReadLS(p.tip)
		cmp.behind[pinsReadCompareKey(p.pin, p.tip)] = 1
	}
	git.answers[pinsReadPinURL("devops")] = pinsReadLS(pinsReadDevopsTip)
	return git, cmp, &fakePinPathCount{behind: 1}
}

// TestImagePinsReadTipsAndBehind drives the read over the four pins of the
// fixture file: each row's source answers its own tip and commit count, so the
// read must report that pin's tip and count, addressing a compare pin's
// repository and commits through the compare and counting devops through its
// inputs.
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
	pathCount := &fakePinPathCount{}
	for _, tt := range tests {
		git.answers[pinsReadPinURL(tt.repo)] = pinsReadLS(tt.tip)
		if tt.name == "devops" {
			pathCount.behind = tt.behind
			continue
		}
		cmp.behind[pinsReadCompareKey(tt.pin, tt.tip)] = tt.behind
	}

	states, err := ReadImagePinTips(context.Background(), pinsReadFixture(pinsReadPinURL), nil, git.read, pathCount.count, cmp)
	require.NoError(t, err)

	require.Len(t, cmp.calls, 4, "the bd floor, then the three compared pins")
	require.Len(t, states, len(tests))
	for i, tt := range tests {
		want := ImagePinState{Name: tt.name, Pin: tt.pin, Tip: tt.tip, Behind: tt.behind}
		assert.Equal(t, want, states[i], "%s pin read", tt.name)
		if tt.name == "devops" {
			continue
		}
		wantCall := pinsReadCompareCall{owner: pinsReadOwner, repo: tt.repo, base: tt.pin, head: tt.tip}
		assert.Equal(t, wantCall, cmp.calls[i+1], "%s pin comparison", tt.name)
	}
	assert.Equal(t, []string{
		pinsReadPinURL("beads"), pinsReadPinURL("organic-mechanic"),
		pinsReadPinURL("history-man"), pinsReadPinURL("devops"),
	}, git.asked, "each source's tip is read once, in the file's order")
}

// TestImagePinsReadAtTipSkipsCompare: a pin whose commit is its source's tip is
// read as zero behind, and the compare API counts no lag for it and none for the
// devops pin, counted through its inputs: only om and hm are lag-compared, and
// the bd floor is the one other comparison, made whether or not bd is at its tip.
func TestImagePinsReadAtTipSkipsCompare(t *testing.T) {
	t.Parallel()
	git, cmp, pathCount := pinsReadAllHealthy()
	git.answers[pinsReadPinURL("beads")] = pinsReadLS(pinsReadBDCommit)

	states, err := ReadImagePinTips(context.Background(), pinsReadFixture(pinsReadPinURL), nil, git.read, pathCount.count, cmp)
	require.NoError(t, err)

	require.Len(t, states, 4)
	assert.Equal(t, ImagePinState{Name: "bd", Pin: pinsReadBDCommit, Tip: pinsReadBDCommit, Behind: 0}, states[0],
		"a pin at its tip is zero behind")
	require.Len(t, cmp.calls, 3, "the floor, then only om's and hm's lag: the bd floor call is the one that names bd")
	for _, call := range cmp.calls {
		assert.False(t, call.base == pinsReadBDCommit && call.head != daemonimage.BDSamplesFloor,
			"the bd pin at its tip is not lag-compared")
	}
}

// TestReadImagePinTipsBDFloor: the bd pin's tip is held to the samples build the
// image needs, whether or not the pin is at it, so a tip that does not contain
// the floor is that pin's Err naming the floor and no lag is counted; a tip that
// contains it reads clean. The guard is bd's alone: another pin's tip is never
// the base of a floor call, only of that pin's own lag compare.
func TestReadImagePinTipsBDFloor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		only           int
		pin            string // the pin this row is about; empty means bd
		pinCommit, tip string
		want           string
	}{
		{name: "a bd tip that contains the samples build"},
		{name: "a bd tip that lacks the samples build", only: 3, want: "does not contain the samples build"},
		{name: "another pin's tip never makes the floor call", pin: "om", pinCommit: pinsReadOMCommit, tip: pinsReadOMTip},
		{name: "the hm pin's tip never makes the floor call", pin: "hm", pinCommit: pinsReadHMCommit, tip: pinsReadHMTip},
		{name: "the devops pin's tip never makes the floor call", pin: "devops", pinCommit: pinsReadDevopsCommit, tip: pinsReadDevopsTip},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			git, cmp, pathCount := pinsReadAllHealthy()
			cmp.floor = tt.only

			states, err := ReadImagePinTips(context.Background(), pinsReadFixture(pinsReadPinURL), nil, git.read, pathCount.count, cmp)
			require.NoError(t, err)
			require.Len(t, states, 4)
			if tt.pin != "" {
				assert.Equal(t, pinsReadOtherStates(), states[1:], "the %s pin reads its lag clean beside bd", tt.pin)
				floor, lag := 0, 0
				for _, call := range cmp.calls {
					if call.base == tt.tip && call.head == daemonimage.BDSamplesFloor {
						floor++
					}
					if call.base == tt.pinCommit && call.head == tt.tip {
						lag++
					}
				}
				assert.Zero(t, floor, "the %s pin's tip is never the base of a floor call", tt.pin)
				assert.LessOrEqual(t, lag, 1, "the %s pin's own lag compare is at most one call", tt.pin)
				assert.Contains(t, cmp.calls, pinsReadCompareCall{owner: pinsReadOwner, repo: "beads", base: pinsReadBDTip, head: daemonimage.BDSamplesFloor},
					"bd still takes the floor call in this row")
				return
			}
			if tt.want == "" {
				assert.NoError(t, states[0].Err, "a bd tip that contains the floor reads clean")
				assert.Equal(t, ImagePinState{Name: "bd", Pin: pinsReadBDCommit, Tip: pinsReadBDTip, Behind: 1}, states[0])
				return
			}
			require.Error(t, states[0].Err, "a bd tip that lacks the floor is that pin's error")
			for _, part := range []string{"bd-floor guard", daemonimage.BDSamplesFloor, pinsReadBDTip, tt.want} {
				assert.Contains(t, states[0].Err.Error(), part, "the error names what failed")
			}
			assert.Equal(t, ImagePinState{Name: "bd", Pin: pinsReadBDCommit, Tip: pinsReadBDTip, Err: states[0].Err}, states[0],
				"the tip read before the floor is kept and no lag is reported")
			assert.Equal(t, pinsReadOtherStates(), states[1:], "the other three pins are still read")
		})
	}
}

// TestImagePinsReadExpandsTheForgeOwner: a file written with {owner} is expanded
// with the owner the resolver names, and the comparison addresses the expanded
// owner's repository.
func TestImagePinsReadExpandsTheForgeOwner(t *testing.T) {
	t.Parallel()
	git, cmp, pathCount := pinsReadAllHealthy()
	placeholderURL := func(repo string) string {
		return "http://forgejo:3000/" + config.ForgeOwnerPlaceholder + "/" + repo + ".git"
	}
	ownerOf := func() (string, error) { return pinsReadOwner, nil }

	states, err := ReadImagePinTips(context.Background(), pinsReadFixture(placeholderURL), ownerOf, git.read, pathCount.count, cmp)
	require.NoError(t, err)

	require.Len(t, states, 4)
	require.Len(t, cmp.calls, 4, "the bd floor and the three compared pins are asked")
	for i, call := range cmp.calls {
		assert.Equal(t, pinsReadOwner, call.owner, "comparison %d names the expanded owner", i)
	}
	assert.Equal(t, pinsReadBDTip, states[0].Tip, "the expanded URL's source is still the one read")
}

// TestImagePinsReadLeavesTheOwnerResolverUncalled: a file with no {owner}
// placeholder is read without asking the resolver, so a reader with no town to
// resolve a forge.owner from still reads it. The resolver answers a usable owner
// and counts its calls, so a read that asks it early fails on the count alone.
func TestImagePinsReadLeavesTheOwnerResolverUncalled(t *testing.T) {
	t.Parallel()
	git, cmp, pathCount := pinsReadAllHealthy()
	calls := 0
	ownerOf := func() (string, error) {
		calls++
		return pinsReadOwner, nil
	}

	states, err := ReadImagePinTips(context.Background(), pinsReadFixture(pinsReadPinURL), ownerOf, git.read, pathCount.count, cmp)
	require.NoError(t, err)

	require.Len(t, states, 4)
	assert.Equal(t, 0, calls, "the file holds no %s placeholder, so the resolver is not asked", config.ForgeOwnerPlaceholder)
}

// TestImagePinsReadAFailedTipIsThatPinsError is acceptance box 1 for a tip
// read: one source's git answer fails, that pin's Err names it, the other three
// pins are read rather than skipped, and no pin is reported with a guessed tip.
func TestImagePinsReadAFailedTipIsThatPinsError(t *testing.T) {
	t.Parallel()
	git, cmp, pathCount := pinsReadAllHealthy()
	git.errs[pinsReadPinURL("beads")] = errors.New("remote hung up")

	states, err := ReadImagePinTips(context.Background(), pinsReadFixture(pinsReadPinURL), nil, git.read, pathCount.count, cmp)
	require.NoError(t, err, "a pin's failed read is not the file's error")
	require.Len(t, states, 4)

	require.Error(t, states[0].Err, "the failed bd read is that pin's error")
	assert.Contains(t, states[0].Err.Error(), "bd pin: cannot read refs/heads/main")
	assert.Equal(t, "bd", states[0].Name, "the failed state still names its pin")
	assert.Equal(t, pinsReadBDCommit, states[0].Pin, "the failed state still names the file's commit")
	assert.Empty(t, states[0].Tip, "a failed read never reports a guessed tip")
	assert.Equal(t, pinsReadOtherStates(), states[1:], "the other three pins are still read")
	assert.Equal(t, []string{
		pinsReadPinURL("beads"), pinsReadPinURL("organic-mechanic"),
		pinsReadPinURL("history-man"), pinsReadPinURL("devops"),
	}, git.asked, "every source's tip is read, including the ones after the failed pin")
}

// TestImagePinsReadAFailedCompareIsThatPinsError is acceptance box 1 for a
// compare: the source's tip reads, the comparison fails, that pin's Err names it,
// and the other three pins are read rather than skipped.
func TestImagePinsReadAFailedCompareIsThatPinsError(t *testing.T) {
	t.Parallel()
	git, cmp, pathCount := pinsReadAllHealthy()
	cmp.errs[pinsReadCompareKey(pinsReadBDCommit, pinsReadBDTip)] = errors.New("forgejo: server error")

	states, err := ReadImagePinTips(context.Background(), pinsReadFixture(pinsReadPinURL), nil, git.read, pathCount.count, cmp)
	require.NoError(t, err, "a pin's failed compare is not the file's error")
	require.Len(t, states, 4)

	require.Error(t, states[0].Err, "the failed bd compare is that pin's error")
	assert.Contains(t, states[0].Err.Error(), "bd pin "+pinsReadBDCommit[:12]+": cannot compare it with refs/heads/main")
	assert.Equal(t, pinsReadBDTip, states[0].Tip, "the tip read before the compare is kept")
	assert.Equal(t, pinsReadOtherStates(), states[1:], "the other three pins are still read")
}

// TestImagePinsReadAnUnparseableURLIsThatPinsError is acceptance box 2: a pin URL
// that does not parse is that pin's Err, the other three pins are read, and the
// URL is never handed to git.
func TestImagePinsReadAnUnparseableURLIsThatPinsError(t *testing.T) {
	t.Parallel()
	git, cmp, pathCount := pinsReadAllHealthy()
	badURL := "http://forgejo:3000/%zz/beads.git"

	states, err := ReadImagePinTips(context.Background(), pinsReadFixtureWithBDURL(badURL), nil, git.read, pathCount.count, cmp)
	require.NoError(t, err, "a pin's URL failure is not the file's error")
	require.Len(t, states, 4)

	require.Error(t, states[0].Err, "the unparseable bd URL is that pin's error")
	assert.Contains(t, states[0].Err.Error(), "bd pin: its URL does not parse")
	assert.Equal(t, "bd", states[0].Name)
	assert.Equal(t, pinsReadBDCommit, states[0].Pin)
	assert.Empty(t, states[0].Tip, "a pin whose URL does not parse is never read")
	assert.Equal(t, pinsReadOtherStates(), states[1:], "the other three pins are still read")
	assert.Equal(t, []string{
		pinsReadPinURL("organic-mechanic"), pinsReadPinURL("history-man"), pinsReadPinURL("devops"),
	}, git.asked, "the unparseable URL is never handed to git")
}

// TestImagePinsReadOnePinRefused drives every branch that fails one pin: the
// failure is that pin's Err, it names the pin, and the other three pins still
// read.
func TestImagePinsReadOnePinRefused(t *testing.T) {
	t.Parallel()
	bdKey := pinsReadCompareKey(pinsReadBDCommit, pinsReadBDTip)
	bdURL := pinsReadPinURL("beads")

	tests := []struct {
		name  string
		file  []byte
		setup func(*fakePinTips, *fakePinCompare)
		want  string
	}{
		{name: "a URL that does not parse", file: pinsReadFixtureWithBDURL("http://forgejo:3000/%zz/beads.git"), want: "its URL does not parse"},
		{name: "a URL that is not http(s)", file: pinsReadFixtureWithBDURL("ssh://forgejo:3000/owner/beads.git"), want: "not http:// or https://"},
		{name: "a URL that names no host", file: pinsReadFixtureWithBDURL("http:///owner/beads.git"), want: "names no host"},
		{name: "a URL that carries a credential", file: pinsReadFixtureWithBDURL("http://user:pass@forgejo:3000/owner/beads.git"), want: "carries a credential"},
		{name: "a URL that carries a query", file: pinsReadFixtureWithBDURL("http://forgejo:3000/owner/beads.git?token=x"), want: "carries a query or fragment"},
		{name: "a URL without an owner and repository", file: pinsReadFixtureWithBDURL("http://forgejo:3000/beads.git"), want: "exactly one owner and one repository"},
		{name: "a URL whose owner traverses", file: pinsReadFixtureWithBDURL("http://forgejo:3000/../beads.git"), want: "no usable owner"},
		{name: "a URL whose repository is empty", file: pinsReadFixtureWithBDURL("http://forgejo:3000/owner/.git"), want: "no usable repository"},
		{
			name:  "a tip read that fails",
			setup: func(git *fakePinTips, _ *fakePinCompare) { git.errs[bdURL] = errors.New("remote hung up") },
			want:  "bd pin: cannot read refs/heads/main",
		},
		{
			name:  "a tip answer with no main ref",
			setup: func(git *fakePinTips, _ *fakePinCompare) { git.answers[bdURL] = "" },
			want:  "named no commit",
		},
		{
			name: "a tip answer that is not a sha",
			setup: func(git *fakePinTips, _ *fakePinCompare) {
				git.answers[bdURL] = pinsReadLS("not-a-sha")
			},
			want: "not a full 40-character",
		},
		{
			name: "a tip answer naming another ref",
			setup: func(git *fakePinTips, _ *fakePinCompare) {
				git.answers[bdURL] = pinsReadBDTip + "\trefs/heads/other\n"
			},
			want: "a line that is not",
		},
		{
			name: "a tip answer naming two commits",
			setup: func(git *fakePinTips, _ *fakePinCompare) {
				git.answers[bdURL] = pinsReadLS(pinsReadBDTip) + pinsReadLS(pinsReadOtherTip)
			},
			want: "was answered 2 times",
		},
		{
			name: "a comparison that fails",
			setup: func(_ *fakePinTips, cmp *fakePinCompare) {
				cmp.errs[bdKey] = errors.New("forgejo: server error")
			},
			want: "bd pin 111111111111: cannot compare it with refs/heads/main",
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
			git, cmp, pathCount := pinsReadAllHealthy()
			file := tt.file
			if file == nil {
				file = pinsReadFixture(pinsReadPinURL)
			}
			if tt.setup != nil {
				tt.setup(git, cmp)
			}

			states, err := ReadImagePinTips(context.Background(), file, nil, git.read, pathCount.count, cmp)
			require.NoError(t, err, "one pin's failure is not the file's error")
			require.Len(t, states, 4)
			require.Error(t, states[0].Err, "the bd pin's failure is its error")
			assert.Contains(t, states[0].Err.Error(), tt.want)
			assert.Equal(t, "bd", states[0].Name, "the failed state names its pin")
			assert.Equal(t, pinsReadBDCommit, states[0].Pin, "the failed state names the file's commit")
			assert.Equal(t, pinsReadOtherStates(), states[1:], "the other three pins are still read")
		})
	}
}

// TestImagePinsReadFileRefusals drives every branch that refuses the file: the
// read reports no pins and an error naming what failed, never a partial list.
func TestImagePinsReadFileRefusals(t *testing.T) {
	t.Parallel()
	placeholderURL := func(repo string) string {
		return "http://forgejo:3000/" + config.ForgeOwnerPlaceholder + "/" + repo + ".git"
	}
	placeholderFile := pinsReadFixture(placeholderURL)

	tests := []struct {
		name    string
		file    []byte
		ownerOf func() (string, error)
		noGit   bool
		noCmp   bool
		want    string
	}{
		{name: "no git seam", noGit: true, want: "no git to read"},
		{name: "no compare seam", noCmp: true, want: "no forgejo compare"},
		{
			name: "a line that is not three fields",
			file: pinsReadFileWithLines(func(lines []string) []string {
				lines[0] = "bd not-a-pin-line"
				return lines
			}),
			want: "image pins file: line 1",
		},
		{
			name: "an unknown pin name",
			file: pinsReadFileWithLines(func(lines []string) []string {
				lines[0] = "zz " + pinsReadPinURL("beads") + " " + pinsReadBDCommit
				return lines
			}),
			want: `unknown pin name "zz"`,
		},
		{
			name: "a duplicate pin name",
			file: pinsReadFileWithLines(func(lines []string) []string {
				lines[1] = lines[0]
				return lines
			}),
			want: `duplicate pin name "bd"`,
		},
		{
			name: "a commit that is not a full sha",
			file: pinsReadFileWithLines(func(lines []string) []string {
				lines[0] = "bd " + pinsReadPinURL("beads") + " not-a-sha"
				return lines
			}),
			want: "not a full 40-character lowercase hex sha",
		},
		{
			name: "a missing pin",
			file: pinsReadFileWithLines(func(lines []string) []string { return lines[1:] }),
			want: "missing pin for bd",
		},
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			git, cmp, _ := pinsReadAllHealthy()
			file := tt.file
			if file == nil {
				file = pinsReadFixture(pinsReadPinURL)
			}
			var tipGit PinTipGit = git.read
			if tt.noGit {
				tipGit = nil
			}
			var compare ImagePinCompare = cmp
			if tt.noCmp {
				compare = nil
			}

			states, err := ReadImagePinTips(context.Background(), file, tt.ownerOf, tipGit, nil, compare)
			require.ErrorContains(t, err, tt.want)
			assert.Nil(t, states, "a refused read reports no pins")
		})
	}
}

// TestImagePinsReadFileShapeAgreesWithTheGuardParser keeps the reader's restated
// shape checks equal to the pin-drift guard's parser: on a file whose URLs all
// parse, the reader refuses exactly what daemonimage.ParsePins refuses, so the
// two cannot drift apart on a file either should refuse.
func TestImagePinsReadFileShapeAgreesWithTheGuardParser(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		file   []byte
		refuse bool
	}{
		{"the fixture file", pinsReadFixture(pinsReadPinURL), false},
		{
			name: "a line that is not three fields",
			file: pinsReadFileWithLines(func(lines []string) []string {
				lines[0] = "bd not-a-pin-line"
				return lines
			}),
			refuse: true,
		},
		{
			name: "an unknown pin name",
			file: pinsReadFileWithLines(func(lines []string) []string {
				lines[0] = "zz " + pinsReadPinURL("beads") + " " + pinsReadBDCommit
				return lines
			}),
			refuse: true,
		},
		{
			name: "a duplicate pin name",
			file: pinsReadFileWithLines(func(lines []string) []string {
				lines[1] = lines[0]
				return lines
			}),
			refuse: true,
		},
		{
			name: "a commit that is not a full sha",
			file: pinsReadFileWithLines(func(lines []string) []string {
				lines[0] = "bd " + pinsReadPinURL("beads") + " not-a-sha"
				return lines
			}),
			refuse: true,
		},
		{
			name:   "a missing pin",
			file:   pinsReadFileWithLines(func(lines []string) []string { return lines[1:] }),
			refuse: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, guardErr := daemonimage.ParsePins(tt.file)
			git, cmp, pathCount := pinsReadAllHealthy()
			_, readErr := ReadImagePinTips(context.Background(), tt.file, nil, git.read, pathCount.count, cmp)

			if !tt.refuse {
				require.NoError(t, guardErr, "the guard parser accepts the fixture file")
				require.NoError(t, readErr, "the reader accepts the fixture file")
				return
			}
			require.Error(t, guardErr, "the guard parser refuses the file")
			require.Error(t, readErr, "the reader refuses the file the guard parser refuses")
		})
	}
}

// TestReadImagePinTipsCountsOnlyInputCommitsForADevopsPin: the devops pin's lag
// is the path-limited count, asked for its URL, its pin, its tip and the inputs
// the image builds it from, and the compare API, which cannot name the paths a
// commit changed, is not asked about it; the three pins with no input list keep
// the compare count.
func TestReadImagePinTipsCountsOnlyInputCommitsForADevopsPin(t *testing.T) {
	t.Parallel()
	git, cmp, _ := pinsReadAllHealthy()
	pathCount := &fakePinPathCount{behind: 3}

	states, err := ReadImagePinTips(context.Background(), pinsReadFixture(pinsReadPinURL), nil, git.read, pathCount.count, cmp)
	require.NoError(t, err)

	require.Len(t, states, 4)
	assert.Equal(t, ImagePinState{Name: "devops", Pin: pinsReadDevopsCommit, Tip: pinsReadDevopsTip, Behind: 3}, states[3],
		"a devops pin's Behind is the path-limited count, not the compare answer")
	assert.Equal(t, []pinsReadPathCountCall{{
		url: pinsReadPinURL("devops"), pin: pinsReadDevopsCommit, tip: pinsReadDevopsTip,
		paths: daemonimage.PinInputPaths("devops"),
	}}, pathCount.calls, "the devops pin is counted once, with its inputs")
	require.Len(t, cmp.calls, 4, "the bd floor and the three pins with no input list are compared")
	for _, call := range cmp.calls {
		assert.NotEqual(t, "devops", call.repo, "the devops pin is counted through its inputs, not the compare")
	}
}

// TestReadImagePinTipsPathCountFailureIsThePinsErr: a path-limited count that
// fails, or answers a negative number, is that pin's Err naming it, and the
// other three pins are still read.
func TestReadImagePinTipsPathCountFailureIsThePinsErr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		pathCount *fakePinPathCount
		want      string
	}{
		{"a count that fails", &fakePinPathCount{err: errors.New("git fetch exited 128")}, "devops pin " + pinsReadDevopsCommit[:12] + ": cannot count"},
		{"a count below zero", &fakePinPathCount{behind: -1}, "which is not a count"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			git, cmp, _ := pinsReadAllHealthy()

			states, err := ReadImagePinTips(context.Background(), pinsReadFixture(pinsReadPinURL), nil, git.read, tt.pathCount.count, cmp)
			require.NoError(t, err, "a pin's failed count is not the file's error")
			require.Len(t, states, 4)

			require.Error(t, states[3].Err, "the failed devops count is that pin's error")
			assert.Contains(t, states[3].Err.Error(), tt.want)
			assert.Equal(t, "devops", states[3].Name, "the failed state still names its pin")
			assert.Equal(t, pinsReadDevopsCommit, states[3].Pin, "the failed state still names the file's commit")
			assert.Equal(t, pinsReadDevopsTip, states[3].Tip, "the tip read before the count is kept")
			assert.Equal(t, pinsReadPinsBeforeDevops(), states[:3], "the other three pins are still read")
		})
	}
}

// TestReadImagePinTipsNilPathSeamIsThePinsErr: a devops pin read with no
// path-count seam is that pin's Err naming it, never the compare count, which
// cannot say which of the commits touch its inputs; the other three pins are
// still read.
func TestReadImagePinTipsNilPathSeamIsThePinsErr(t *testing.T) {
	t.Parallel()
	git, cmp, _ := pinsReadAllHealthy()

	states, err := ReadImagePinTips(context.Background(), pinsReadFixture(pinsReadPinURL), nil, git.read, nil, cmp)
	require.NoError(t, err, "a pin's missing seam is not the file's error")
	require.Len(t, states, 4)

	require.Error(t, states[3].Err, "the devops pin read with no seam is its error")
	assert.Contains(t, states[3].Err.Error(), "devops pin "+pinsReadDevopsCommit[:12]+": no path count")
	assert.Equal(t, "devops", states[3].Name, "the failed state still names its pin")
	assert.Equal(t, pinsReadDevopsTip, states[3].Tip, "the tip read before the count is kept")
	assert.Equal(t, pinsReadPinsBeforeDevops(), states[:3], "the other three pins are still read")
	assert.Len(t, cmp.calls, 4, "the devops pin is not compared in place of the missing seam")
}
