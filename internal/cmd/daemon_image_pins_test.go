package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/daemonimage"
)

// pinsRefreshTip is the origin/main each fixture source checkout holds: a
// distinct full sha per source, so a test cannot pass by writing another
// source's tip.
var pinsRefreshTip = map[string]string{
	"bd":     strings.Repeat("1", 40),
	"om":     strings.Repeat("2", 40),
	"hm":     strings.Repeat("3", 40),
	"devops": strings.Repeat("4", 40),
}

// pinsRefreshPin is the commit each fixture image pin names before a refresh.
var pinsRefreshPin = map[string]string{
	"bd":     imageTestBDCommit,
	"om":     imageTestOMCommit,
	"hm":     imageTestHMCommit,
	"devops": imageTestDevCommit,
}

// pinsRefreshSeed is the seed-pins file a refresh starts from: a comment, the
// gastown line the refresh never touches, and the beads and devops lines that
// follow the image's bd and devops commits.
func pinsRefreshSeed() string {
	return "# the polecat seed\n" +
		"gastown fb1fa4d818410b99cb01538802999480655f5d58\n" +
		"beads " + imageTestBDCommit + "\n" +
		"devops " + imageTestDevCommit + "\n"
}

// pinsRefreshGit answers the refresh's git reads from a table keyed by the
// checkout and the arguments. A call the test did not plan fails, so an
// unexpected read is a failure, and the command runs no real git.
type pinsRefreshGit struct {
	answer map[string]string
	errs   map[string]error
}

func (g *pinsRefreshGit) run(dir string, args ...string) ([]byte, error) {
	key := dir + " " + strings.Join(args, " ")
	if err, ok := g.errs[key]; ok {
		return nil, err
	}
	if out, ok := g.answer[key]; ok {
		return []byte(out), nil
	}
	return nil, errors.New("no answer for " + key)
}

// pinsRefreshRig is a set of real files and checkout directories for the
// command, with the git answers each source takes. The default fixture is a
// bd pin 30 behind its tip, an om pin 5 behind, an hm pin at its tip, and a
// devops pin 25 behind.
type pinsRefreshRig struct {
	flags   daemonImagePinsFlags
	git     *pinsRefreshGit
	write   func(path string, data []byte) error
	resolve func(path string) (string, error)
	out     *bytes.Buffer
	src     map[string]string
}

