package installer

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/imagebuild"
	"github.com/steveyegge/gastown/internal/polecatimage"
)

const (
	// ImageWorkerFlag is the first argument of a detached build's worker
	// process. It is not a documented install argument: installer.Parse routes
	// it, on the first argument alone and before any install flag is read, to
	// the worker's own parser. The installer starts the worker for
	// --no-image-first so the build can outlive the install without a shell to
	// detach from (gt-tpg0p.12).
	ImageWorkerFlag = "--image-build-worker"

	// imageProgramRel is the polecat-image program under the rig checkout: the
	// binary plugin.md declares and `make build` writes (gt-tpg0p.5-7). It is
	// the program, not the plugin's run.sh shim, which only execs it.
	imageProgramRel = "plugins/polecat-image/bin/polecat-image"

	// imageDefaultAlarm is how long a build may run when
	// INSTALL_GT_IMAGE_ALARM says nothing usable.
	imageDefaultAlarm = 1200

	// imageMaxAlarm is the largest whole number of seconds a time.Duration
	// holds: a bigger value would overflow to a negative alarm and leave the
	// build with no bound at all.
	imageMaxAlarm = int(int64(math.MaxInt64) / int64(time.Second))

	// imageBuildFailed is the escalation a detached build reports under. It has
	// its own fingerprint because an install that already swapped the binary
	// stands when that build fails, so rebuild-gt has no failure to report
	// (gt-vhn89).
	imageBuildFailed = "install-gt:image-build-failed"

	// imageFailClosedEnv makes the plugin fail its run on a deferral or on a
	// publish that did not complete (gt-efupr). Only the build before the swap
	// is told: an image the registry does not hold must not let the install
	// through, while a detached rebuild has nothing left to fail.
	imageFailClosedEnv = "POLECAT_IMAGE_FAIL_CLOSED"

	// imageSlingBound is how long a build before the swap waits for an in-flight
	// gt sling to finish before the install fails. The dispatcher slings most
	// minutes, so a build that fails on the first deferral fails most installs
	// (gt-rfwr4).
	imageSlingBound = 3 * time.Minute

	// imageSlingPoll is how often that wait reads whether a sling still runs.
	imageSlingPoll = 15 * time.Second
)

// wholeSeconds is an alarm that leaves the build a bound: a whole number of
// seconds above zero.
var wholeSeconds = regexp.MustCompile(`^[1-9][0-9]*$`)

// ImageBuild is one sandbox image build the step asks for: the program, the
// settings it is given on top of the run's own environment, and how long it
// may run.
type ImageBuild struct {
	// Program is the polecat-image program to run.
	Program string
	// Env is what the build is told about this install, as NAME=VALUE pairs
	// added to the environment it inherits. A setting the town's config.json
	// gives nothing for is absent here, so the plugin's inherited environment
	// still answers for it (gt-ijudv, gt-g3l7s).
	Env []string
	// Alarm is how long the build may run before it is stopped.
	Alarm time.Duration
	// Lock is the plugin's own lock directory; empty means it takes none.
	Lock string
	// GT and Fingerprint name the escalation a detached build reports under;
	// both empty for a build whose caller is still there to report it.
	GT          string
	Fingerprint string
	// Log is the file the build's output goes to.
	Log string
	// Detached asks for the build to be started in the background and not
	// waited for.
	Detached bool
}

// ImageRunner runs one image build and returns the status it ended with. A
// detached build returns as soon as it was started, so its status says only
// whether that worked. A nil Run.ImageRunner runs the build for real.
type ImageRunner func(ImageBuild) (int, error)

// ImageStep is the script's "The sandbox image, before the swap" section: the
// sandbox image is built and published for the target commit before
// install-local swaps the binary, so a sling is never refused for an image
// carrying another commit (gt-pmxo9, gt-xzo8a). A build that does not finish
// ends the run failed/build-failed with the old binary still in place.
//
// The caller runs it before Install, which is the swap (see runWith). The build
// takes only the plugin's own lock and never the install-wide one, so an install
// that takes that lock afterwards does not hold it for the length of a build
// (gt-tpg0p.13).
func (r *Run) ImageStep() Exit {
	return r.imageStep(false)
}

