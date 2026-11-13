package daemonimage

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// refreshTip is the origin/main commit each fixture source checkout holds: a
// distinct sha per source, so a test cannot pass by writing the wrong source's
// tip.
var refreshTip = map[string]string{
	"bd":     strings.Repeat("8", 40),
	"om":     strings.Repeat("9", 40),
	"hm":     strings.Repeat("a", 40),
	"devops": strings.Repeat("b", 40),
}

// refreshSourceDir is the checkout the fixture source is read from.
func refreshSourceDir(name string) string { return "/src/" + name }

// refreshMainOf maps each pin to its fixture checkout.
func refreshMainOf(p Pin) (string, error) { return refreshSourceDir(p.Name), nil }

// refreshAnswers is the git answers a healthy source takes: its origin/main is
// its tip, its pin is an ancestor of the tip, and the pin is behind by behind
// commits.
func refreshAnswers(name, pin string, behind int) map[string]string {
	dir, tip := refreshSourceDir(name), refreshTip[name]
	return map[string]string{
		dir + " rev-parse --verify origin/main^{commit}":     tip + "\n",
		dir + " merge-base --is-ancestor " + pin + " " + tip: "",
		dir + " rev-list --count " + pin + ".." + tip:        strconv.Itoa(behind) + "\n",
	}
}

// TestRefreshPinsMovesOnlyTheStalePins drives the planner with the four pins of
// the fixture file: bd is 30 behind and devops exactly 20 behind, so both move;
// om is 5 behind and hm 19 behind, so both keep their line.
func TestRefreshPinsMovesOnlyTheStalePins(t *testing.T) {
	t.Parallel()
	pins := mustParsePins(t, validImagePins())
	behind := map[string]int{"bd": 30, "om": 5, "hm": 19, "devops": 20}
	answer := make(map[string]string)
	for _, p := range pins {
		for k, v := range refreshAnswers(p.Name, p.Commit, behind[p.Name]) {
			answer[k] = v
		}
	}
	git := &pinsGit{answer: answer, errs: map[string]error{}}

	got, err := PlanPinRefresh(pins, refreshMainOf, git.run)
	if err != nil {
		t.Fatalf("PlanPinRefresh: %v", err)
	}
	want := []PinChange{
		{Name: "bd", From: pinBDCommit, To: refreshTip["bd"], Behind: 30},
		{Name: "devops", From: pinDevopsCommit, To: refreshTip["devops"], Behind: 20},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PlanPinRefresh = %+v, want %+v", got, want)
	}
}

// TestRefreshPinsRefusesWhatItCannotPlace drives each refusal of the planner
// with one source's git answer broken: the error names that source, and no
// change is returned for any pin.
func TestRefreshPinsRefusesWhatItCannotPlace(t *testing.T) {
	t.Parallel()
	pins := mustParsePins(t, validImagePins())
	for _, tc := range []struct {
		name    string
		source  string
		breakIt func(g *pinsGit, dir, pin, tip string)
		noMain  bool
		wantErr []string
	}{
		{
			name:   "a source whose origin/main cannot be read",
			source: "hm",
			breakIt: func(g *pinsGit, dir, _, _ string) {
				g.errs[dir+" rev-parse --verify origin/main^{commit}"] = errors.New("unknown revision")
			},
			wantErr: []string{"hm", "origin/main"},
		},
		{
			name:   "a pin that is not an ancestor of its tip",
			source: "devops",
			breakIt: func(g *pinsGit, dir, pin, tip string) {
				g.errs[dir+" merge-base --is-ancestor "+pin+" "+tip] = exitStatus(1)
			},
			wantErr: []string{"devops", "not an ancestor"},
		},
		{
			name:   "an ancestry answer that is not a yes or no",
			source: "om",
			breakIt: func(g *pinsGit, dir, pin, tip string) {
				g.errs[dir+" merge-base --is-ancestor "+pin+" "+tip] = exitStatus(128)
			},
			wantErr: []string{"om", "cannot test"},
		},
		{
			name:   "a commit count that cannot be read",
			source: "bd",
			breakIt: func(g *pinsGit, dir, pin, tip string) {
				g.errs[dir+" rev-list --count "+pin+".."+tip] = errors.New("bad object")
			},
			wantErr: []string{"bd", "cannot count"},
		},
		{
			name:    "a source with no checkout",
			source:  "om",
			noMain:  true,
			wantErr: []string{"om"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			answer := make(map[string]string)
			for _, p := range pins {
				for k, v := range refreshAnswers(p.Name, p.Commit, 30) {
					answer[k] = v
				}
			}
			git := &pinsGit{answer: answer, errs: map[string]error{}}
			if tc.breakIt != nil {
				tc.breakIt(git, refreshSourceDir(tc.source), pinFor(pins, tc.source), refreshTip[tc.source])
			}
			mainOf := refreshMainOf
			if tc.noMain {
				mainOf = func(p Pin) (string, error) {
					if p.Name == tc.source {
						return "", errors.New("no checkout named")
					}
					return refreshMainOf(p)
				}
			}
			got, err := PlanPinRefresh(pins, mainOf, git.run)
			if err == nil {
				t.Fatalf("PlanPinRefresh = %+v, want a refusal", got)
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %q", err, w)
				}
			}
		})
	}
}

