package daemonimage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
	polecatseed "github.com/steveyegge/gastown/internal/polecat/seed"
)

// The pin commits below are fixtures for the parser and agreement tests. They
// are not the committed pins: TestCommittedImagePinsParse and
// TestCommittedPinsAgree read the real files, so a pin bump edits those files
// only.
const (
	pinBDCommit     = "3333333333333333333333333333333333333333"
	pinOMCommit     = "4444444444444444444444444444444444444444"
	pinHMCommit     = "5555555555555555555555555555555555555555"
	pinDevopsCommit = "6666666666666666666666666666666666666666"
)

// testOwner is the Forgejo owner every fixture pin URL names, and testPinURL
// builds the URL from it: the pins file a test hands the parser and the Pin it
// compares with use this one constant, so a rename cannot split the input from
// the expectation.
const testOwner = "owner"

func testPinURL(repo string) string { return "http://forgejo:3000/" + testOwner + "/" + repo + ".git" }

// pinsFile is the four pins a valid file holds, and the text the parser is
// handed for each test.
func validImagePins() string {
	return "# the four external sources\n" +
		"\n" +
		"bd " + testPinURL("beads") + " " + pinBDCommit + "\n" +
		"om " + testPinURL("organic-mechanic") + " " + pinOMCommit + "\n" +
		"hm " + testPinURL("history-man") + " " + pinHMCommit + "\n" +
		"devops " + testPinURL("devops") + " " + pinDevopsCommit + "\n"
}

func TestParseImagePins(t *testing.T) {
	t.Parallel()
	want := []Pin{
		{Name: "bd", URL: testPinURL("beads"), Commit: pinBDCommit},
		{Name: "om", URL: testPinURL("organic-mechanic"), Commit: pinOMCommit},
		{Name: "hm", URL: testPinURL("history-man"), Commit: pinHMCommit},
		{Name: "devops", URL: testPinURL("devops"), Commit: pinDevopsCommit},
	}
	got, err := ParsePins([]byte(validImagePins()))
	if err != nil {
		t.Fatalf("ParsePins: %v", err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ParsePins = %+v, want %+v", got, want)
	}

	line := func(n int, s string) []string { return []string{fmt.Sprintf("line %d", n), s} }
	for _, tc := range []struct {
		name    string
		file    string
		want    []string
		notWant []string
	}{
		{"an unknown name", validImagePins() + "tracker " + testPinURL("tracker") + " " + pinBDCommit + "\n",
			line(7, `unknown pin name "tracker"`), nil},
		{"a duplicate name", validImagePins() + "bd " + testPinURL("beads") + " " + pinBDCommit + "\n",
			line(7, `duplicate pin name "bd"`), nil},
		{"a short commit", strings.Replace(validImagePins(), pinBDCommit, pinBDCommit[:7], 1),
			line(3, "bd commit"), nil},
		{"a non-hex commit", strings.Replace(validImagePins(), pinBDCommit, strings.Repeat("g", 40), 1),
			line(3, "is not a full 40-character lowercase hex sha"), nil},
		{name: "a url with a credential",
			file:    strings.Replace(validImagePins(), testPinURL("beads"), "http://user:secret@forgejo:3000/"+testOwner+"/beads.git", 1),
			want:    line(3, "carries a credential"),
			notWant: []string{"secret"}},
		{name: "a url with a token in its query",
			file:    strings.Replace(validImagePins(), testPinURL("beads"), testPinURL("beads")+"?access_token=tok3n", 1),
			want:    line(3, "carries a query or fragment"),
			notWant: []string{"tok3n"}},
		{name: "a url with a token in its fragment",
			file:    strings.Replace(validImagePins(), testPinURL("beads"), testPinURL("beads")+"#tok3n", 1),
			want:    line(3, "carries a query or fragment"),
			notWant: []string{"tok3n"}},
		{"a url that is not http(s)", strings.Replace(validImagePins(), testPinURL("beads"), "git://forgejo:3000/"+testOwner+"/beads.git", 1),
			line(3, "is not http:// or https://"), nil},
		{"a url with no repository path", strings.Replace(validImagePins(), testPinURL("beads"), "http://forgejo:3000", 1),
			line(3, "names no repository path"), nil},
		{"a url with no host", strings.Replace(validImagePins(), testPinURL("beads"), "http:///"+testOwner+"/beads.git", 1),
			line(3, "names no host"), nil},
		{"a url that does not parse", strings.Replace(validImagePins(), testPinURL("beads"), "http://[::1", 1),
			line(3, "url does not parse"), nil},
		{"a url holding the owner placeholder", strings.Replace(validImagePins(), testPinURL("beads"),
			"http://forgejo:3000/"+config.ForgeOwnerPlaceholder+"/beads.git", 1),
			line(3, "nothing expanded"), nil},
		{"a line missing its name", strings.Replace(validImagePins(), "bd "+testPinURL("beads"), testPinURL("beads"), 1),
			line(3, `want "<name> <url> <commit>"`), nil},
		{"a line missing its commit", strings.Replace(validImagePins(), " "+pinBDCommit, "", 1),
			line(3, `want "<name> <url> <commit>"`), nil},
		{"a missing name", strings.Replace(validImagePins(), "devops "+testPinURL("devops")+" "+pinDevopsCommit+"\n", "", 1),
			[]string{"missing pin for devops"}, nil},
		{"an empty file", "# nothing here\n", []string{"missing pin for bd, om, hm, devops"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParsePins([]byte(tc.file))
			if err == nil {
				t.Fatal("ParsePins accepted the file")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("ParsePins error = %q, want it to contain %q", err, want)
				}
			}
			for _, secret := range tc.notWant {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("ParsePins error = %q, want it not to echo %q", err, secret)
				}
			}
		})
	}
}