// ImageAfter is --no-image-first's old order, for an urgent daemon fix: once
// the binary is swapped and the daemon told to restart, the image is rebuilt
// detached and best effort. It never changes the install's outcome, and a
// failure is the build's own escalation (imageBuildFailed), since the install
// itself stands (gt-vhn89).
func (r *Run) ImageAfter() {
	r.imageStep(true)
}

// imageStep is both orders: detached=false builds before the swap and can end
// the run, detached=true starts the rebuild after it and cannot.
func (r *Run) imageStep(detached bool) Exit {
	order, consequence := "before", "the binary was not swapped"
	if detached {
		order, consequence = "after", "the binary was already swapped"
	}
	registry := r.imageDaemonEnv("POLECAT_IMAGE_REGISTRY")
	// A registry the daemon already exported is inherited, so the build
	// publishes even though the config named none (gt-ijudv): only a build with
	// no registry at all is local-only.
	if registry.Value == "" && r.getenv("POLECAT_IMAGE_REGISTRY") == "" {
		// A config that could not be read is not a town that configured no
		// registry: the build would go local-only and the image the town needs
		// would never be published. The step before the swap is fail-closed,
		// so it stops the install rather than pass an image the registry does
		// not hold (gt-tpg0p.12).
		if registry.Unreadable && !detached {
			r.C.Log("polecat image: the town's image registry could not be read (" + registry.Why +
				"), so the image cannot be published")
			return r.imageUnpublished("the sandbox image for " + r.C.Expected +
				" cannot be published: the town's image registry could not be read (" + registry.Why + ")")
		}
		if registry.Unreadable {
			r.C.Log("polecat image: the town's image registry could not be read (" + registry.Why +
				"), so the rebuild is local only and the registry keeps the old image")
		} else {
			r.C.Log("polecat image: build is local only and not published: " + registry.Why)
		}
	}
	crane := r.imageDaemonEnv("POLECAT_IMAGE_CRANE").Value
	program := r.imageProgram()
	if program == "" || !r.Files.isFile(program) {
		at := program
		if at == "" {
			at = "?"
		}
		r.C.Log("polecat image: no plugin at '" + at + "', so the image cannot be built " +
			order + " the swap; " + consequence)
		return r.imageNotBuilt(detached, "no plugin at '"+at+"'")
	}
	// Without the build output, the program would read the old commit from the
	// installed gt and build an image for the binary this install replaces.
	bin := r.imageBinary()
	if !r.Files.isExecutable(bin) {
		r.C.Log("polecat image: no build output at " + bin + ", so the image was not built")
		return r.imageNotBuilt(detached, "no build output at "+bin)
	}
	// A build before the swap is the one that fails the install, so it waits out
	// a sling in flight rather than failing on the first deferral (gt-rfwr4).
	// The detached build cannot fail the install and defers on its own.
	alarm := time.Duration(r.imageAlarm()) * time.Second
	var waited time.Duration
	if !detached {
		var ok bool
		if waited, ok = r.waitForSling(alarm); !ok {
			return r.imageFail()
		}
		// The wait comes out of the alarm, so the build before the swap runs
		// within it as a whole. Nothing left is a failure: a build with an alarm
		// of zero would have no bound at all (gt-jie3d).
		if waited >= alarm {
			r.C.Log("polecat image: the sling wait used the whole alarm of " + alarm.String() + "; the build is not run")
			return r.imageFail()
		}
	}
	logPath := r.imageLog()
	if logPath != os.DevNull {
		_ = r.Files.mkdirAll(filepath.Dir(logPath))
	}
	r.C.Log("polecat image: building " + r.C.Expected + " " + order + " the binary swap (log " + logPath + ")")
	b := ImageBuild{
		Program:  program,
		Env:      r.imageEnv(registry.Value, crane, bin, !detached),
		Alarm:    alarm - waited,
		Lock:     r.imageLock(),
		Log:      logPath,
		Detached: detached,
	}
	if detached {
		b.GT, b.Fingerprint = r.Paths.GT(), imageBuildFailed
	}
	code, err := r.imageRunner()(b)
	if detached {
		if err != nil {
			// A build that could not be started never reaches the town to
			// report itself, so the install raises its escalation instead.
			r.C.Log("polecat image: the rebuild for " + r.C.Expected + " could not be started: " + err.Error())
			r.C.Escalate("medium", imageBuildFailed, "the sandbox image rebuild for "+r.C.Expected+
				" could not be started; stale-image slings are refused until it is rebuilt")
			return ExitOK
		}
		// Only the start is known here: the worker runs on its own, so this
		// line names where a failure surfaces for a reader of the install's
		// output (gt-tpg0p.12).
		r.C.Log("polecat image: rebuild for " + r.C.Expected + " started in the background (log " +
			logPath + "); a failure is reported as " + imageBuildFailed)
		return ExitOK
	}
	if err == nil && code == 0 {
		r.C.Log("polecat image: in force at " + r.C.Expected)
		return ExitOK
	}
	if err != nil {
		r.C.Log("polecat image: the build for " + r.C.Expected + " did not finish: " + err.Error() + "; " + consequence)
	} else {
		r.C.Log("polecat image: the build for " + r.C.Expected + " did not finish (exit " +
			strconv.Itoa(code) + "); " + consequence)
	}
	r.tailImageLog(logPath)
	return r.imageFail()
}

