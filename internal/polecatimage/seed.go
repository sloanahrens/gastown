package polecatimage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// This file is the seed Job's two containers: the init container that checks the pinned commits
// out, and the seed container that runs cachevol.PlanSeed's steps into the shared claim and hands
// it to the cache user (gt-dv04y). They run inside the polecat image, so their logic is Go in the
// image's own binary.

// defaultSeedRoot and defaultSeedSource are the container paths the seed Job mounts the claim and
// the checkouts at, and the flags' defaults.
const (
	defaultSeedRoot   = cachevol.SeedCacheRoot
	defaultSeedSource = cachevol.SeedSourceRoot
)

// SeedFiles is the filesystem the Job's containers use: the marker both of them compare, and the
// directories the seed pass creates. The containers are the only callers of the os implementation.
type SeedFiles interface {
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, data []byte, perm os.FileMode) error
	MkdirAll(name string, perm os.FileMode) error
}

// OSFiles is SeedFiles on the container's filesystem.
type OSFiles struct{}

func (OSFiles) ReadFile(p string) ([]byte, error)                 { return os.ReadFile(p) }
func (OSFiles) WriteFile(p string, b []byte, m os.FileMode) error { return os.WriteFile(p, b, m) }
func (OSFiles) MkdirAll(p string, m os.FileMode) error            { return os.MkdirAll(p, m) }

// SeedCmd is one child process of the seed pass.
type SeedCmd struct {
	// Dir is the directory the child runs in; "" is the caller's.
	Dir string
	// Argv is the command and its arguments.
	Argv []string
	// UID and GID are the user the child runs as; a negative UID runs it as the seed Job's own
	// user, which is the root the Job's pod security context gives it.
	UID int
	GID int
}

// SeedRunner runs one child of the seed pass and returns its combined output. A child that could
// not start reports what the runner could not do; a child that ran and failed reports its exit
// status, and its output carries the reason.
type SeedRunner func(SeedCmd) (string, error)

// SeedPass is one run of the seed container: the claim to fill, the checkouts the init container
// made, and the pins to seed from.
type SeedPass struct {
	// Root is the claim's mount, cachevol.SeedCacheRoot in the Job.
	Root string
	// Source is the checkouts' mount, cachevol.SeedSourceRoot in the Job.
	Source string
	// Pins are the rigs and commits the checkouts hold, in seed order.
	Pins  []cachevol.Pin
	Files SeedFiles
	Run   SeedRunner
	Logf  func(format string, args ...any)
}

// Seed fills the claim: it hands the directories and checkouts the builds write to the cache user,
// runs each rig's PlanSeed steps as that user, then chowns and chmods the whole claim and writes
// the marker.
//
// The marker is written last on purpose: a marker in the claim says every step and the handover
// succeeded, so a rerun with the same pins does nothing and a failed run is retried whole.
func (p *SeedPass) Seed() error {
	if len(p.Pins) == 0 {
		return errors.New("the seed pass names no pin, so it would seed nothing")
	}
	plan, err := cachevol.PlanSeed(p.Pins)
	if err != nil {
		return err
	}
	markerPath := path.Join(p.Root, cachevol.SeedMarkerName)
	want := cachevol.SeedMarker(p.Pins)
	seeded, err := markerHoldsPins(p.Files, p.Root, p.Pins)
	if err != nil {
		return err
	}
	if seeded {
		p.Logf("seed: %s already holds the %d pinned commit(s); nothing to do", markerPath, len(p.Pins))
		return nil
	}

	buildCache := path.Join(p.Root, cachevol.SeedBuildCacheDir)
	moduleCache := path.Join(p.Root, cachevol.SeedModuleCacheDir)
	for _, dir := range []string{p.Root, buildCache, moduleCache} {
		if err := p.Files.MkdirAll(dir, 0o775); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	owner := strconv.Itoa(cachevol.SeedUserID) + ":" + strconv.Itoa(cachevol.SeedUserID)
	// The builds run as the cache user, so everything they write first must be its own: the two
	// directories this pass just made as root, and the checkouts the clone init container made as
	// root. go build may write a main package's binary beside its sources, so the checkouts need it.
	if err := p.asRoot("chown", "-h", owner, buildCache, moduleCache); err != nil {
		return err
	}
	if err := p.asRoot("chown", "-R", "-h", owner, p.Source); err != nil {
		return err
	}

	for _, rig := range plan {
		dir := path.Join(p.Source, rig.Rig)
		for _, step := range rig.Steps {
			line := strings.Join(step.Argv, " ")
			out, err := p.Run(SeedCmd{Dir: dir, Argv: step.Argv, UID: cachevol.SeedUserID, GID: cachevol.SeedUserID})
			switch {
			case err == nil:
				p.Logf("seed: %s: %s", dir, line)
			case step.Required:
				return fmt.Errorf("the seed step %q in %s failed: %w: %s", line, dir, err, reason(out))
			default:
				// The Dockerfile seed RUN skips a step that needs something the image lacks, and
				// logs it; a required step failing is the only one that stops the seed.
				p.Logf("seed: %s: %s failed, skipped: %s", dir, line, reason(out))
			}
		}
	}

	// A sandbox and the trim Job run as the cache user, so the whole claim is handed over before
	// the marker says it is ready.
	if err := p.asRoot("chown", "-R", "-h", owner, p.Root); err != nil {
		return err
	}
	if err := p.asRoot("chmod", "-R", "a+rwX", p.Root); err != nil {
		return err
	}
	if err := p.Files.WriteFile(markerPath, []byte(want), 0o644); err != nil {
		return fmt.Errorf("writing the seed marker %s: %w", markerPath, err)
	}
	p.Logf("seed: %d rig(s) seeded into %s", len(plan), p.Root)
	return nil
}

// asRoot runs argv as the seed Job's own user: the chowns and the chmod are the root work the Job
// exists for, and they run over paths the pass built, never over a pin's content.
func (p *SeedPass) asRoot(argv ...string) error {
	out, err := p.Run(SeedCmd{Argv: argv, UID: -1, GID: -1})
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, reason(out))
	}
	return nil
}