const (
	pinsTestDir  = "/src/bd"
	pinsTestPin  = "1111111111111111111111111111111111111111"
	pinsTestMain = "2222222222222222222222222222222222222222"
	pinsTestPath = "/repo/deploy/daemon/image-pins"
)

// pinsMainAt is the fixed clock the fake git reads commit times against, so the
// age bound does not depend on when the test runs.
var pinsMainAt = time.Unix(1_780_000_000, 0)

// pinsGit answers git from a table keyed by the directory and arguments, and
// fails a call the test did not plan, so an unexpected read is a failure. errs
// holds the calls that fail, with the error git gave them.
type pinsGit struct {
	answer map[string]string
	errs   map[string]error
	// calls records every key the guard asked git for, so a test can assert
	// which counts it ran.
	calls []string
}

// exitStatus is the error a git command that exited with code gives: the exit
// status, which is what isNotAncestor reads. The fake leaves out the stderr line
// runGit adds.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func (e exitStatus) ExitCode() int { return int(e) }

func (f *pinsGit) run(dir string, args ...string) ([]byte, error) {
	key := dir + " " + strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if err, ok := f.errs[key]; ok {
		return nil, err
	}
	if out, ok := f.answer[key]; ok {
		return []byte(out), nil
	}
	return nil, fmt.Errorf("no answer for %q", key)
}

// pinTable fills every answer a healthy pin takes: its origin/main, the
// ancestry test, the commit count, and the two committer times, with the pin's
// commit pinAge older than main's tip.
func (f *pinsGit) pinTable(pin, main string, behind int, pinAge time.Duration) {
	f.answer[pinsTestDir+" rev-parse --verify origin/main^{commit}"] = main + "\n"
	f.answer[pinsTestDir+" merge-base --is-ancestor "+pin+" "+main] = ""
	f.answer[pinsTestDir+" rev-list --count "+pin+".."+main] = strconv.Itoa(behind) + "\n"
	f.answer[pinsTestDir+" show -s --format=%ct "+main] = strconv.FormatInt(pinsMainAt.Unix(), 10) + "\n"
	f.answer[pinsTestDir+" show -s --format=%ct "+pin] = strconv.FormatInt(pinsMainAt.Add(-pinAge).Unix(), 10) + "\n"
}