// waitForSling reports whether the build before the swap may run now, and how
// long it waited. It reads the sling deferral first and returns at once when
// none is in flight, so a build that is not deferred never polls. Otherwise it
// reads again every imageSlingPoll until the deferral clears or the bound has
// passed. The bound is imageSlingBound, or the alarm when that is shorter, so
// the wait never runs past the alarm the build is given. Past the bound it logs
// why and reports false, and the caller fails the install with the image's own
// error. The reading is the same one the plugin defers on
// (polecatimage.SlingDeferReason), so the build does not start into a sling the
// plugin would refuse (gt-rfwr4).
func (r *Run) waitForSling(alarm time.Duration) (time.Duration, bool) {
	reason := r.slingDeferral()
	if reason == "" {
		return 0, true
	}
	bound := min(imageSlingBound, alarm)
	r.C.Log("polecat image: " + reason + "; waiting up to " + bound.String() +
		" for it to finish before the build")
	var waited time.Duration
	for waited < bound {
		poll := min(imageSlingPoll, bound-waited)
		r.imageSleep(poll)
		waited += poll
		if reason = r.slingDeferral(); reason == "" {
			r.C.Log("polecat image: no sling is running, so the build goes ahead")
			return waited, true
		}
	}
	r.C.Log("polecat image: " + reason + " after " + bound.String() + "; the build is not run")
	return waited, false
}

// slingDeferral is the reason a gt sling defers the build, "" when none runs.
// Without the seam it asks pgrep, and a pgrep that cannot answer defers too.
func (r *Run) slingDeferral() string {
	if r.SlingDeferral != nil {
		return r.SlingDeferral()
	}
	_, _, code := r.Cmd.run("", "pgrep", "-f", polecatimage.SlingPattern)
	return polecatimage.SlingDeferReason(code, "pgrep")
}

// imageSleep waits d on the clock the run was given, or the real one.
func (r *Run) imageSleep(d time.Duration) {
	if r.ImageSleep != nil {
		r.ImageSleep(d)
		return
	}
	time.Sleep(d)
}

// imageNotBuilt is the answer to a build whose preflight could not pass (why
// says which one): the build before the swap fails the install closed, and the
// detached one leaves the install standing but still escalates under the build's
// own fingerprint. Nothing else reports a rebuild that never ran, so a silent
// return would leave a stale image with no notice beyond the log (gt-tpg0p.12).
func (r *Run) imageNotBuilt(detached bool, why string) Exit {
	if !detached {
		return r.imageFail()
	}
	r.C.Escalate("medium", imageBuildFailed, "the sandbox image rebuild for "+r.C.Expected+
		" could not be started ("+why+"); stale-image slings are refused until it is rebuilt")
	return ExitOK
}

