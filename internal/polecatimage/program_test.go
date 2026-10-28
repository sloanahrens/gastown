package polecatimage

import (
	"bytes"
	"fmt"
	"os"
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
	home := t.TempDir()
	cfg, err := ConfigFromEnv(envMap(map[string]string{"HOME": home}))
	if err != nil {
		t.Fatalf("ConfigFromEnv() failed: %v", err)
	}
	town := filepath.Join(home, "gt")
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
	town := t.TempDir()
	cfg, err := ConfigFromEnv(envMap(map[string]string{
		"HOME":                        t.TempDir(),
		"POLECAT_IMAGE_TOWN_ROOT":     town,
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
		{cfg.LockDir, filepath.Join(town, ".runtime", "polecat-image.lock"), "lock"},
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
	_, err := ConfigFromEnv(envMap(map[string]string{"POLECAT_IMAGE_TOWN_ROOT": t.TempDir(), "POLECAT_IMAGE_STALE_MINUTES": "twenty"}))
	if err == nil || !strings.Contains(err.Error(), "POLECAT_IMAGE_STALE_MINUTES=twenty is not a whole number of minutes") {
		t.Fatalf("ConfigFromEnv() = %v, want the unreadable stale lock", err)
	}
}

// TestConfigFromEnvCarriesAnUnreadableKeepThrough is the prune-only knob's blast
// radius: a count that is not a number reaches the prune, which stops alone,
// instead of failing the build and publish path (gt-tpg0p.7).
func TestConfigFromEnvCarriesAnUnreadableKeepThrough(t *testing.T) {
	t.Parallel()
	cfg, err := ConfigFromEnv(envMap(map[string]string{"POLECAT_IMAGE_TOWN_ROOT": t.TempDir(), "POLECAT_IMAGE_KEEP": "three"}))
	if err != nil {
		t.Fatalf("ConfigFromEnv() failed: %v", err)
	}
	if cfg.Keep != "three" {
		t.Errorf("the keep count is %q, want the text the operator wrote", cfg.Keep)
	}
}

// writeTownImageConfig writes the town's settings/config.json.
func writeTownImageConfig(t *testing.T, town, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(town, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "settings", "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestConfigFromEnvPrefersTheEnvironmentToTheTownConfig pins the first source:
// what the daemon or the installer exported wins over the town's config, so an
// operator can point one run elsewhere (gt-du3fi).
func TestConfigFromEnvPrefersTheEnvironmentToTheTownConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownImageConfig(t, town, `{"daemon":{"env":{"POLECAT_IMAGE_REGISTRY":"config.example:5000","POLECAT_IMAGE_CRANE":"/config/bin/crane"}}}`)
	cfg, err := ConfigFromEnv(envMap(map[string]string{
		"POLECAT_IMAGE_TOWN_ROOT": town,
		"POLECAT_IMAGE_REGISTRY":  "env.example:6000",
		"POLECAT_IMAGE_CRANE":     "/env/bin/crane",
	}))
	if err != nil {
		t.Fatalf("ConfigFromEnv() failed: %v", err)
	}
	if cfg.Registry != "env.example:6000" {
		t.Errorf("the registry is %q, want the exported one", cfg.Registry)
	}
	if cfg.Crane != "/env/bin/crane" {
		t.Errorf("the crane is %q, want the exported one", cfg.Crane)
	}
}

// TestConfigFromEnvResolvesEachImageSettingAlone pins the two keys apart: one
// exported leaves the other to the town's config.
func TestConfigFromEnvResolvesEachImageSettingAlone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownImageConfig(t, town, `{"daemon":{"env":{"POLECAT_IMAGE_REGISTRY":"config.example:5000","POLECAT_IMAGE_CRANE":"/config/bin/crane"}}}`)
	for _, tc := range []struct {
		name     string
		env      map[string]string
		registry string
		crane    string
	}{
		{"neither exported", map[string]string{}, "config.example:5000", "/config/bin/crane"},
		{"registry exported", map[string]string{"POLECAT_IMAGE_REGISTRY": "env.example:6000"}, "env.example:6000", "/config/bin/crane"},
		{"crane exported", map[string]string{"POLECAT_IMAGE_CRANE": "/env/bin/crane"}, "config.example:5000", "/env/bin/crane"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{"POLECAT_IMAGE_TOWN_ROOT": town}
			for k, v := range tc.env {
				env[k] = v
			}
			cfg, err := ConfigFromEnv(envMap(env))
			if err != nil {
				t.Fatalf("ConfigFromEnv() failed: %v", err)
			}
			if cfg.Registry != tc.registry {
				t.Errorf("the registry is %q, want %q", cfg.Registry, tc.registry)
			}
			if cfg.Crane != tc.crane {
				t.Errorf("the crane is %q, want %q", cfg.Crane, tc.crane)
			}
		})
	}
}

// TestConfigFromEnvReadsTheTownConfigWhenTheEnvironmentNamesNeither is a hand
// install's path: a shell exports neither key, so both come from the town's
// settings/config.json, and no other key of it reaches the run (gt-du3fi).
//
// The other keys are deliberately not strings: a run that decoded daemon.env
// whole would fail on the number and the object, so the registry resolving
// while they sit in the same map is what shows only the named entry is decoded.
func TestConfigFromEnvReadsTheTownConfigWhenTheEnvironmentNamesNeither(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownImageConfig(t, town, `{"daemon":{"env":{
		"POLECAT_IMAGE_REGISTRY":"config.example:5000",
		"POLECAT_IMAGE_CRANE":"/config/bin/crane",
		"ANTHROPIC_API_KEY":"sk-town-sentinel",
		"GT_SEAT_REFILL_MAX_PRIORITY":50,
		"ANOTHER_SECRET":{"token":"sk-nested-sentinel"}}}}`)
	cfg, err := ConfigFromEnv(envMap(map[string]string{"POLECAT_IMAGE_TOWN_ROOT": town}))
	if err != nil {
		t.Fatalf("ConfigFromEnv() failed: %v", err)
	}
	if cfg.Registry != "config.example:5000" {
		t.Errorf("the registry is %q, want the town's", cfg.Registry)
	}
	if cfg.Crane != "/config/bin/crane" {
		t.Errorf("the crane is %q, want the town's", cfg.Crane)
	}
	// The rest of daemon.env is the town's secrets: a run reads the two image
	// keys and carries none of the others.
	if got := fmt.Sprintf("%+v", cfg); strings.Contains(got, "sk-town-sentinel") || strings.Contains(got, "sk-nested-sentinel") {
		t.Errorf("the run's configuration carries a daemon.env key it did not name: %s", got)
	}
}

// TestConfigFromEnvResolvesNoRegistryWhenNothingNamesOne pins the third source:
// no registry exported and none configured publishes nothing, which is not a
// fault to report (gt-du3fi).
func TestConfigFromEnvResolvesNoRegistryWhenNothingNamesOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body string }{
		{"no config file", ""},
		{"no daemon.env", `{"daemon":{}}`},
		{"no registry in daemon.env", `{"daemon":{"env":{"POLECAT_IMAGE_CRANE":"/config/bin/crane"}}}`},
		{"an empty registry", `{"daemon":{"env":{"POLECAT_IMAGE_REGISTRY":""}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.body != "" {
				writeTownImageConfig(t, town, tc.body)
			}
			cfg, err := ConfigFromEnv(envMap(map[string]string{"POLECAT_IMAGE_TOWN_ROOT": town}))
			if err != nil {
				t.Fatalf("ConfigFromEnv() failed: %v", err)
			}
			if cfg.Registry != "" {
				t.Errorf("the registry is %q, want none", cfg.Registry)
			}
		})
	}
}

// TestConfigFromEnvRefusesATownConfigItCannotRead keeps an unreadable config
// from passing as a town that asked for no publish: the run fails before it
// builds anything, and the error names what could not be read (gt-du3fi).
func TestConfigFromEnvRefusesATownConfigItCannotRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body, want string }{
		{"not JSON", `{not json`, "is not readable JSON"},
		{"daemon is not an object", `{"daemon":"nope"}`, "is not readable JSON"},
		{"daemon.env is not an object", `{"daemon":{"env":["config.example:5000"]}}`, "daemon.env in "},
		{"the registry is not a string", `{"daemon":{"env":{"POLECAT_IMAGE_REGISTRY":7001}}}`, "POLECAT_IMAGE_REGISTRY in daemon.env of "},
		{"the file cannot be read", "", "could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.body == "" {
				// A directory where the file belongs: the read fails with
				// something other than "there is no config here".
				if err := os.MkdirAll(filepath.Join(town, "settings", "config.json"), 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				writeTownImageConfig(t, town, tc.body)
			}
			_, err := ConfigFromEnv(envMap(map[string]string{"POLECAT_IMAGE_TOWN_ROOT": town}))
			if err == nil {
				t.Fatal("ConfigFromEnv() accepted a town config it cannot read")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error is %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestConfigFromEnvRefusesAnUnreadableConfigTheCraneNeeds is the crane's half:
// an exported registry leaves the crane to the town, and a config the run
// cannot read is the failure rather than a silent fall back to PATH's crane
// (gt-du3fi).
func TestConfigFromEnvRefusesAnUnreadableConfigTheCraneNeeds(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownImageConfig(t, town, `{not json`)
	_, err := ConfigFromEnv(envMap(map[string]string{
		"POLECAT_IMAGE_TOWN_ROOT": town,
		"POLECAT_IMAGE_REGISTRY":  "env.example:6000",
	}))
	if err == nil {
		t.Fatal("ConfigFromEnv() fell back to a crane it did not resolve")
	}
	if !strings.Contains(err.Error(), "is not readable JSON") {
		t.Errorf("the error is %q, want the unreadable config", err)
	}
}

// TestConfigFromEnvNeverReadsAConfigBothKeysOverride pins the other side: a run
// the daemon or the installer exported both image settings for needs no config,
// so a broken one does not fail it (gt-du3fi).
func TestConfigFromEnvNeverReadsAConfigBothKeysOverride(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownImageConfig(t, town, `{not json`)
	cfg, err := ConfigFromEnv(envMap(map[string]string{
		"POLECAT_IMAGE_TOWN_ROOT": town,
		"POLECAT_IMAGE_REGISTRY":  "env.example:6000",
		"POLECAT_IMAGE_CRANE":     "/env/bin/crane",
	}))
	if err != nil {
		t.Fatalf("ConfigFromEnv() read a config both keys override: %v", err)
	}
	if cfg.Registry != "env.example:6000" || cfg.Crane != "/env/bin/crane" {
		t.Errorf("the run resolved %q and %q, want the exported values", cfg.Registry, cfg.Crane)
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
	reply.list = result{stdout: "NAME CREATED\ngt-corpus 2026-10-23\n"}
	tr.reply = programAnswer(t, tr, reply)
	t.Cleanup(tr.Cleanup)
	if err := tr.Program(); err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	tr.wantLogged(t, "deferred: sandbox gt-corpus has no registry record yet")
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

// TestRunMainFailsWhenTheTownConfigCannotBeRead is the call site's half of the
// fail-closed rule: a run whose town config exists and cannot be read exits 1
// with the reason, rather than building and publishing nothing as a clean run
// (gt-du3fi).
func TestRunMainFailsWhenTheTownConfigCannotBeRead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownImageConfig(t, town, `{"daemon":{"env":{"POLECAT_IMAGE_REGISTRY":7001}}}`)
	var out bytes.Buffer
	if code := runMain(&out, envMap(map[string]string{"POLECAT_IMAGE_TOWN_ROOT": town})); code != 1 {
		t.Errorf("runMain() = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "ERROR: ") || !strings.Contains(out.String(), "POLECAT_IMAGE_REGISTRY in daemon.env of ") {
		t.Errorf("runMain() logged %q, want the unreadable town config", out.String())
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