// TestCheckPinsRefusesAStalePin drives each bound with a fake git: a pin at main
// passes silently, a pin inside both bounds passes with one note, and a pin
// ahead or diverged, 41 commits behind, or 8 days older than main's tip is the
// pin-drift guard's error naming the rig, pin, main and the line to edit.
func TestCheckPinsRefusesAStalePin(t *testing.T) {
	t.Parallel()
	pin := Pin{Name: "bd", URL: testPinURL("beads"), Commit: pinsTestPin}
	mainOf := func(Pin) (string, error) { return pinsTestDir, nil }
	mainOfFailing := func(Pin) (string, error) { return "", fmt.Errorf("no source checkout named") }
	edit := "set the bd line in " + pinsTestPath + " to " + pinsTestMain
	for _, tc := range []struct {
		name       string
		setup      func(*pinsGit)
		mainOf     MainOf
		noGit      bool
		wantErr    []string
		notWantErr []string
		wantNote   string
	}{
		{
			name: "a pin at main",
			setup: func(f *pinsGit) {
				f.answer[pinsTestDir+" rev-parse --verify origin/main^{commit}"] = pinsTestPin + "\n"
			},
		},
		{
			name:     "a pin inside both bounds",
			setup:    func(f *pinsGit) { f.pinTable(pinsTestPin, pinsTestMain, 5, 2*24*time.Hour) },
			wantNote: "rig bd pin " + pinsTestPin[:12] + " is 5 commits behind main " + pinsTestMain[:12],
		},
		{
			name: "a pin that is not an ancestor of main",
			setup: func(f *pinsGit) {
				f.answer[pinsTestDir+" rev-parse --verify origin/main^{commit}"] = pinsTestMain + "\n"
				f.errs[pinsTestDir+" merge-base --is-ancestor "+pinsTestPin+" "+pinsTestMain] = exitStatus(1)
			},
			wantErr: []string{"pin-drift guard:", "is not an ancestor of main", "ahead or diverged", edit},
		},
		{
			name: "an ancestry check git cannot answer",
			setup: func(f *pinsGit) {
				f.answer[pinsTestDir+" rev-parse --verify origin/main^{commit}"] = pinsTestMain + "\n"
				f.errs[pinsTestDir+" merge-base --is-ancestor "+pinsTestPin+" "+pinsTestMain] = exitStatus(128)
			},
			wantErr: []string{"pin-drift guard:", "cannot test whether it is an ancestor of main", "fetch or fix that checkout"},
		},
		{
			name: "an ancestry check that fails with no exit status",
			setup: func(f *pinsGit) {
				f.answer[pinsTestDir+" rev-parse --verify origin/main^{commit}"] = pinsTestMain + "\n"
				f.errs[pinsTestDir+" merge-base --is-ancestor "+pinsTestPin+" "+pinsTestMain] = fmt.Errorf("git was canceled")
			},
			wantErr:    []string{"pin-drift guard:", "cannot test whether it is an ancestor of main", "fetch or fix that checkout"},
			notWantErr: []string{"git was canceled"},
		},
		{
			name:    "a pin 41 commits behind",
			setup:   func(f *pinsGit) { f.pinTable(pinsTestPin, pinsTestMain, MaxPinBehind+1, time.Hour) },
			wantErr: []string{"pin-drift guard:", "rig bd pin " + pinsTestPin[:12], "is 41 commits behind main " + pinsTestMain[:12], "over the 40-commit bound", edit},
		},
		{
			name:    "a pin more than 7 days older than main's tip",
			setup:   func(f *pinsGit) { f.pinTable(pinsTestPin, pinsTestMain, 3, MaxPinAge+24*time.Hour) },
			wantErr: []string{"pin-drift guard:", "older than main's tip", "over the " + MaxPinAge.String() + " bound", edit},
		},
		{
			name:    "an origin/main that cannot be read",
			wantErr: []string{"pin-drift guard:", "cannot read origin/main of rig bd in " + pinsTestDir},
		},
		{
			name: "a commit count that cannot be read",
			setup: func(f *pinsGit) {
				f.answer[pinsTestDir+" rev-parse --verify origin/main^{commit}"] = pinsTestMain + "\n"
				f.answer[pinsTestDir+" merge-base --is-ancestor "+pinsTestPin+" "+pinsTestMain] = ""
			},
			wantErr: []string{"pin-drift guard:", "cannot count the commits to main"},
		},
		{
			name: "a commit count that is not a number",
			setup: func(f *pinsGit) {
				f.pinTable(pinsTestPin, pinsTestMain, 3, time.Hour)
				f.answer[pinsTestDir+" rev-list --count "+pinsTestPin+".."+pinsTestMain] = "lots\n"
			},
			wantErr: []string{"pin-drift guard:", "cannot count the commits to main"},
		},
		{
			name: "a pin commit time that cannot be read",
			setup: func(f *pinsGit) {
				f.answer[pinsTestDir+" rev-parse --verify origin/main^{commit}"] = pinsTestMain + "\n"
				f.answer[pinsTestDir+" merge-base --is-ancestor "+pinsTestPin+" "+pinsTestMain] = ""
				f.answer[pinsTestDir+" rev-list --count "+pinsTestPin+".."+pinsTestMain] = "3\n"
			},
			wantErr: []string{"pin-drift guard:", "cannot read the commit times"},
		},
		{
			name: "a main commit time that cannot be read",
			setup: func(f *pinsGit) {
				f.answer[pinsTestDir+" rev-parse --verify origin/main^{commit}"] = pinsTestMain + "\n"
				f.answer[pinsTestDir+" merge-base --is-ancestor "+pinsTestPin+" "+pinsTestMain] = ""
				f.answer[pinsTestDir+" rev-list --count "+pinsTestPin+".."+pinsTestMain] = "3\n"
				f.answer[pinsTestDir+" show -s --format=%ct "+pinsTestPin] = strconv.FormatInt(pinsMainAt.Unix(), 10) + "\n"
			},
			wantErr: []string{"pin-drift guard:", "cannot read the commit times"},
		},
		{
			name: "a commit time that is not a number",
			setup: func(f *pinsGit) {
				f.pinTable(pinsTestPin, pinsTestMain, 3, time.Hour)
				f.answer[pinsTestDir+" show -s --format=%ct "+pinsTestPin] = "yesterday\n"
			},
			wantErr: []string{"pin-drift guard:", "cannot read the commit times"},
		},
		{
			name:    "a pin with no source checkout",
			mainOf:  mainOfFailing,
			wantErr: []string{"pin-drift guard:", "rig bd pin", "no source checkout named"},
		},
		{
			name:    "no git to read with",
			noGit:   true,
			wantErr: []string{"pin-drift guard:", "no git to read origin/main"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &pinsGit{answer: map[string]string{}, errs: map[string]error{}}
			if tc.setup != nil {
				tc.setup(f)
			}
			src := mainOf
			if tc.mainOf != nil {
				src = tc.mainOf
			}
			var git bdbuild.Git = f.run
			if tc.noGit {
				git = nil
			}
			notes, err := CheckPins([]Pin{pin}, src, pinsTestPath, git)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("CheckPins: %v", err)
				}
				if tc.wantNote == "" && len(notes) != 0 {
					t.Errorf("CheckPins notes = %v, want none for a pin at main", notes)
				}
				if tc.wantNote != "" && (len(notes) != 1 || !strings.Contains(notes[0], tc.wantNote)) {
					t.Errorf("CheckPins notes = %v, want one containing %q", notes, tc.wantNote)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckPins = %v, want a refusal", notes)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("CheckPins error = %q, want it to contain %q", err, want)
				}
			}
			for _, notWant := range tc.notWantErr {
				if strings.Contains(err.Error(), notWant) {
					t.Errorf("CheckPins error = %q, must not echo %q", err, notWant)
				}
			}
		})
	}
}