func newPinsRefreshRig(t *testing.T) *pinsRefreshRig {
	t.Helper()
	root := t.TempDir()
	r := &pinsRefreshRig{
		git:     &pinsRefreshGit{answer: map[string]string{}, errs: map[string]error{}},
		resolve: func(p string) (string, error) { return p, nil },
		write:   func(path string, data []byte) error { return os.WriteFile(path, data, 0o644) },
		out:     &bytes.Buffer{},
		src:     map[string]string{},
	}
	for _, name := range []string{"bd", "om", "hm", "devops"} {
		dir := filepath.Join(root, "src", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		r.src[name] = dir
	}
	image := filepath.Join(root, "image-pins")
	seed := filepath.Join(root, "seed-pins")
	writePinsRefreshFile(t, image, "# the four external sources\n\n"+imagePinsText())
	writePinsRefreshFile(t, seed, pinsRefreshSeed())
	r.flags = daemonImagePinsFlags{
		daemonImageFlags: daemonImageFlags{
			pins:      image,
			bdSrc:     r.src["bd"],
			omSrc:     r.src["om"],
			hmSrc:     r.src["hm"],
			devopsSrc: r.src["devops"],
		},
		seedPins: seed,
	}
	r.setSource("bd", pinsRefreshTip["bd"], 30)
	r.setSource("om", pinsRefreshTip["om"], 5)
	r.setSource("hm", pinsRefreshPin["hm"], 0)
	r.setSource("devops", pinsRefreshTip["devops"], 25)
	return r
}

// setSource answers the git reads for source name: its origin/main is tip, and
// its pin is an ancestor of tip behind by behind commits. A pin at its tip needs
// no further read.
func (r *pinsRefreshRig) setSource(name, tip string, behind int) {
	dir, pin := r.src[name], pinsRefreshPin[name]
	r.git.answer[dir+" rev-parse --verify origin/main^{commit}"] = tip + "\n"
	if tip == pin {
		return
	}
	r.git.answer[dir+" merge-base --is-ancestor "+pin+" "+tip] = ""
	r.git.answer[dir+" rev-list --count "+pin+".."+tip] = strconv.Itoa(behind) + "\n"
}

// deps is the command's host seams for the rig: its git, identity path
// resolution, and the rig's output buffer.
func (r *pinsRefreshRig) deps() daemonImagePinsDeps {
	return daemonImagePinsDeps{
		git:          r.git.run,
		evalSymlinks: r.resolve,
		write:        r.write,
		out:          r.out,
	}
}

// run runs the command on the rig with apply set to apply.
func (r *pinsRefreshRig) run(apply bool) error {
	f := r.flags
	f.apply = apply
	return runDaemonImagePins(f, r.deps())
}

func writePinsRefreshFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readPinsRefreshFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestDaemonImagePinsRefreshRewritesBothFiles runs the refresh with --apply and
// checks a pin 30 behind is rewritten to its tip in both files, a devops pin 25
// behind likewise, and a pin 5 behind is left unchanged, with the gastown line
// and the comment untouched.
func TestDaemonImagePinsRefreshRewritesBothFiles(t *testing.T) {
	t.Parallel()
	r := newPinsRefreshRig(t)
	if err := r.run(true); err != nil {
		t.Fatalf("refresh --apply: %v\n%s", err, r.out)
	}

	wantImage := "# the four external sources\n\n" +
		"bd " + imageTestPinURL("beads") + " " + pinsRefreshTip["bd"] + "\n" +
		"om " + imageTestPinURL("organic-mechanic") + " " + imageTestOMCommit + "\n" +
		"hm " + imageTestPinURL("history-man") + " " + imageTestHMCommit + "\n" +
		"devops " + imageTestPinURL("devops") + " " + pinsRefreshTip["devops"] + "\n"
	if got := readPinsRefreshFile(t, r.flags.pins); got != wantImage {
		t.Errorf("image-pins after refresh:\n%s\nwant:\n%s", got, wantImage)
	}
	wantSeed := "# the polecat seed\n" +
		"gastown fb1fa4d818410b99cb01538802999480655f5d58\n" +
		"beads " + pinsRefreshTip["bd"] + "\n" +
		"devops " + pinsRefreshTip["devops"] + "\n"
	if got := readPinsRefreshFile(t, r.flags.seedPins); got != wantSeed {
		t.Errorf("seed-pins after refresh:\n%s\nwant:\n%s", got, wantSeed)
	}
	if !strings.Contains(r.out.String(), "wrote ") {
		t.Errorf("output does not report the write:\n%s", r.out)
	}
}

// TestDaemonImagePinsDryRunWritesNothing runs the refresh without --apply: it
// prints the pins it would move and leaves both files byte-identical.
func TestDaemonImagePinsDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	r := newPinsRefreshRig(t)
	imageBefore := readPinsRefreshFile(t, r.flags.pins)
	seedBefore := readPinsRefreshFile(t, r.flags.seedPins)

	if err := r.run(false); err != nil {
		t.Fatalf("dry run: %v\n%s", err, r.out)
	}
	if got := readPinsRefreshFile(t, r.flags.pins); got != imageBefore {
		t.Errorf("dry run changed the image-pins file:\n%s", got)
	}
	if got := readPinsRefreshFile(t, r.flags.seedPins); got != seedBefore {
		t.Errorf("dry run changed the seed-pins file:\n%s", got)
	}
	for _, want := range []string{
		"bd: pin " + imageTestBDCommit[:12] + " -> " + pinsRefreshTip["bd"][:12] + " (30 commits behind origin/main)",
		"om: pin " + imageTestOMCommit[:12] + " stays",
		"devops: pin " + imageTestDevCommit[:12] + " -> " + pinsRefreshTip["devops"][:12] + " (25 commits behind origin/main)",
		"dry run: nothing was written",
	} {
		if !strings.Contains(r.out.String(), want) {
			t.Errorf("dry-run output does not contain %q:\n%s", want, r.out)
		}
	}
}