// TestRewritePinsCommitsTouchesOnlyTheCommitColumn checks the rewrite keeps every
// other byte: comments, the {owner} placeholder in a URL, spacing, CRLF line
// endings and line order.
func TestRewritePinsCommitsTouchesOnlyTheCommitColumn(t *testing.T) {
	t.Parallel()
	const next = "cccccccccccccccccccccccccccccccccccccccc"
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "comments, placeholder and spacing survive",
			in:   "# the four sources\n\nbd  http://forgejo:3000/{owner}/beads.git    " + pinBDCommit + "\nom http://forgejo:3000/{owner}/organic-mechanic.git " + pinOMCommit + "\n",
			want: "# the four sources\n\nbd  http://forgejo:3000/{owner}/beads.git    " + next + "\nom http://forgejo:3000/{owner}/organic-mechanic.git " + pinOMCommit + "\n",
		},
		{
			name: "CRLF line endings survive",
			in:   "bd " + testPinURL("beads") + " " + pinBDCommit + "\r\nom " + testPinURL("organic-mechanic") + " " + pinOMCommit + "\r\n",
			want: "bd " + testPinURL("beads") + " " + next + "\r\nom " + testPinURL("organic-mechanic") + " " + pinOMCommit + "\r\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := RewritePinCommits([]byte(tc.in), map[string]string{"bd": next}, PinsPath)
			if err != nil {
				t.Fatalf("RewritePinCommits: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("RewritePinCommits =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// TestRewritePinsCommitsRefusesAMissingLine checks the rewrite refuses rather
// than writing a file it cannot place: a key with no line is an error naming the
// key and the file, so a source is never dropped silently, and a key line with
// no commit to replace is an error, so the key is never overwritten as if it
// were the commit.
func TestRewritePinsCommitsRefusesAMissingLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		in      string
		wantErr []string
	}{
		{
			name:    "a key with no line",
			in:      "bd " + testPinURL("beads") + " " + pinBDCommit + "\n",
			wantErr: []string{PinsPath, "devops"},
		},
		{
			name:    "a key line with no commit",
			in:      "bd " + testPinURL("beads") + " " + pinBDCommit + "\ndevops\n",
			wantErr: []string{PinsPath, "line 2", "devops"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := RewritePinCommits([]byte(tc.in), map[string]string{"devops": pinDevopsCommit}, PinsPath)
			if err == nil {
				t.Fatalf("RewritePinCommits = %q, want a refusal", got)
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %q", err, w)
				}
			}
		})
	}
}