// devopsInputs is what the image builds from the devops repository, written out
// independently of PinInputPaths so a drifted list leaves the fake git with no
// answer for the path-limited count.
var devopsInputs = []string{
	"cmd/reporter", "cmd/release-reporter", "cmd/cloudcheck", "internal/ledgerdb",
	"pkg/patrol", "pkg/releasebot", "ops/cloud-check.apps", "go.mod", "go.sum",
}

// devopsPin is the devops fixture pin the counts below drive.
var devopsPin = Pin{Name: "devops", URL: testPinURL("devops"), Commit: pinsTestPin}

// inputCountKey is the fake git key of the count over the devops build inputs.
func inputCountKey() string {
	return pinsTestDir + " rev-list --count " + pinsTestPin + ".." + pinsTestMain + " -- " + strings.Join(devopsInputs, " ")
}

// devopsCounts fills the answers a devops pin takes: pinTable's, plus the count
// over its build inputs.
func devopsCounts(f *pinsGit, behind, touching int, pinAge time.Duration) {
	f.pinTable(pinsTestPin, pinsTestMain, behind, pinAge)
	f.answer[inputCountKey()] = strconv.Itoa(touching) + "\n"
}

// checkDevopsPins runs the guard on the devops fixture pin.
func checkDevopsPins(f *pinsGit) ([]string, error) {
	return CheckPins([]Pin{devopsPin}, func(Pin) (string, error) { return pinsTestDir, nil }, pinsTestPath, f.run)
}