// imageFail is fail_install for the image: the old binary is still in place,
// so there is nothing to roll back, and the daemon was not told to restart.
func (r *Run) imageFail() Exit {
	return r.imageUnpublished("the sandbox image for " + r.C.Expected + " was not built and published")
}

// imageUnpublished fails an install that would leave the town without an image
// it can be served from, before the swap: fail_install's build-failed, with the
// old binary in place and the daemon not told to restart (gt-tpg0p.12).
func (r *Run) imageUnpublished(what string) Exit {
	prev := r.C.Prev
	if prev == "" {
		prev = "the previous build"
	}
	return r.FailInstall("build-failed",
		what+"; "+r.Paths.GT()+" was left at "+prev+" and the daemon was not told to restart",
		"medium", "install-gt:build-failed", "failed")
}

// tailImageLog repeats the build log's last lines in the install's own output,
// as the script's tail does: the log is where a failed build's reason is.
func (r *Run) tailImageLog(path string) {
	if path == "" || path == os.DevNull {
		return
	}
	data, err := r.Files.readFile(path)
	if err != nil {
		return
	}
	body := strings.TrimRight(string(data), "\n")
	if body == "" {
		return
	}
	lines := strings.Split(body, "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	for _, line := range lines {
		r.C.Log("polecat image: " + line)
	}
}

// imageProgram is the program the build runs: INSTALL_GT_IMAGE_RUN when the
// caller names one, else the polecat-image program the rig checkout's `make
// build` writes (gt-tpg0p.5-7). The plugin runner executes that binary
// directly, so the installer does too (gt-tpg0p.1).
func (r *Run) imageProgram() string {
	if p := r.getenv("INSTALL_GT_IMAGE_RUN"); p != "" {
		return p
	}
	return filepath.Join(r.Paths.RigDir, imageProgramRel)
}

// imageBinary is the build output whose commit the image is built at, never the
// installed gt: before the swap the installed binary still reports the old
// commit (gt-xzo8a). INSTALL_GT_IMAGE_BINARY overrides it.
func (r *Run) imageBinary() string {
	if b := r.getenv("INSTALL_GT_IMAGE_BINARY"); b != "" {
		return b
	}
	return filepath.Join(r.Paths.RigDir, "gt")
}

// imageLog is where the build writes its output, else the town's runtime
// directory; a run with no town has nowhere to put it and gives /dev/null.
func (r *Run) imageLog() string {
	if l := r.getenv("INSTALL_GT_IMAGE_LOG"); l != "" {
		return l
	}
	if r.Paths.TownRoot == "" {
		return os.DevNull
	}
	return filepath.Join(r.Paths.TownRoot, ".runtime", "polecat-image-install.log")
}

// imageLock is the plugin's own lock, which this build takes; the install lock
// is taken after it, never by it, so a queued install waits for that wait and
// not the build (gt-tpg0p.13).
func (r *Run) imageLock() string {
	if l := r.getenv("INSTALL_GT_IMAGE_LOCK"); l != "" {
		return l
	}
	if r.Paths.TownRoot == "" {
		return ""
	}
	return filepath.Join(r.Paths.TownRoot, ".runtime", "polecat-image.lock")
}

// imageAlarm is INSTALL_GT_IMAGE_ALARM when it is a whole number of seconds
// above zero that a time.Duration holds, else the default, with the value
// named: an alarm of 0, a non-number or a value too large for a duration would
// leave the build with no bound at all.
func (r *Run) imageAlarm() int {
	raw := r.getenv("INSTALL_GT_IMAGE_ALARM")
	if raw == "" {
		return imageDefaultAlarm
	}
	alarm, err := strconv.Atoi(raw)
	if err != nil || !wholeSeconds.MatchString(raw) || alarm <= 0 || alarm > imageMaxAlarm {
		r.C.Log("polecat image: INSTALL_GT_IMAGE_ALARM='" + raw +
			"' is not a usable number of seconds, so the build gets " +
			strconv.Itoa(imageDefaultAlarm))
		return imageDefaultAlarm
	}
	return alarm
}