// TestDaemonImagePinsRefusesBeforeWriting drives each refusal with the rig
// broken one way: both files stay byte-identical, and the error names the source
// or the file that cannot be read or placed.
func TestDaemonImagePinsRefusesBeforeWriting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		breakIt func(t *testing.T, r *pinsRefreshRig)
		wantErr []string
	}{
		{
			name: "a source whose origin/main cannot be read",
			breakIt: func(t *testing.T, r *pinsRefreshRig) {
				r.git.errs[r.src["om"]+" rev-parse --verify origin/main^{commit}"] = errors.New("unknown revision")
			},
			wantErr: []string{"om", "origin/main"},
		},
		{
			name: "a pin that is not an ancestor of its tip",
			breakIt: func(t *testing.T, r *pinsRefreshRig) {
				r.setSource("hm", pinsRefreshTip["hm"], 0)
				dir, pin := r.src["hm"], imageTestHMCommit
				r.git.errs[dir+" merge-base --is-ancestor "+pin+" "+pinsRefreshTip["hm"]] = refreshExitStatus(1)
			},
			wantErr: []string{"hm", "not an ancestor"},
		},
		{
			name: "a source with no checkout named",
			breakIt: func(t *testing.T, r *pinsRefreshRig) {
				r.flags.omSrc = ""
			},
			wantErr: []string{"--om-src"},
		},
		{
			name: "an image-pins file that does not parse",
			breakIt: func(t *testing.T, r *pinsRefreshRig) {
				writePinsRefreshFile(t, r.flags.pins, strings.Replace(imagePinsText(), imageTestBDCommit, "notasha", 1))
			},
			wantErr: []string{"image-pins"},
		},
		{
			name: "a seed-pins file that does not parse",
			breakIt: func(t *testing.T, r *pinsRefreshRig) {
				writePinsRefreshFile(t, r.flags.seedPins, "beads\ndevops "+imageTestDevCommit+"\n")
			},
			wantErr: []string{"seed-pins"},
		},
		{
			name: "a seed-pins file with no devops line",
			breakIt: func(t *testing.T, r *pinsRefreshRig) {
				writePinsRefreshFile(t, r.flags.seedPins, "beads "+imageTestBDCommit+"\n")
			},
			wantErr: []string{"seed-pins", "devops"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newPinsRefreshRig(t)
			tc.breakIt(t, r)
			imageBefore := readPinsRefreshFile(t, r.flags.pins)
			seedBefore := readPinsRefreshFile(t, r.flags.seedPins)

			err := r.run(true)
			if err == nil {
				t.Fatal("refresh --apply returned no error")
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %q", err, w)
				}
			}
			if got := readPinsRefreshFile(t, r.flags.pins); got != imageBefore {
				t.Errorf("a refused refresh changed the image-pins file:\n%s", got)
			}
			if got := readPinsRefreshFile(t, r.flags.seedPins); got != seedBefore {
				t.Errorf("a refused refresh changed the seed-pins file:\n%s", got)
			}
		})
	}
}

// refreshExitStatus is the error a git command exiting with code gives: the
// shape isNotAncestor reads, as the fake git answers it.
type refreshExitStatus int

func (e refreshExitStatus) Error() string { return "exit status " + strconv.Itoa(int(e)) }

func (e refreshExitStatus) ExitCode() int { return int(e) }