// newPinsGit is a fake git a test has planned no answer for.
func newPinsGit() *pinsGit { return &pinsGit{answer: map[string]string{}, errs: map[string]error{}} }

// TestPinInputPathsNameTheDevopsBuildInputs checks the devops pin's inputs are
// exactly the three commands the image builds, the packages they import, the
// cloud-check file staged beside cloudcheck and the module files, and that a
// source the image builds from its whole repository has no input list.
func TestPinInputPathsNameTheDevopsBuildInputs(t *testing.T) {
	t.Parallel()
	if got := PinInputPaths("devops"); !reflect.DeepEqual(got, devopsInputs) {
		t.Errorf("PinInputPaths(devops) = %v, want %v", got, devopsInputs)
	}
	for _, name := range []string{"bd", "om", "hm", "tracker"} {
		if got := PinInputPaths(name); got != nil {
			t.Errorf("PinInputPaths(%s) = %v, want nil: that source is built from its whole repository", name, got)
		}
	}
}

// TestCommitsBehindCountsOnlyInputPaths checks the devops pin's count is asked
// for the commits touching its inputs, and a source with no input list is
// counted with no path limiting at all.
func TestCommitsBehindCountsOnlyInputPaths(t *testing.T) {
	t.Parallel()
	devops := newPinsGit()
	devopsCounts(devops, 3, 1, time.Hour)
	if _, err := checkDevopsPins(devops); err != nil {
		t.Fatalf("CheckPins: %v", err)
	}
	if !hasCall(devops, inputCountKey()) {
		t.Errorf("the guard ran no count over the devops inputs: %v", devops.calls)
	}
	bd := newPinsGit()
	bd.pinTable(pinsTestPin, pinsTestMain, 3, time.Hour)
	if _, err := CheckPins([]Pin{{Name: "bd", URL: testPinURL("beads"), Commit: pinsTestPin}},
		func(Pin) (string, error) { return pinsTestDir, nil }, pinsTestPath, bd.run); err != nil {
		t.Fatalf("CheckPins: %v", err)
	}
	for _, call := range bd.calls {
		if strings.HasPrefix(call, pinsTestDir+" rev-list") && strings.Contains(call, " -- ") {
			t.Errorf("the guard ran a path-limited count for a whole-repository source: %q", call)
		}
	}
}

// hasCall reports whether the fake git was asked key.
func hasCall(f *pinsGit, key string) bool {
	for _, call := range f.calls {
		if call == key {
			return true
		}
	}
	return false
}