// imageEnv is what the build is told about this install: the town root it reads
// its settings from, the build output whose commit it builds, and its own lock
// (gt-87hlc). The registry and crane are added only when the town's config.json
// gives them, and the town root and lock only when this run has them, so a value
// this run gives nothing for still comes from the environment the build
// inherits rather than being blanked.
func (r *Run) imageEnv(registry, crane, bin string, failClosed bool) []string {
	// The build output names the commit and is always set; every other setting is
	// left out when this run has none, so it overwrites no inherited value.
	env := []string{"POLECAT_IMAGE_GT=" + bin}
	for _, pair := range []string{
		"POLECAT_IMAGE_TOWN_ROOT=" + r.Paths.TownRoot,
		"POLECAT_IMAGE_LOCK=" + r.imageLock(),
		"POLECAT_IMAGE_REGISTRY=" + registry,
		"POLECAT_IMAGE_CRANE=" + crane,
	} {
		if _, value, _ := strings.Cut(pair, "="); value != "" {
			env = append(env, pair)
		}
	}
	if failClosed {
		env = append(env, imageFailClosedEnv+"=1")
	}
	return env
}

// daemonEnv is one daemon.env setting the town gives: its value, why it gives
// none, and whether the town could be read at all.
type daemonEnv struct {
	Value string
	Why   string
	// Unreadable says the town could not be read, so an empty Value means "not
	// known" rather than "not configured". The two must not be confused: a
	// town that configured no registry is deliberately local-only, while one
	// whose settings cannot be read may need a registry this run cannot name
	// (gt-tpg0p.12).
	Unreadable bool
}

// imageDaemonEnv is the script's image_daemon_env: the value the town's
// settings/config.json gives daemon.env[key], the one source the daemon gives
// its plugin runs (gt-jmd9n), or the reason it gives none. Nothing here is
// fatal on its own: a build with no registry runs local-only, as it always
// has, and the caller decides what an unreadable config means.
func (r *Run) imageDaemonEnv(key string) daemonEnv {
	if r.Paths.TownRoot == "" {
		return daemonEnv{Why: "no town root"}
	}
	cfg := filepath.Join(r.Paths.TownRoot, "settings", "config.json")
	data, err := r.Files.readFile(cfg)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return daemonEnv{Why: cfg + " is missing"}
		}
		return daemonEnv{Why: cfg + " could not be read", Unreadable: true}
	}
	var doc struct {
		Daemon json.RawMessage `json:"daemon"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return daemonEnv{Why: cfg + " is not readable JSON", Unreadable: true}
	}
	var daemon map[string]json.RawMessage
	if len(doc.Daemon) > 0 && string(doc.Daemon) != "null" {
		if err := json.Unmarshal(doc.Daemon, &daemon); err != nil {
			return daemonEnv{Why: cfg + " is not readable JSON", Unreadable: true}
		}
	}
	block, ok := daemon["env"]
	if !ok || string(block) == "null" {
		return daemonEnv{Why: cfg + " has no daemon.env"}
	}
	entries := map[string]json.RawMessage{}
	if err := json.Unmarshal(block, &entries); err != nil {
		return daemonEnv{Why: "daemon.env in " + cfg + " is a " + jsonKind(block) + ", not an object", Unreadable: true}
	}
	entry, ok := entries[key]
	if !ok {
		return daemonEnv{Why: "no " + key + " in daemon.env of " + cfg}
	}
	var entryValue string
	if err := json.Unmarshal(entry, &entryValue); err != nil {
		return daemonEnv{Why: key + " in daemon.env of " + cfg + " is not a string", Unreadable: true}
	}
	if entryValue == "" {
		return daemonEnv{Why: "no " + key + " in daemon.env of " + cfg}
	}
	return daemonEnv{Value: entryValue}
}

// jsonKind names a JSON value the way jq's type does, for the message that says
// daemon.env is not an object.
func jsonKind(raw []byte) string {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case strings.HasPrefix(trimmed, `"`):
		return "string"
	case strings.HasPrefix(trimmed, "["):
		return "array"
	case strings.HasPrefix(trimmed, "{"):
		return "object"
	case trimmed == "true" || trimmed == "false":
		return "boolean"
	case trimmed == "null" || trimmed == "":
		return "null"
	default:
		return "number"
	}
}