// TestDaemonImagePinsWriteFailuresNameTheFile fails each write in turn: a failed
// image write leaves the seed file unwritten and says so, since the image file may
// be partly written; a failed seed write after the image write names the seed file
// and says the image was written, so the operator knows which file disagrees; and
// a failed seed write with no image write claims nothing about the image file.
func TestDaemonImagePinsWriteFailuresNameTheFile(t *testing.T) {
	t.Parallel()
	t.Run("the image write fails", func(t *testing.T) {
		t.Parallel()
		r := newPinsRefreshRig(t)
		seedBefore := readPinsRefreshFile(t, r.flags.seedPins)
		r.write = func(path string, data []byte) error {
			if path == r.flags.pins {
				return errors.New("disk full")
			}
			return os.WriteFile(path, data, 0o644)
		}
		err := r.run(true)
		if err == nil {
			t.Fatal("refresh --apply returned no error")
		}
		for _, w := range []string{r.flags.pins, "--seed-pins was not written", "may be partly written"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("error %q does not contain %q", err, w)
			}
		}
		if got := readPinsRefreshFile(t, r.flags.seedPins); got != seedBefore {
			t.Errorf("seed-pins changed after the image write failed:\n%s", got)
		}
	})
	t.Run("the seed write fails after the image write", func(t *testing.T) {
		t.Parallel()
		r := newPinsRefreshRig(t)
		r.write = func(path string, data []byte) error {
			if path == r.flags.seedPins {
				return errors.New("disk full")
			}
			return os.WriteFile(path, data, 0o644)
		}
		err := r.run(true)
		if err == nil {
			t.Fatal("refresh --apply returned no error")
		}
		for _, w := range []string{r.flags.seedPins, "was written"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("error %q does not contain %q", err, w)
			}
		}
		if got := readPinsRefreshFile(t, r.flags.pins); !strings.Contains(got, pinsRefreshTip["bd"]) {
			t.Errorf("image-pins does not carry the refreshed bd pin after its write:\n%s", got)
		}
	})
	t.Run("the seed write fails with no image write", func(t *testing.T) {
		t.Parallel()
		r := newPinsRefreshRig(t)
		// The image pins stay put, so the image file is not written, while the seed
		// file's beads line trails the image's bd commit and must be rewritten.
		r.setSource("bd", pinsRefreshTip["bd"], 5)
		r.setSource("devops", pinsRefreshTip["devops"], 5)
		writePinsRefreshFile(t, r.flags.seedPins, "beads "+pinsRefreshTip["bd"]+"\ndevops "+imageTestDevCommit+"\n")
		imageBefore := readPinsRefreshFile(t, r.flags.pins)
		r.write = func(path string, data []byte) error {
			if path == r.flags.seedPins {
				return errors.New("disk full")
			}
			return os.WriteFile(path, data, 0o644)
		}
		err := r.run(true)
		if err == nil {
			t.Fatal("refresh --apply returned no error")
		}
		for _, w := range []string{r.flags.seedPins, "may be partly written", "--pins was not changed"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("error %q does not contain %q", err, w)
			}
		}
		if got := readPinsRefreshFile(t, r.flags.pins); got != imageBefore {
			t.Errorf("image-pins changed though only the seed write was attempted:\n%s", got)
		}
	})
}

// TestDaemonImagePinsApplyWithNothingToRefreshWritesNothing runs --apply when no
// pin trails its tip by PinRefreshBehind commits and the seed file already agrees:
// the command says there is nothing to refresh and never reaches the write seam.
func TestDaemonImagePinsApplyWithNothingToRefreshWritesNothing(t *testing.T) {
	t.Parallel()
	r := newPinsRefreshRig(t)
	r.setSource("bd", pinsRefreshTip["bd"], 5)
	r.setSource("devops", pinsRefreshTip["devops"], 5)
	imageBefore := readPinsRefreshFile(t, r.flags.pins)
	seedBefore := readPinsRefreshFile(t, r.flags.seedPins)
	r.write = func(path string, data []byte) error {
		t.Errorf("--apply with nothing to refresh wrote %s", path)
		return nil
	}

	if err := r.run(true); err != nil {
		t.Fatalf("apply with nothing to refresh: %v\n%s", err, r.out)
	}
	if !strings.Contains(r.out.String(), "nothing to refresh") {
		t.Errorf("output does not say there is nothing to refresh:\n%s", r.out)
	}
	if got := readPinsRefreshFile(t, r.flags.pins); got != imageBefore {
		t.Errorf("image-pins changed:\n%s", got)
	}
	if got := readPinsRefreshFile(t, r.flags.seedPins); got != seedBefore {
		t.Errorf("seed-pins changed:\n%s", got)
	}
}