// TestRefreshPinsFilesRefusesAnImageWithNoLine checks the two files are refused
// together: an image file with no line for a moved pin returns no bytes for
// either file, and the error names the image file.
func TestRefreshPinsFilesRefusesAnImageWithNoLine(t *testing.T) {
	t.Parallel()
	image := []byte("bd " + testPinURL("beads") + " " + pinBDCommit + "\n")
	seed := []byte("beads " + pinBDCommit + "\ndevops " + pinDevopsCommit + "\n")
	const next = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	newImage, newSeed, err := RefreshPinFiles(image, seed, PinsPath, seedPinsPath,
		map[string]string{"devops": next}, map[string]string{"beads": pinBDCommit, "devops": next})
	if err == nil {
		t.Fatalf("RefreshPinFiles = %q, %q, want a refusal", newImage, newSeed)
	}
	if !strings.Contains(err.Error(), PinsPath) {
		t.Errorf("error %q does not name %q", err, PinsPath)
	}
	if newImage != nil || newSeed != nil {
		t.Errorf("a refused refresh returned bytes: %q, %q", newImage, newSeed)
	}
}

// TestRefreshSeedPinsKeepsTheFilesAgreeing refreshes the image file's bd and the
// seed file's beads line, then checks TestCommittedPinsAgree's rule holds on the
// two refreshed files: each shared source names the same commit in both.
func TestRefreshSeedPinsKeepsTheFilesAgreeing(t *testing.T) {
	t.Parallel()
	const next = "dddddddddddddddddddddddddddddddddddddddd"
	image := []byte(validImagePins())
	seed := []byte("gastown fb1fa4d818410b99cb01538802999480655f5d58\nbeads " + pinBDCommit + "\ndevops " + pinDevopsCommit + "\n")
	if err := agreeingPins(image, seed); err != nil {
		t.Fatalf("fixture files disagree before the refresh: %v", err)
	}

	image, err := RewritePinCommits(image, map[string]string{"bd": next}, PinsPath)
	if err != nil {
		t.Fatalf("RewritePinCommits: %v", err)
	}
	commits := map[string]string{"bd": next, "devops": pinDevopsCommit}
	seed, err = RefreshSeedPins(seed, SeedCommits(commits), seedPinsPath)
	if err != nil {
		t.Fatalf("RefreshSeedPins: %v", err)
	}
	if err := agreeingPins(image, seed); err != nil {
		t.Errorf("refreshed files disagree: %v", err)
	}
	if !strings.Contains(string(seed), "gastown fb1fa4d818410b99cb01538802999480655f5d58\n") {
		t.Errorf("the gastown line was touched: %q", seed)
	}
}

// TestRefreshSeedPinsRefusesAFileThatDoesNotParse checks a seed file the parser
// refuses is an error naming the file, and nothing is returned for it.
func TestRefreshSeedPinsRefusesAFileThatDoesNotParse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		seed string
	}{
		{name: "an empty file", seed: "# nothing\n"},
		{name: "a line with no commit", seed: "beads\ndevops " + pinDevopsCommit + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := RefreshSeedPins([]byte(tc.seed), map[string]string{"devops": pinDevopsCommit}, seedPinsPath)
			if err == nil {
				t.Fatalf("RefreshSeedPins = %q, want a refusal", got)
			}
			if !strings.Contains(err.Error(), seedPinsPath) {
				t.Errorf("error %q does not name %q", err, seedPinsPath)
			}
			if got != nil {
				t.Errorf("RefreshSeedPins returned bytes with its error: %q", got)
			}
		})
	}
}

// mustParsePins parses text as an image-pins file, failing the test if it does
// not parse.
func mustParsePins(t *testing.T, text string) []Pin {
	t.Helper()
	pins, err := ParsePins([]byte(text))
	if err != nil {
		t.Fatalf("ParsePins: %v", err)
	}
	return pins
}

// pinFor is the pin's commit in pins, by name.
func pinFor(pins []Pin, name string) string {
	for _, p := range pins {
		if p.Name == name {
			return p.Commit
		}
	}
	return ""
}