// imageRunner is the seam a test records builds through, or the real one.
func (r *Run) imageRunner() ImageRunner {
	if r.ImageRunner != nil {
		return r.ImageRunner
	}
	return r.runImageBuild
}

// runImageBuild is the real ImageRunner. The build before the swap runs under
// imagebuild's alarm and process group in this process, which is where the
// script's supervisor helper put it. The detached one is a worker process of
// its own: this process exits with the install, and a build that inherited its
// descriptors would hold the install lock past it.
func (r *Run) runImageBuild(b ImageBuild) (int, error) {
	if b.Detached {
		return 0, spawnImageWorker(b)
	}
	logFile, err := os.OpenFile(b.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 1, err
	}
	defer func() { _ = logFile.Close() }()
	return r.imageSupervisor()(imagebuild.Config{
		Alarm:       b.Alarm,
		Lock:        b.Lock,
		GT:          b.GT,
		Fingerprint: b.Fingerprint,
		Command:     []string{b.Program},
		Env:         withEnv(os.Environ(), b.Env...),
		Stdout:      logFile,
		Stderr:      logFile,
	}), nil
}

// imageSupervisor is the seam a test records a supervised build through, or the
// real imagebuild.Run: the build before the swap runs in this process, under
// the alarm and process group the supervisor gives it.
func (r *Run) imageSupervisor() func(imagebuild.Config) int {
	if r.ImageSupervisor != nil {
		return r.ImageSupervisor
	}
	return imagebuild.Run
}

// spawnImageWorker starts the detached build as this program under
// ImageWorkerFlag, in a session of its own and writing the build log: the
// worker is what outlives the install, and it holds none of the install's
// descriptors because it is a new process.
func spawnImageWorker(b ImageBuild) error {
	return startImageWorker(b, workerExec{
		executable: os.Executable,
		start:      func(cmd *exec.Cmd) error { return cmd.Start() },
		release:    func(p *os.Process) error { return p.Release() },
	})
}

// workerExec is how a worker process is started; startImageWorker's test drives
// each failure with its own answer.
type workerExec struct {
	executable func() (string, error)
	start      func(cmd *exec.Cmd) error
	release    func(p *os.Process) error
}

// startImageWorker opens the build log (the worker's streams), starts the
// worker in a session of its own, and lets go of it so this process can exit
// without it.
func startImageWorker(b ImageBuild, x workerExec) error {
	exe, err := x.executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(b.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	cmd := exec.Command(exe, workerArgs(b)...)
	cmd.Env = withEnv(os.Environ(), b.Env...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := x.start(cmd); err != nil {
		return err
	}
	return x.release(cmd.Process)
}

// workerArgs is the argv a detached build's worker is started with, matching
// ImageWorker's own parser.
func workerArgs(b ImageBuild) []string {
	args := []string{ImageWorkerFlag, "--alarm", strconv.Itoa(int(b.Alarm / time.Second))}
	if b.Lock != "" {
		args = append(args, "--lock", b.Lock)
	}
	if b.GT != "" && b.Fingerprint != "" {
		args = append(args, "--gt", b.GT, "--fingerprint", b.Fingerprint)
	}
	return append(args, "--", b.Program)
}

// withEnv returns env with the NAME=VALUE pairs applied in order: a name the
// environment already has is replaced where it stands, not duplicated.
func withEnv(env []string, pairs ...string) []string {
	out := append([]string(nil), env...)
	for _, pair := range pairs {
		name, _, _ := strings.Cut(pair, "=")
		replaced := false
		for i, have := range out {
			if key, _, ok := strings.Cut(have, "="); ok && key == name {
				out[i] = pair
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, pair)
		}
	}
	return out
}
