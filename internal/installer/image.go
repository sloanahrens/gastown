package installer

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/imagebuild"
)

const (
	// ImageWorkerFlag is the first argument of a detached build's worker
	// process. It is not a documented install argument: the installer starts
	// the worker for --no-image-first so the build can outlive the install
	// without a shell to detach from (gt-tpg0p.12).
	ImageWorkerFlag = "--image-build-worker"

	// imageProgramRel is the polecat-image program under the rig checkout: the
	// binary plugin.md declares and `make build` writes (gt-tpg0p.5-7). It is
	// the program, not the plugin's run.sh shim, which only execs it.
	imageProgramRel = "plugins/polecat-image/bin/polecat-image"

	// imageDefaultAlarm is how long a build may run when
	// INSTALL_GT_IMAGE_ALARM says nothing usable.
	imageDefaultAlarm = 1200

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
	registry, why := r.imageDaemonEnv("POLECAT_IMAGE_REGISTRY")
	// A registry the daemon already exported is inherited, so the build
	// publishes even though the config named none (gt-ijudv): only a build with
	// no registry at all is local-only.
	if registry == "" && r.getenv("POLECAT_IMAGE_REGISTRY") == "" {
		r.C.Log("polecat image: build is local only and not published: " + why)
	}
	crane, _ := r.imageDaemonEnv("POLECAT_IMAGE_CRANE")
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
	alarm := r.imageAlarm()
	logPath := r.imageLog()
	if logPath != os.DevNull {
		_ = r.Files.mkdirAll(filepath.Dir(logPath))
	}
	r.C.Log("polecat image: building " + r.C.Expected + " " + order + " the binary swap (log " + logPath + ")")
	b := ImageBuild{
		Program:  program,
		Env:      r.imageEnv(registry, crane, bin, !detached),
		Alarm:    time.Duration(alarm) * time.Second,
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
		r.C.Log("polecat image: rebuild for " + r.C.Expected + " started in the background (log " + logPath + ")")
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
	prev := r.C.Prev
	if prev == "" {
		prev = "the previous build"
	}
	return r.FailInstall("build-failed",
		"the sandbox image for "+r.C.Expected+" was not built and published; "+r.Paths.GT()+
			" was left at "+prev+" and the daemon was not told to restart",
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
// above zero, else the default, with the value named: an alarm of 0 or a
// non-number would leave the build with no bound at all.
func (r *Run) imageAlarm() int {
	raw := r.getenv("INSTALL_GT_IMAGE_ALARM")
	if raw == "" {
		return imageDefaultAlarm
	}
	alarm, err := strconv.Atoi(raw)
	if err != nil || !wholeSeconds.MatchString(raw) || alarm <= 0 {
		r.C.Log("polecat image: INSTALL_GT_IMAGE_ALARM='" + raw +
			"' is not a whole number of seconds above zero, so the build gets " +
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

// imageDaemonEnv is the script's image_daemon_env: the value the town's
// settings/config.json gives daemon.env[key], the one source the daemon gives
// its plugin runs (gt-jmd9n), or "" and the reason it has none. Nothing here is
// fatal: a build with no registry runs local-only, as it always has.
func (r *Run) imageDaemonEnv(key string) (value, why string) {
	if r.Paths.TownRoot == "" {
		return "", "no town root"
	}
	cfg := filepath.Join(r.Paths.TownRoot, "settings", "config.json")
	data, err := r.Files.readFile(cfg)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", cfg + " is missing"
		}
		return "", cfg + " could not be read"
	}
	var doc struct {
		Daemon json.RawMessage `json:"daemon"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", cfg + " is not readable JSON"
	}
	var daemon map[string]json.RawMessage
	if len(doc.Daemon) > 0 && string(doc.Daemon) != "null" {
		if err := json.Unmarshal(doc.Daemon, &daemon); err != nil {
			return "", cfg + " is not readable JSON"
		}
	}
	block, ok := daemon["env"]
	if !ok || string(block) == "null" {
		return "", cfg + " has no daemon.env"
	}
	entries := map[string]json.RawMessage{}
	if err := json.Unmarshal(block, &entries); err != nil {
		return "", "daemon.env in " + cfg + " is a " + jsonKind(block) + ", not an object"
	}
	entry, ok := entries[key]
	if !ok {
		return "", "no " + key + " in daemon.env of " + cfg
	}
	var entryValue string
	if err := json.Unmarshal(entry, &entryValue); err != nil {
		return "", key + " in daemon.env of " + cfg + " is not a string"
	}
	if entryValue == "" {
		return "", "no " + key + " in daemon.env of " + cfg
	}
	return entryValue, ""
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
	return imagebuild.Run(imagebuild.Config{
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