// TestDaemonImagePinsFlagsAreWired parses the pins subcommand's flags and checks
// each reaches the run function, and that the subcommand sits under image.
func TestDaemonImagePinsFlagsAreWired(t *testing.T) {
	t.Parallel()
	var got daemonImagePinsFlags
	cmd := newDaemonImagePinsCmd(func(_ io.Writer, f daemonImagePinsFlags) error {
		got = f
		return nil
	})
	cmd.SetArgs([]string{
		"--pins", "/p/image-pins", "--seed-pins", "/p/seed-pins",
		"--bd-src", "/s/bd", "--om-src", "/s/om", "--hm-src", "/s/hm", "--devops-src", "/s/devops",
		"--forge-owner", "acme", "--town-root", "/t", "--apply",
	})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := daemonImagePinsFlags{
		daemonImageFlags: daemonImageFlags{
			pins: "/p/image-pins", bdSrc: "/s/bd", omSrc: "/s/om", hmSrc: "/s/hm", devopsSrc: "/s/devops",
			forgeOwner: "acme", townRoot: "/t", apply: true,
		},
		seedPins: "/p/seed-pins",
	}
	if got != want {
		t.Errorf("flags = %+v, want %+v", got, want)
	}
	registered := false
	for _, sub := range daemonImageCmd.Commands() {
		if sub.Name() == "pins" {
			registered = true
		}
	}
	if !registered {
		t.Error("gt daemon image has no pins subcommand")
	}
	if daemonImagePinsCmd.Name() != "pins" || daemonimage.PinRefreshBehind != 20 {
		t.Errorf("pins command is %q with refresh bound %d", daemonImagePinsCmd.Name(), daemonimage.PinRefreshBehind)
	}
}

// TestDaemonImagePinsRefusesBadInputBeforeReading drives each refusal the flag
// checks make before any file is read: a relative pins path, a seed file that is
// not there, a checkout inside a canonical clone, and a forge owner the URLs
// cannot name. Each run writes neither file and names the input it refuses.
func TestDaemonImagePinsRefusesBadInputBeforeReading(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		breakIt func(t *testing.T, r *pinsRefreshRig)
		wantErr string
	}{
		{
			name: "a relative pins path",
			breakIt: func(t *testing.T, r *pinsRefreshRig) {
				r.flags.pins = filepath.Base(r.flags.pins)
			},
			wantErr: "is not absolute",
		},
		{
			name: "a seed-pins file that is not there",
			breakIt: func(t *testing.T, r *pinsRefreshRig) {
				r.flags.seedPins = filepath.Join(t.TempDir(), "absent-seed-pins")
			},
			wantErr: "cannot be read",
		},
		{
			name: "a seed-pins path that is a directory",
			breakIt: func(t *testing.T, r *pinsRefreshRig) {
				r.flags.seedPins = t.TempDir()
			},
			wantErr: "cannot be read",
		},
		{
			name: "a checkout that does not resolve",
			breakIt: func(t *testing.T, r *pinsRefreshRig) {
				r.resolve = func(string) (string, error) { return "", errors.New("too many links") }
			},
			wantErr: "cannot be resolved",
		},
		{
			name: "a checkout that is not there",
			breakIt: func(t *testing.T, r *pinsRefreshRig) {
				r.flags.omSrc = filepath.Join(t.TempDir(), "absent-om")
			},
			wantErr: "is not an existing directory",
		},
		{
			name: "a checkout named by a relative path",
			breakIt: func(t *testing.T, r *pinsRefreshRig) {
				r.flags.omSrc = "om"
			},
			wantErr: "is not absolute",
		},
		{
			name: "a checkout inside a canonical clone",
			breakIt: func(t *testing.T, r *pinsRefreshRig) {
				canonical := filepath.Join(t.TempDir(), "mayor", "rig")
				if err := os.MkdirAll(canonical, 0o755); err != nil {
					t.Fatal(err)
				}
				r.flags.devopsSrc = canonical
			},
			wantErr: "canonical clone",
		},
		{
			name: "a forge owner the URLs cannot name",
			breakIt: func(t *testing.T, r *pinsRefreshRig) {
				r.flags.forgeOwner = "no good/owner"
			},
			wantErr: "forge owner",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newPinsRefreshRig(t)
			imagePath, seedPath := r.flags.pins, r.flags.seedPins
			imageBefore := readPinsRefreshFile(t, imagePath)
			seedBefore := readPinsRefreshFile(t, seedPath)
			tc.breakIt(t, r)

			err := r.run(true)
			if err == nil {
				t.Fatal("refresh --apply returned no error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not name %q", err, tc.wantErr)
			}
			if got := readPinsRefreshFile(t, imagePath); got != imageBefore {
				t.Errorf("a refused refresh changed the image-pins file:\n%s", got)
			}
			if got := readPinsRefreshFile(t, seedPath); got != seedBefore {
				t.Errorf("a refused refresh changed the seed-pins file:\n%s", got)
			}
		})
	}
}
