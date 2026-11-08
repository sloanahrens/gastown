package daemonimage

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
	polecatseed "github.com/steveyegge/gastown/internal/polecat/seed"
)

const (
	pinBDCommit     = "ecf0fc58c789b6d5a158bcf4b485662bc15f296d"
	pinOMCommit     = "218e13d1f6727a9b343239615c7b5f009c0ddd19"
	pinHMCommit     = "e207fe9a913a7b3cddbdb631a04bcb81156f1fdf"
	pinDevopsCommit = "09bcaa41d4720129c4c89779f924c7824fa77dcb"
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
}

// exitStatus is the error a git command that exited with code gives: the exit
// status, which is what isNotAncestor reads. The fake leaves out the stderr line
// runGit adds.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func (e exitStatus) ExitCode() int { return int(e) }

func (f *pinsGit) run(dir string, args ...string) ([]byte, error) {
	key := dir + " " + strings.Join(args, " ")
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

// TestCommittedImagePinsParse reads the committed file with the real parser, so
// a hand edit that breaks its syntax or drops a name fails the gate.
func TestCommittedImagePinsParse(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", filepath.FromSlash(PinsPath))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	pins, err := ParsePins(body)
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
	if err := agreeingPins(image, seed); err != nil {
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
			wantErr: []string{seedPinsPath, otherSHA, PinsPath, "bd", pinBDCommit},
		},
		{
			name:    "a devops commit the image file does not name",
			image:   validImagePins(),
			seed:    seedWith(pinBDCommit, otherSHA),
			wantErr: []string{seedPinsPath, otherSHA, PinsPath, "devops", pinDevopsCommit},
		},
		{
			name:    "a seed file with no beads line",
			image:   validImagePins(),
			seed:    "devops " + pinDevopsCommit + "\n",
			wantErr: []string{seedPinsPath, "names no beads line", PinsPath, "bd", pinBDCommit},
		},
		{
			name:    "a seed file with no devops line",
			image:   validImagePins(),
			seed:    "beads " + pinBDCommit + "\n",
			wantErr: []string{seedPinsPath, "names no devops line", PinsPath, "devops", pinDevopsCommit},
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