// TestCheckPinsPassesAPinBehindOnlyByOtherFiles checks a devops pin far behind
// main fails only when the commits touch what the image builds from it: 54
// commits behind with 1 touching its inputs passes, with a note naming both
// numbers.
func TestCheckPinsPassesAPinBehindOnlyByOtherFiles(t *testing.T) {
	t.Parallel()
	f := newPinsGit()
	devopsCounts(f, 54, 1, 2*24*time.Hour)
	notes, err := checkDevopsPins(f)
	if err != nil {
		t.Fatalf("CheckPins: %v", err)
	}
	want := "rig devops pin " + pinsTestPin[:12] + " is 54 commits behind main " + pinsTestMain[:12] + ", 1 touching its inputs"
	if len(notes) != 1 || !strings.Contains(notes[0], want) {
		t.Errorf("CheckPins notes = %v, want one containing %q", notes, want)
	}
}

// TestCheckPinsRefusesAPinBehindOnItsInputs checks the 40-commit bound applies to
// the commits touching the devops inputs: 41 of them behind fails with both
// counts in the refusal.
func TestCheckPinsRefusesAPinBehindOnItsInputs(t *testing.T) {
	t.Parallel()
	f := newPinsGit()
	devopsCounts(f, 50, MaxPinBehind+1, time.Hour)
	_, err := checkDevopsPins(f)
	if err == nil {
		t.Fatal("CheckPins passed a pin 41 commits behind on its inputs")
	}
	for _, want := range []string{
		"pin-drift guard:",
		"rig devops pin " + pinsTestPin[:12] + " is 50 commits behind main " + pinsTestMain[:12],
		"41 touching its inputs",
		"over the 40-commit bound",
		"set the devops line in " + pinsTestPath + " to " + pinsTestMain,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("CheckPins error = %q, want it to contain %q", err, want)
		}
	}
}

// TestCheckPinsRefusesAnInputCountItCannotRead checks a path-limited count git
// fails or answers with a non-number is refused, never read as zero commits
// touching the inputs.
func TestCheckPinsRefusesAnInputCountItCannotRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		breakIt func(*pinsGit)
	}{
		{"a count git refuses", func(f *pinsGit) { f.errs[inputCountKey()] = errors.New("bad pathspec") }},
		{"a count that is not a number", func(f *pinsGit) { f.answer[inputCountKey()] = "lots\n" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newPinsGit()
			f.pinTable(pinsTestPin, pinsTestMain, 50, time.Hour)
			tc.breakIt(f)
			if _, err := checkDevopsPins(f); err == nil || !strings.Contains(err.Error(), "cannot count the commits to main") {
				t.Fatalf("CheckPins error = %v, want a refusal naming the count", err)
			}
		})
	}
}

// fetchedMainKey is the fake git key of the fetch checkFetchedPin runs for a
// pin's main tip, with flags between "fetch" and the pin URL.
func fetchedMainKey(url string, flags ...string) string {
	return pinsTestDir + " fetch " + strings.Join(flags, " ") + " " + url + " main:refs/remotes/origin/main"
}

// TestCheckFetchedPinFetchesFilteredFullHistoryForAPinWithPaths: a pin the image
// builds from listed paths is compared against a main fetched with
// --filter=blob:none and no --depth, since the path-limited count needs history
// back to a pin more than fetchTipDepth commits behind.
func TestCheckFetchedPinFetchesFilteredFullHistoryForAPinWithPaths(t *testing.T) {
	t.Parallel()
	f := newPinsGit()
	devopsCounts(f, 54, 1, time.Hour)
	want := fetchedMainKey(devopsPin.URL, "--filter=blob:none")
	f.answer[want] = ""
	if err := checkFetchedPin(f.run, pinsTestDir, devopsPin, pinsTestPath); err != nil {
		t.Fatalf("checkFetchedPin: %v", err)
	}
	if !hasCall(f, want) {
		t.Errorf("the guard did not fetch main filtered for a pin with paths: calls %v, want %q", f.calls, want)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, pinsTestDir+" fetch") && strings.Contains(call, "--depth") {
			t.Errorf("the guard depth-limited the fetch for a pin with paths: %q", call)
		}
	}
}

