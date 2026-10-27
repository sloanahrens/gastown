package polecatimage

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// envMap is a get function over a fixed environment, so a test reads no
// variable its own shell set.
func envMap(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

// TestConfigFromEnvDefaults pins the town a run reads when the operator names
// nothing: HOME's gt, and the paths under it.
func TestConfigFromEnvDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := ConfigFromEnv(envMap(map[string]string{"HOME": "/home/op"}))
	if err != nil {
		t.Fatalf("ConfigFromEnv() failed: %v", err)
	}
	town := "/home/op/gt"
	for _, tc := range []struct {
		got, want, name string
	}{
		{cfg.RigRepo, filepath.Join(town, "gastown", ".repo.git"), "rig repo"},
		{cfg.BuildRoot, filepath.Join(town, "gastown", ".polecat-image-build"), "build root"},
		{cfg.BDSrc, filepath.Join(town, "beads", "mayor", "rig"), "bd source"},
		{cfg.LockDir, filepath.Join(town, ".runtime", "polecat-image.lock"), "lock"},
		{cfg.SandboxRecords, filepath.Join(town, ".runtime", "sandboxes"), "sandbox records"},
		{cfg.Published, filepath.Join(town, ".runtime", "polecat-image.published"), "publish record"},
		{cfg.Image, "gastown-polecat:dev", "image"},
		{cfg.Keep, "3", "keep"},
		{cfg.BuilderKeep, "8GB", "builder keep"},
		{cfg.CacheCap, "10GB", "cache cap"},
		{cfg.Docker, "docker", "docker"},
		{cfg.GT, "gt", "gt"},
	} {
		if tc.got != tc.want {
			t.Errorf("the %s is %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	if cfg.Registry != "" || cfg.FailClosed {
		t.Errorf("a bare environment publishes to %q and fails closed %v, want neither", cfg.Registry, cfg.FailClosed)
	}
	if cfg.StaleAfter != 20*time.Minute {
		t.Errorf("the stale lock after %v, want 20 minutes", cfg.StaleAfter)
	}
}

// TestConfigFromEnvOverrides pins the seams every path and program can be
// overridden through.
func TestConfigFromEnvOverrides(t *testing.T) {
	t.Parallel()
	cfg, err := ConfigFromEnv(envMap(map[string]string{
		"HOME":                        "/home/op",
		"POLECAT_IMAGE_TOWN_ROOT":     "/town",
		"POLECAT_IMAGE_NAME":          "example:dev",
		"POLECAT_IMAGE_REGISTRY":      "localhost:5001",
		"POLECAT_IMAGE_FAIL_CLOSED":   "1",
		"POLECAT_IMAGE_STALE_MINUTES": "5",
		"POLECAT_IMAGE_KEEP":          "7",
		"POLECAT_IMAGE_BUILDER_KEEP":  "2GB",
		"POLECAT_CACHE_CAP":           "512M",
		"POLECAT_IMAGE_DOCKER":        "docker-dev",
	}))
	if err != nil {
		t.Fatalf("ConfigFromEnv() failed: %v", err)
	}
	for _, tc := range []struct {
		got, want, name string
	}{
		{cfg.LockDir, filepath.Join("/town", ".runtime", "polecat-image.lock"), "lock"},
		{cfg.Image, "example:dev", "image"},
		{cfg.Registry, "localhost:5001", "registry"},
		{cfg.Keep, "7", "keep"},
		{cfg.BuilderKeep, "2GB", "builder keep"},
		{cfg.CacheCap, "512M", "cache cap"},
		{cfg.Docker, "docker-dev", "docker"},
	} {
		if tc.got != tc.want {
			t.Errorf("the %s is %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	if !cfg.FailClosed {
		t.Error("POLECAT_IMAGE_FAIL_CLOSED=1 did not make the run fail closed")
	}
	if cfg.StaleAfter != 5*time.Minute {
		t.Errorf("the stale lock after %v, want 5 minutes", cfg.StaleAfter)
	}
}

// TestConfigFromEnvNeedsATownRoot is the one unreadable path: a run that guessed
// a town would build from another one's repo.
func TestConfigFromEnvNeedsATownRoot(t *testing.T) {
	t.Parallel()
	_, err := ConfigFromEnv(envMap(map[string]string{}))
	if err == nil || !strings.Contains(err.Error(), "POLECAT_IMAGE_TOWN_ROOT and HOME are both unset") {
		t.Fatalf("ConfigFromEnv() = %v, want the missing town root", err)
	}
}

// TestConfigFromEnvRefusesAnUnreadableStaleLock pins the variable the run path
// cannot carry on without: it drives the lock takeover.
func TestConfigFromEnvRefusesAnUnreadableStaleLock(t *testing.T) {
	t.Parallel()
	_, err := ConfigFromEnv(envMap(map[string]string{"POLECAT_IMAGE_TOWN_ROOT": "/town", "POLECAT_IMAGE_STALE_MINUTES": "twenty"}))
	if err == nil || !strings.Contains(err.Error(), "POLECAT_IMAGE_STALE_MINUTES=twenty is not a whole number of minutes") {
		t.Fatalf("ConfigFromEnv() = %v, want the unreadable stale lock", err)
	}
}

// TestConfigFromEnvCarriesAnUnreadableKeepThrough is the prune-only knob's blast
// radius: a count that is not a number reaches the prune, which stops alone,
// instead of failing the build and publish path (gt-tpg0p.7).
func TestConfigFromEnvCarriesAnUnreadableKeepThrough(t *testing.T) {
	t.Parallel()
	cfg, err := ConfigFromEnv(envMap(map[string]string{"POLECAT_IMAGE_TOWN_ROOT": "/town", "POLECAT_IMAGE_KEEP": "three"}))
	if err != nil {
		t.Fatalf("ConfigFromEnv() failed: %v", err)
	}
	if cfg.Keep != "three" {
		t.Errorf("the keep count is %q, want the text the operator wrote", cfg.Keep)
	}
}

// programAnswer answers a whole Program run: runReply's answers for the run
// itself, plus the calls the trims make.
func programAnswer(t *testing.T, tr *testRun, reply *runReply) func(command) result {
	t.Helper()
	base := reply.answer(t, labelFormat(tr))
	return func(c command) result {
		if c.name == "docker" && len(c.args) > 0 {
			switch c.args[0] {
			case "images":
				return result{stdout: "dev\n"}
			case "builder":
				return result{stdout: "Total reclaimed space: 0B\n"}
			case "volume":
				return result{}
			}
		}
		return base(c)
	}
}

// TestProgramTrimsAfterAFinishedRun pins the trims a finished run ends with: the
// tags, the build cache and the cache volumes, after it reported the image
// current (gt-tpg0p.7).
func TestProgramTrimsAfterAFinishedRun(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.Registry = ""
	reply := newRunReply("abc1234")
	reply.label = "abc1234"
	tr.reply = programAnswer(t, tr, reply)
	t.Cleanup(tr.Cleanup)
	if err := tr.Program(); err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	tr.wantLogged(t, "current at abc1234")
	tr.wantCall(t, "docker", "images", "--format", imagesFormat, "gastown-polecat")
	tr.wantCall(t, "docker", "builder", "prune", "--keep-storage", "8GB", "-f")
	tr.wantCall(t, "docker", "volume", "ls", "--format", "{{.Name}}", "--filter", "name=^gt-cache-")
}

// TestProgramTrimsAfterADeferral pins the other completed run: a deferral is
// not a failure, so the trims still run.
func TestProgramTrimsAfterADeferral(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.Registry = ""
	reply := newRunReply("abc1234")
	reply.label = "9999999"
	reply.pgrep = result{exit: 0}
	tr.reply = programAnswer(t, tr, reply)
	t.Cleanup(tr.Cleanup)
	if err := tr.Program(); err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	tr.wantLogged(t, "deferred: sandbox sling in progress")
	tr.wantCall(t, "docker", "builder", "prune", "--keep-storage", "8GB", "-f")
}

// TestProgramSkipsTheTrimsAfterAFailure pins the rule that separates a failure
// from a deferral: a run that failed trimmed nothing, so its failure is not
// buried under trim logs.
func TestProgramSkipsTheTrimsAfterAFailure(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = newRunReply("").answer(t, labelFormat(tr))
	if err := tr.Program(); err == nil {
		t.Fatal("Program() = nil, want the failure Main answered")
	}
	for _, never := range []string{"images", "builder", "volume"} {
		if anyCallNamed(tr.callsTo("docker"), never) {
			t.Errorf("a failed run went on to the trims: %v", callLines(tr, "docker"))
		}
	}
}

// TestRunMainLogsAConfigFailureItCannotRead is the program's failure before a
// run starts: the error is logged and the status is 1.
func TestRunMainLogsAConfigFailureItCannotRead(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if code := runMain(&out, envMap(map[string]string{})); code != 1 {
		t.Errorf("runMain() = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "ERROR: cannot find the town root") {
		t.Errorf("runMain() logged %q, want the missing town root", out.String())
	}
}

// TestRunProgramDispatchesTheTrimHelper pins the subcommand the image's
// container runs, so a helper call can never start a whole plugin run.
func TestRunProgramDispatchesTheTrimHelper(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	plantCacheFile(t, filepath.Join(root, "go-build", "a"), 10, testTime)
	var out, errOut bytes.Buffer
	code := runProgram([]string{"trim-cache", "--cap-bytes", "100", "--root", root}, &out, &errOut, envMap(map[string]string{}))
	if code != 0 {
		t.Fatalf("runProgram(trim-cache) = %d, want 0 (%s)", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != "under 10" {
		t.Errorf("runProgram(trim-cache) printed %q, want %q", got, "under 10")
	}
}

// TestRunProgramDispatchesARun is the other half of the switch: no arguments
// configures a run, and an environment it cannot read is the exit status.
func TestRunProgramDispatchesARun(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	if code := runProgram(nil, &out, &errOut, envMap(map[string]string{})); code != 1 {
		t.Errorf("runProgram() = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "ERROR: cannot find the town root") {
		t.Errorf("runProgram() logged %q, want the missing town root", out.String())
	}
}

// TestTrimCacheMainReportsAnUnderCapTree is the helper's success path: the byte
// count reaches stdout, which is what the caller logs.
func TestTrimCacheMainReportsAnUnderCapTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	plantCacheFile(t, filepath.Join(root, "go-build", "a"), 100, testTime)
	var out, errOut bytes.Buffer
	if code := trimCacheMain([]string{"--cap-bytes", "1000", "--root", root}, &out, &errOut); code != 0 {
		t.Fatalf("trimCacheMain() = %d, want 0 (%s)", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != "under 100" {
		t.Errorf("trimCacheMain() printed %q, want %q", got, "under 100")
	}
}

// TestTrimCacheMainReportsATrim drives the over-cap path end to end over a real
// tree.
func TestTrimCacheMainReportsATrim(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	plantCacheFile(t, filepath.Join(root, "go-build", "a"), 400, testTime)
	var out, errOut bytes.Buffer
	if code := trimCacheMain([]string{"--cap-bytes", "100", "--root", root}, &out, &errOut); code != 0 {
		t.Fatalf("trimCacheMain() = %d, want 0 (%s)", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != "trimmed 400 0" {
		t.Errorf("trimCacheMain() printed %q, want %q", got, "trimmed 400 0")
	}
}

// TestTrimCacheMainNeedsACap is the helper's refusal: a caller that names no cap
// gets exit 3 and a reason, never a trim to an assumed size.
func TestTrimCacheMainNeedsACap(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	if code := trimCacheMain([]string{"--root", t.TempDir()}, &out, &errOut); code != 3 {
		t.Errorf("trimCacheMain() = %d, want 3", code)
	}
	if !strings.Contains(errOut.String(), "--cap-bytes needs a whole number of bytes") {
		t.Errorf("trimCacheMain() said %q, want the missing cap", errOut.String())
	}
}

// TestTrimCacheMainReportsATreeItCannotMeasure is the helper's failure path: the
// caller logs a trim it cannot vouch for rather than a quiet success.
func TestTrimCacheMainReportsATreeItCannotMeasure(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	missing := filepath.Join(t.TempDir(), "absent")
	if code := trimCacheMain([]string{"--cap-bytes", "100", "--root", missing}, &out, &errOut); code != 3 {
		t.Fatalf("trimCacheMain() = %d, want 3", code)
	}
	if !strings.Contains(errOut.String(), missing) {
		t.Errorf("trimCacheMain() said %q, want the tree it could not measure", errOut.String())
	}
}