// markerHoldsPins reports whether the claim at root already records exactly these pins. Both
// containers compare through it: the init container skips its clone and the seed pass skips its
// seed on the same evidence, so the two cannot disagree about whether a claim is seeded.
//
// An unreadable marker is an error, never a "no": a claim whose marker cannot be read has to fail
// the Job instead of being cloned over or seeded again on a guess.
func markerHoldsPins(files SeedFiles, root string, pins []cachevol.Pin) (bool, error) {
	markerPath := path.Join(root, cachevol.SeedMarkerName)
	held, err := files.ReadFile(markerPath)
	switch {
	case err == nil:
		return string(held) == cachevol.SeedMarker(pins), nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("reading the seed marker %s: %w", markerPath, err)
	}
}

// CloneSeed checks each pin out under dest, one directory per rig, from the pin's clone URL. It
// runs in the seed Job's init container, so a pin the URL cannot serve fails the Job before the
// seed container touches the claim.
//
// claim is the mounted cache claim, whose marker says what the claim was seeded from: when it
// already holds these pins, a rerun clones nothing, because the seed pass will do nothing either
// (gt-posdj). An absent or different marker still clones every pin.
func CloneSeed(dest, claim string, pins []cachevol.Pin, files SeedFiles, run SeedRunner, logf func(string, ...any)) error {
	if len(pins) == 0 {
		return errors.New("the seed clone names no pin, so it would check nothing out")
	}
	// PlanSeed's rules decide what a rig name may be, and the rig is a directory name here.
	if _, err := cachevol.PlanSeed(pins); err != nil {
		return err
	}
	if err := cachevol.CheckCloneURLs(pins); err != nil {
		return err
	}
	if claim == "" {
		return errors.New("the seed clone names no claim, so it could not tell whether the pins are already checked out")
	}
	seeded, err := markerHoldsPins(files, claim, pins)
	if err != nil {
		return err
	}
	if seeded {
		logf("seed: %s already holds the %d pinned commit(s); nothing to clone", path.Join(claim, cachevol.SeedMarkerName), len(pins))
		return nil
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dest, err)
	}
	for _, pin := range pins {
		dir := path.Join(dest, pin.Rig)
		logf("seed: cloning %s at %s", pin.Rig, pin.Commit)
		// --no-checkout then an explicit checkout, because the pin is a commit: a clone of the
		// default branch has the objects but not that name.
		for _, argv := range [][]string{
			{"git", "clone", "--no-checkout", "--quiet", pin.URL, dir},
			{"git", "-C", dir, "checkout", "--quiet", "--detach", pin.Commit},
		} {
			out, err := run(SeedCmd{Argv: argv, UID: -1, GID: -1})
			if err != nil {
				return fmt.Errorf("the seed clone of %s at %s: %w: %s", pin.Rig, pin.Commit, err, scrubURL(reason(out), pin.URL))
			}
		}
	}
	return nil
}

// reason is a failed child's last output line, or that it printed none: a failure whose output is
// empty must not end in a bare colon.
func reason(out string) string {
	if line := lastLine(out); line != "" {
		return line
	}
	return "no output"
}

// scrubURL removes a clone URL from a failure's output: git quotes the URL it fetched, and a URL
// must reach no log.
func scrubURL(out, raw string) string {
	if raw == "" {
		return out
	}
	return strings.ReplaceAll(out, raw, "<clone url>")
}

// execSeedRun answers SeedRunner on this container: the child inherits the container's environment,
// which carries GOCACHE and GOMODCACHE, and runs as the uid SeedCmd names.
func execSeedRun(c SeedCmd) (string, error) {
	return runSeedChild(exec.Command, c)
}

// runSeedChild is execSeedRun with the spawn handed in, so a test fails a child without starting
// one: the seed pass's tests are unit tests, and they run no process.
func runSeedChild(spawn func(name string, arg ...string) *exec.Cmd, c SeedCmd) (string, error) {
	if len(c.Argv) == 0 {
		return "", errors.New("the seed pass ran no command")
	}
	cmd := spawn(c.Argv[0], c.Argv[1:]...)
	cmd.Dir = c.Dir
	if c.UID >= 0 {
		seedAs(cmd, c.UID, c.GID)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// decodePins reads the pins argv element the seed Job passes both containers.
func decodePins(raw string) ([]cachevol.Pin, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("no --pins: the seed Job passes the pins it renders")
	}
	var pins []cachevol.Pin
	if err := json.Unmarshal([]byte(raw), &pins); err != nil {
		return nil, fmt.Errorf("--pins is not the seed Job's pin list: %w", err)
	}
	return pins, nil
}