// TestCheckFetchedPinKeepsTheDepthLimitForAPinWithoutPaths: a pin the image
// builds from its whole repository still fetches its main tip at fetchTipDepth.
func TestCheckFetchedPinKeepsTheDepthLimitForAPinWithoutPaths(t *testing.T) {
	t.Parallel()
	pin := Pin{Name: "bd", URL: testPinURL("beads"), Commit: pinsTestPin}
	f := newPinsGit()
	f.pinTable(pinsTestPin, pinsTestMain, 3, time.Hour)
	want := fetchedMainKey(pin.URL, "--depth", strconv.Itoa(MaxPinBehind+1))
	f.answer[want] = ""
	if err := checkFetchedPin(f.run, pinsTestDir, pin, pinsTestPath); err != nil {
		t.Fatalf("checkFetchedPin: %v", err)
	}
	if !hasCall(f, want) {
		t.Errorf("the guard did not fetch main at the depth limit for a pin with no paths: calls %v, want %q", f.calls, want)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, pinsTestDir+" fetch") && strings.Contains(call, "--filter") {
			t.Errorf("the guard filtered the fetch for a pin with no paths: %q", call)
		}
	}
}

// TestCheckFetchedPinFetchFailureIsStillRefused: a failed filtered fetch is the
// existing refusal naming the pin, never a pass.
func TestCheckFetchedPinFetchFailureIsStillRefused(t *testing.T) {
	t.Parallel()
	f := newPinsGit()
	devopsCounts(f, 54, 1, time.Hour)
	f.answer[fetchedMainKey(devopsPin.URL, "--depth", strconv.Itoa(MaxPinBehind+1))] = ""
	f.errs[fetchedMainKey(devopsPin.URL, "--filter=blob:none")] = errors.New("no such ref")
	err := checkFetchedPin(f.run, pinsTestDir, devopsPin, pinsTestPath)
	if err == nil {
		t.Fatal("checkFetchedPin passed a pin whose main fetch failed")
	}
	for _, want := range []string{"pin-drift guard:", "cannot fetch main", "devops"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("checkFetchedPin error = %q, want it to contain %q", err, want)
		}
	}
}

// TestCommittedImagePinsParse reads the committed file with the real parser, so
// a hand edit that breaks its syntax or drops a name fails the gate.
func TestCommittedImagePinsParse(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", filepath.FromSlash(PinsPath))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	text, err := config.ExpandForgeOwner(string(body), config.DefaultForgeOwner)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	pins, err := ParsePins([]byte(text))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if len(pins) != len(PinNames) {
		t.Fatalf("%s parses to %d pins, want %d", path, len(pins), len(PinNames))
	}
	seen := make(map[string]bool, len(pins))
	for _, p := range pins {
		seen[p.Name] = true
	}
	for _, name := range PinNames {
		if !seen[name] {
			t.Errorf("%s names no %s pin", path, name)
		}
	}
}

// seedPinsPath is the polecat seed-pins file, relative to the repository root.
const seedPinsPath = "deploy/polecat/seed-pins"

// seedPinSources pairs each seed-pins rig with the image-pins source it is the
// same repository as: the seed names beads where the image names bd.
var seedPinSources = []struct{ rig, pin string }{
	{"beads", "bd"},
	{"devops", "devops"},
}

// agreeingPins is the check TestCommittedPinsAgree runs on the two files'
// bytes: every seed-pins line for a source the image file also pins must name
// the same commit, so one file cannot be bumped without the other. A
// disagreement names both files and the commits they name.
func agreeingPins(image, seed []byte) error {
	pins, err := ParsePins(image)
	if err != nil {
		return fmt.Errorf("%s: %w", PinsPath, err)
	}
	sources, err := polecatseed.Pins(seed)
	if err != nil {
		return fmt.Errorf("%s: %w", seedPinsPath, err)
	}
	imageCommit := make(map[string]string, len(pins))
	for _, p := range pins {
		imageCommit[p.Name] = p.Commit
	}
	seedCommit := make(map[string]string, len(sources))
	for _, s := range sources {
		seedCommit[s.Rig] = s.Commit
	}
	for _, src := range seedPinSources {
		want := imageCommit[src.pin]
		got, ok := seedCommit[src.rig]
		if !ok {
			return fmt.Errorf("%s names no %s line, but %s pins %s at %s", seedPinsPath, src.rig, PinsPath, src.pin, want)
		}
		if got != want {
			return fmt.Errorf("%s pins %s at %s, but %s pins %s at %s: bump both files to the same commit", seedPinsPath, src.rig, got, PinsPath, src.pin, want)
		}
	}
	return nil
}

// TestCommittedPinsAgree fails when the shipped seed-pins file names a different
// beads or devops commit than the shipped image-pins file's bd or devops commit.
func TestCommittedPinsAgree(t *testing.T) {
	t.Parallel()
	image, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(PinsPath)))
	if err != nil {
		t.Fatalf("reading %s: %v", PinsPath, err)
	}
	seed, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(seedPinsPath)))
	if err != nil {
		t.Fatalf("reading %s: %v", seedPinsPath, err)
	}
	text, err := config.ExpandForgeOwner(string(image), config.DefaultForgeOwner)
	if err != nil {
		t.Fatalf("%s: %v", PinsPath, err)
	}
	if err := agreeingPins([]byte(text), seed); err != nil {
		t.Error(err)
	}
}

// TestAgreeingPinsRefuses drives each refusal of the agreement check with a
// fixture: a seed commit the image file does not name, a seed file missing one
// of the two sources, and a file that does not parse. Each refusal names the
// file it found the disagreement in.
func TestAgreeingPinsRefuses(t *testing.T) {
	t.Parallel()
	otherSHA := strings.Repeat("a", 40)
	seedWith := func(beads, devops string) string {
		return "beads " + beads + "\ndevops " + devops + "\n"
	}
	for _, tc := range []struct {
		name    string
		image   string
		seed    string
		wantErr []string
	}{
		{
			name:  "files that agree",
			image: validImagePins(),
			seed:  seedWith(pinBDCommit, pinDevopsCommit),
		},
		{
			name:    "a beads commit the image file does not name",
			image:   validImagePins(),
			seed:    seedWith(otherSHA, pinDevopsCommit),
			wantErr: []string{seedPinsPath + " pins beads at " + otherSHA, PinsPath + " pins bd at " + pinBDCommit},
		},
		{
			name:    "a devops commit the image file does not name",
			image:   validImagePins(),
			seed:    seedWith(pinBDCommit, otherSHA),
			wantErr: []string{seedPinsPath + " pins devops at " + otherSHA, PinsPath + " pins devops at " + pinDevopsCommit},
		},
		{
			name:    "a seed file with no beads line",
			image:   validImagePins(),
			seed:    "devops " + pinDevopsCommit + "\n",
			wantErr: []string{seedPinsPath + " names no beads line, but " + PinsPath + " pins bd at " + pinBDCommit},
		},
		{
			name:    "a seed file with no devops line",
			image:   validImagePins(),
			seed:    "beads " + pinBDCommit + "\n",
			wantErr: []string{seedPinsPath + " names no devops line, but " + PinsPath + " pins devops at " + pinDevopsCommit},
		},
		{
			name:    "a seed file that names no rig",
			image:   validImagePins(),
			seed:    "# nothing here\n",
			wantErr: []string{seedPinsPath, "names no rig"},
		},
		{
			name:    "an image file that does not parse",
			image:   "bd " + pinBDCommit + "\n",
			seed:    seedWith(pinBDCommit, pinDevopsCommit),
			wantErr: []string{PinsPath, "want \"<name> <url> <commit>\""},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := agreeingPins([]byte(tc.image), []byte(tc.seed))
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("agreeingPins: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("agreeingPins accepted the files")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("agreeingPins error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}
