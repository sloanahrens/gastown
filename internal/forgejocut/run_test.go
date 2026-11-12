package forgejocut

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejoprov"
	"github.com/steveyegge/gastown/internal/forgejotest"
)

// The world one whole-run test builds: a town root, a rig with a bare repository,
// a mayor clone and a crew clone, the config home the promote key and the bot
// tokens go under, and the two git URLs the run reads and writes.
const (
	runTownRoot    = "/town"
	runRig         = "acme"
	runCrewName    = "operator"
	runRepoName    = "acme/rig"
	runGitHubURL   = "https://github.example/acme/rig.git"
	runForgejoURL  = "http://forge.example/acme/rig.git"
	runPromoteURL  = "git@github.example:acme/rig.git"
	runAPIURL      = forgejotest.APIRoot + forgejotest.APIPath
	runWebURL      = forgejotest.APIRoot
	runTokenFile   = "/fixture/admin.env"
	runConfigDir   = "/fixture/config"
	runKeyPath     = runConfigDir + "/gt/promote-acme.key"
	runScratchRoot = "/fake-scratch/forgejo-cutover.unit"
)

// runAdminGitURL is the web root plus the repository: the base the run's own git
// work rides.
var runAdminGitURL = runWebURL + "/" + runRepoName + ".git"

// The key material the fake ssh-keygen writes. The private half's marker is what
// every output assertion searches for; the public half's is what the derive path
// writes.
const (
	runPrivateKey = "RUN-PRIVATE-KEY-MATERIAL"
	runPublicKey  = "ssh-ed25519 AAAARUNPUBLICKEY promote-acme"
)

// runOptions is the cutover the run tests drive: every value the parse step
// derived, as the command would have derived it.
func runOptions(dryRun bool) *Options {
	return &Options{
		Rig:            runRig,
		TownRoot:       runTownRoot,
		RigRoot:        runTownRoot + "/" + runRig,
		BareRepo:       runTownRoot + "/" + runRig + "/.repo.git",
		Repo:           runRepoName,
		GitHubURL:      runGitHubURL,
		GitHubSlug:     runRepoName,
		ForgejoURL:     runForgejoURL,
		AdminGitURL:    runAdminGitURL,
		PromoteTarget:  runPromoteURL,
		PromoteKey:     runKeyPath,
		MainBranch:     DefaultMainBranch,
		GateWorkflow:   DefaultGateWorkflow,
		BotPrefix:      DefaultBotPrefix,
		APIURL:         runAPIURL,
		WebURL:         runWebURL,
		AdminTokenFile: runTokenFile,
		Probe:          "/probe/forgejo-probe.sh",
		GT:             "gt",
		DryRun:         dryRun,
	}
}

// runTownJSON is the town registry before the cutover: the rig's git_url is still
// GitHub's.
const runTownJSON = `{
  "type": "town",
  "version": 2,
  "registry": {
    "version": 1,
    "rigs": {
      "acme": {
        "dolt_database": "acme",
        "git_url": "` + runGitHubURL + `"
      }
    }
  }
}
`

// runSettingsJSON is the rig's settings before the cutover: no merge_queue.forgejo
// block yet.
const runSettingsJSON = `{
  "type": "rig-settings",
  "version": 1,
  "merge_queue": {
    "test_command": "make gate"
  }
}
`

// runGit is the git seam the whole run goes through: each remote's ref listing,
// each repository's origin, and the scratch repository the import fetches into
// and pushes from. A push is applied to the remote the way the server applies it,
// so a second run over the same world reads what the first pushed. Every call and
// every state change is recorded.
type runGit struct {
	refs      map[string]map[string]string // url -> ref -> sha
	scratch   map[string]map[string]string // dir -> ref -> sha fetched into it
	origins   map[string]string            // dir -> origin url
	bare      map[string]bool              // dir -> a bare repository whose git dir is dir
	work      map[string]bool              // dir -> a work tree whose top level is dir
	fail      map[string]error             // operation -> the error its call answers with
	ancestors map[string]bool              // "a>b" -> a is b or an ancestor of b
	ahead     map[string]string            // "a>b" -> the commits b has that a lacks
	calls     [][]string
	contexts  []context.Context // the context each call was run under
	writes    []string          // a push and a set-url, the calls that change a remote
}

// run runs one git argv.
func (g *runGit) run(ctx context.Context, argv []string) ([]byte, error) {
	g.calls = append(g.calls, slices.Clone(argv))
	g.contexts = append(g.contexts, ctx)
	if len(argv) < 2 || argv[0] != "git" {
		return nil, fmt.Errorf("not a git call: %q", argv)
	}
	sub, rest := argv[1], argv[2:]
	dir := ""
	if sub == "-C" {
		if len(rest) < 2 {
			return nil, fmt.Errorf("git -C with no subcommand: %q", argv)
		}
		dir, rest = rest[0], rest[1:]
		sub, rest = rest[0], rest[1:]
	}
	if sub == "ls-remote" || sub == "init" {
		switch sub {
		case "ls-remote":
			if err := g.fail[sub]; err != nil {
				return nil, err
			}
			return g.listing(rest[len(rest)-1])
		case "init":
			return nil, nil
		}
	}
	if len(rest) == 0 {
		return nil, fmt.Errorf("git %s with no arguments: %q", sub, argv)
	}
	switch sub {
	case "fetch":
		if err := g.fail[sub]; err != nil {
			return nil, err
		}
		url, ok := afterSeparatorValue(rest)
		if !ok {
			return nil, fmt.Errorf("fetch without a remote: %q", argv)
		}
		src, ok := g.refs[url]
		if !ok {
			return nil, fmt.Errorf("no remote at %s", url)
		}
		if g.scratch[dir] == nil {
			g.scratch[dir] = map[string]string{}
		}
		for name, sha := range src {
			g.scratch[dir][name] = sha
		}
		return nil, nil
	case "push":
		if err := g.fail[sub]; err != nil {
			return nil, err
		}
		url, ok := afterSeparatorValue(rest)
		if !ok {
			return nil, fmt.Errorf("push without a remote: %q", argv)
		}
		dst, ok := g.refs[url]
		if !ok {
			return nil, fmt.Errorf("no remote at %s", url)
		}
		g.writes = append(g.writes, "push "+url)
		for _, arg := range rest {
			if !strings.HasPrefix(arg, "refs/") {
				continue
			}
			src, name, ok := strings.Cut(arg, ":")
			if !ok {
				return nil, fmt.Errorf("refspec %q has no target", arg)
			}
			sha, ok := g.scratch[dir][src]
			if !ok {
				return nil, fmt.Errorf("%s holds no %s to push", dir, src)
			}
			dst[name] = sha
		}
		return nil, nil
	case "rev-parse":
		if err := g.fail[sub]; err != nil {
			return nil, err
		}
		switch rest[0] {
		case "--show-toplevel":
			if !g.work[dir] {
				return nil, errors.New("fatal: this operation must be run in a work tree")
			}
			return []byte(dir + "\n"), nil
		case "--is-bare-repository":
			if g.bare[dir] {
				return []byte("true\n.\n"), nil
			}
			if g.work[dir] {
				return []byte("false\n" + dir + "/.git\n"), nil
			}
			return nil, errors.New("fatal: not a git repository")
		case "--verify":
			return []byte(strings.TrimSuffix(rest[1], "^{commit}") + "\n"), nil
		}
		return nil, fmt.Errorf("unexpected rev-parse: %q", argv)
	case "merge-base":
		if rest[0] != "--is-ancestor" || len(rest) < 3 {
			return nil, fmt.Errorf("unexpected merge-base: %q", argv)
		}
		if g.ancestors[rest[1]+">"+rest[2]] {
			return nil, nil
		}
		return nil, exitStatus(1)
	case "rev-list":
		a, b, ok := strings.Cut(rest[1], "..")
		if !ok {
			return nil, fmt.Errorf("rev-list range %q", rest[1])
		}
		n, ok := g.ahead[a+">"+b]
		if !ok {
			return nil, fmt.Errorf("no count staged for %s..%s", a, b)
		}
		return []byte(n + "\n"), nil
	case "remote":
		switch rest[0] {
		case "get-url":
			origin, ok := g.origins[dir]
			if !ok {
				return nil, exitStatus(noSuchRemoteExit)
			}
			return []byte(origin + "\n"), nil
		case "set-url":
			if err := g.fail["set-url"]; err != nil {
				return nil, err
			}
			g.writes = append(g.writes, "set-url "+dir)
			g.origins[dir] = rest[2]
			return nil, nil
		}
	}
	return nil, fmt.Errorf("unexpected git call: %q", argv)
}

// afterSeparatorValue returns the word after the "--" that ends a fetch's or a
// push's options: the remote those calls name.
func afterSeparatorValue(args []string) (string, bool) {
	i := slices.Index(args, "--")
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

// listing renders one remote's refs the way ls-remote prints them.
func (g *runGit) listing(url string) ([]byte, error) {
	refs, ok := g.refs[url]
	if !ok {
		return nil, fmt.Errorf("no remote at %s", url)
	}
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s\t%s\n", refs[name], name)
	}
	return []byte(b.String()), nil
}

// runExec is the Runner seam the whole run goes through. It answers the landing
// queue read, the probe, ssh-keygen and the daemon restart from what a test
// staged, and records every call. A key mint is its only write: the probe and the
// restart are the script's commands that a converged run still makes.
type runExec struct {
	files      *memFiles
	counts     []string // the landing counts, in call order; the last repeats
	countErr   error
	probeErr   error
	mintErr    error
	deriveErr  error
	restartErr error
	calls      [][]string           // Output calls
	runs       [][]string           // Run calls
	tokens     []forgejoprov.Secret // the token each RunWithToken call was handed
	writes     []string             // a key mint
	countIndex int
}

func (f *runExec) Output(argv []string) ([]byte, error) {
	f.calls = append(f.calls, slices.Clone(argv))
	switch argv[0] {
	case "bd":
		if f.countErr != nil {
			return nil, f.countErr
		}
		if f.countIndex >= len(f.counts) {
			return []byte(f.counts[len(f.counts)-1] + "\n"), nil
		}
		answer := f.counts[f.countIndex]
		f.countIndex++
		return []byte(answer + "\n"), nil
	case "ssh-keygen":
		if f.deriveErr != nil {
			return nil, f.deriveErr
		}
		return []byte(runPublicKey + "\n"), nil
	}
	return nil, fmt.Errorf("unexpected Output call: %q", argv)
}

func (f *runExec) Run(argv []string) error {
	f.runs = append(f.runs, slices.Clone(argv))
	switch argv[0] {
	case "bash":
		return f.probeErr
	case "ssh-keygen":
		if f.mintErr != nil {
			return f.mintErr
		}
		key := argv[len(argv)-1]
		f.writes = append(f.writes, "ssh-keygen "+key)
		// The child writes both halves mode 644, as the script's stub did, so the
		// mode the step sets afterwards is what a test reads.
		f.files.addFile(key, runPrivateKey, 0o644)
		f.files.addFile(key+".pub", runPublicKey+"\n", 0o644)
		return nil
	case "env":
		return f.restartErr
	}
	return fmt.Errorf("unexpected Run call: %q", argv)
}

// RunWithToken is the probe's channel when no token file was named: it records
// the token and behaves as Run does.
func (f *runExec) RunWithToken(argv []string, token forgejoprov.Secret) error {
	f.tokens = append(f.tokens, token)
	return f.Run(argv)
}

// runWorld is one whole cutover's world: the options, the seam set and the fakes
// behind it, and the two streams the run wrote to.
type runWorld struct {
	opts   *Options
	env    RunEnv
	files  *memFiles
	git    *runGit
	exec   *runExec
	fake   *forgejotest.Fake
	pfiles *provisionFiles
	// madeTemp and removed are the import's scratch directory lifecycle: the
	// (dir, pattern) MkdirTemp was asked for, and the path RemoveAll was given.
	madeTemp *[]string
	removed  *[]string
	stdout   *strings.Builder
	stderr   *strings.Builder
}

// newRunWorld builds the world: a rig whose Forgejo copy already holds main while
// GitHub holds main, feature and a tag, so a first run imports two refs and a
// second finds the three converged.
func newRunWorld(t *testing.T, dryRun bool) *runWorld {
	t.Helper()
	opts := runOptions(dryRun)
	crewDir := opts.RigRoot + "/crew/" + runCrewName
	files := newMemFiles()
	for _, dir := range []string{
		runTownRoot, opts.RigRoot, opts.BareRepo,
		opts.RigRoot + "/mayor/rig", opts.RigRoot + "/crew", crewDir,
		opts.RigRoot + "/crew/notes", opts.RigRoot + "/settings", runTownRoot + "/mayor",
	} {
		files.addDir(dir)
	}
	files.addFile(runTownRoot+"/mayor/town.json", runTownJSON, 0o644)
	files.addFile(opts.RigRoot+"/settings/config.json", runSettingsJSON, 0o644)

	git := &runGit{
		refs: map[string]map[string]string{
			runGitHubURL: {
				"refs/heads/main":    shaMiddle,
				"refs/heads/feature": shaTip,
				"refs/tags/v1":       shaSide,
			},
			runAdminGitURL: {"refs/heads/main": shaMiddle},
		},
		scratch: map[string]map[string]string{},
		origins: map[string]string{
			opts.BareRepo:               runGitHubURL,
			opts.RigRoot + "/mayor/rig": runGitHubURL,
			crewDir:                     runGitHubURL,
		},
		bare: map[string]bool{opts.BareRepo: true},
		work: map[string]bool{
			opts.RigRoot + "/mayor/rig": true,
			crewDir:                     true,
		},
		fail:      map[string]error{},
		ancestors: map[string]bool{},
		ahead:     map[string]string{},
	}
	fake := forgejotest.New()
	fake.AddRepo(runRepoName)
	fake.ViewerExists = true
	pfiles := newProvisionFiles()
	exec := &runExec{files: files, counts: []string{"0"}}
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	madeTemp, removed := &[]string{}, &[]string{}
	env := RunEnv{
		Git:    git.run,
		Files:  files,
		Now:    func() time.Time { return time.Date(2026, 11, 1, 14, 5, 9, 0, time.UTC) },
		Runner: exec,
		MkdirTemp: func(dir, pattern string) (string, error) {
			*madeTemp = append(*madeTemp, dir+"|"+pattern)
			return runScratchRoot, nil
		},
		RemoveAll: func(path string) error {
			*removed = append(*removed, path)
			return nil
		},
		ProvisionEnv: forgejoprov.Env{
			Getenv:   runGetenv,
			ReadFile: runReadFile,
			Files:    pfiles,
			HTTP:     &http.Client{Transport: fake},
			ListLock: noListLock,
		},
		Stdout: stdout,
		Stderr: stderr,
	}
	return &runWorld{opts: opts, env: env, files: files, git: git, exec: exec, fake: fake, pfiles: pfiles, madeTemp: madeTemp, removed: removed, stdout: stdout, stderr: stderr}
}

// runGetenv is the environment seam a whole-run test reads: the config home the
// key and the bot tokens go under, and no token in the environment.
func runGetenv(name string) string {
	if name == "XDG_CONFIG_HOME" {
		return runConfigDir
	}
	return ""
}

// runReadFile is the token file reader a whole-run test reads: the stand-in admin
// token file, and an absent file for any other path.
func runReadFile(path string) ([]byte, error) {
	if path == runTokenFile {
		return []byte("FORGEJO_ADMIN_TOKEN=" + forgejotest.AdminToken + "\n"), nil
	}
	return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
}

// run executes the cutover over the world.
func (w *runWorld) run(t *testing.T, ctx context.Context) int {
	t.Helper()
	return Run(ctx, w.opts, w.env)
}

// output is everything the run wrote, stdout and stderr together, the way the
// script's test captured it.
func (w *runWorld) output() string { return w.stdout.String() + w.stderr.String() }

// failFile makes one Files operation fail for the run: op is stat, read, write,
// rename, chmod or mkdir, as failFiles reads it.
func (w *runWorld) failFile(op, path string) {
	w.env.Files = failFiles{memFiles: w.files, fail: map[string]error{op + " " + path: errExit}}
}

// writtenPaths is every path a fake recorded a write to, so a test can say where
// the run wrote rather than only how many times.
func (w *runWorld) writtenPaths() []string {
	var paths []string
	paths = append(paths, w.files.writes...)
	for _, pair := range w.files.renames {
		paths = append(paths, pair[0], pair[1])
	}
	for _, entry := range w.exec.writes {
		paths = append(paths, strings.TrimPrefix(entry, "ssh-keygen "))
	}
	for path := range w.pfiles.written {
		paths = append(paths, path)
	}
	return paths
}

// resetWrites forgets every write each fake recorded, so a second run's stillness
// is what the logs then hold.
func (w *runWorld) resetWrites() {
	w.files.writes = nil
	w.files.renames = nil
	w.git.writes = nil
	w.exec.writes = nil
	w.fake.ClearCalls()
}

// childArgv returns the first recorded run of program, or nil.
func childArgv(runs [][]string, program string) []string {
	for _, argv := range runs {
		if len(argv) > 0 && argv[0] == program {
			return slices.Clone(argv)
		}
	}
	return nil
}

// snapshot is the token files a provisioning run wrote, as a value a second run
// can be compared against.
type provisionSnapshot struct {
	written map[string]string
	modes   map[string]fs.FileMode
}

func (w *runWorld) snapshotTokens() provisionSnapshot {
	s := provisionSnapshot{written: map[string]string{}, modes: map[string]fs.FileMode{}}
	for path, data := range w.pfiles.written {
		s.written[path] = string(data)
	}
	for path, mode := range w.pfiles.modes {
		s.modes[path] = mode
	}
	return s
}

// TestRunDryRunWritesNothingOutsideItsTempDir: a dry run prints the probe, the
// ref import, the key it would mint, the gh deploy-key command and the daemon
// restart, and writes nothing outside the one scratch directory it makes and
// removes: no key, no settings, no town.json, no remote, no push, no Forgejo
// write and no restart. Making WriteSettings or the provisioner ignore DryRun
// turns it red.
func TestRunDryRunWritesNothingOutsideItsTempDir(t *testing.T) {
	t.Parallel()
	w := newRunWorld(t, true)
	beforeSettings := string(w.files.files[w.opts.RigRoot+"/settings/config.json"])
	beforeTown := string(w.files.files[runTownRoot+"/mayor/town.json"])

	if code := w.run(t, context.Background()); code != ExitOK {
		t.Fatalf("Run = %d, want 0 (output: %s)", code, w.output())
	}

	out := w.output()
	for _, want := range []string{
		"bash /probe/forgejo-probe.sh acme --repo acme/rig",
		"--town-root " + runTownRoot,
		"git ls-remote",
		"ssh-keygen -q -t ed25519",
		"dry run: no promote key was created",
		"gh repo deploy-key add",
		"--allow-write",
		"daemon restart",
		"actions/permissions -F enabled=false",
		"would back up",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run's output does not hold %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "backed up") {
		t.Errorf("the dry run claimed a backup it did not make:\n%s", out)
	}

	// The operator notes and the progress go to different streams, as the script's
	// note and log did.
	if !strings.Contains(w.stdout.String(), "gh repo deploy-key add") || !strings.Contains(w.stdout.String(), "Finish the cutover by hand:") {
		t.Errorf("stdout = %q, want the operator notes", w.stdout.String())
	}
	if strings.Contains(w.stderr.String(), "Finish the cutover by hand:") {
		t.Errorf("stderr = %q, want no operator note", w.stderr.String())
	}
	if strings.Contains(w.stdout.String(), "ls-remote") {
		t.Errorf("stdout = %q, want the progress on stderr", w.stdout.String())
	}

	if len(w.files.writes) != 0 || len(w.files.renames) != 0 {
		t.Errorf("the dry run wrote files: %q, renamed %q", w.files.writes, w.files.renames)
	}
	for _, path := range []string{runKeyPath, runKeyPath + ".pub"} {
		if _, ok := w.files.files[path]; ok {
			t.Errorf("the dry run created the key file %s", path)
		}
	}
	if len(w.git.writes) != 0 {
		t.Errorf("the dry run changed %q", w.git.writes)
	}
	if len(w.exec.writes) != 0 {
		t.Errorf("the dry run ran the write children %q", w.exec.writes)
	}
	if writes := w.fake.Writes(); len(writes) != 0 {
		t.Errorf("the dry run sent %d write(s) to Forgejo", len(writes))
	}
	if len(w.pfiles.written) != 0 {
		t.Errorf("the dry run wrote token files: %q", w.pfiles.written)
	}
	for _, argv := range w.exec.runs {
		if argv[0] == "bash" {
			t.Errorf("the dry run ran the probe: %q", argv)
		}
		if argv[0] == "env" {
			t.Errorf("the dry run restarted the daemon: %q", argv)
		}
	}
	if got := string(w.files.files[w.opts.RigRoot+"/settings/config.json"]); got != beforeSettings {
		t.Errorf("the dry run rewrote the settings:\n%s", got)
	}
	if got := string(w.files.files[runTownRoot+"/mayor/town.json"]); got != beforeTown {
		t.Errorf("the dry run rewrote town.json:\n%s", got)
	}

	// Its own temp dir is the one place a dry run may write: it is made from the
	// default temp location, removed when the import ends, and every path any fake
	// recorded a write to lies inside it.
	if !slices.Equal(*w.madeTemp, []string{"|forgejo-cutover."}) {
		t.Errorf("the dry run made the scratch directories %q, want one from the default temp dir", *w.madeTemp)
	}
	if !slices.Equal(*w.removed, []string{runScratchRoot}) {
		t.Errorf("the dry run removed %q, want its scratch directory %q", *w.removed, runScratchRoot)
	}
	for _, path := range w.writtenPaths() {
		if path != runScratchRoot && !strings.HasPrefix(path, runScratchRoot+"/") {
			t.Errorf("the dry run wrote outside its temp dir: %q", path)
		}
	}
}

// TestRunSecondRunChangesNothing drives the cutover twice over one world and holds
// the second run to no write anywhere: no Forgejo write, no push or set-url, no
// file write, no key operation and no key mint. The probe and the daemon restart
// still run — a converged run repeats both — and the test pins them to the first
// run's argv with no other child beside them. Making any step write
// unconditionally turns it red.
func TestRunSecondRunChangesNothing(t *testing.T) {
	t.Parallel()
	w := newRunWorld(t, false)
	if code := w.run(t, context.Background()); code != ExitOK {
		t.Fatalf("the first Run = %d, want 0 (output: %s)", code, w.output())
	}
	// The first run must have done the work the second is held back from.
	if len(w.git.writes) == 0 || len(w.files.writes) == 0 || len(w.exec.writes) == 0 {
		t.Fatalf("the first run did not write: git %q, files %q, exec %q", w.git.writes, w.files.writes, w.exec.writes)
	}
	if len(w.fake.Writes()) == 0 {
		t.Fatal("the first run sent no write to Forgejo")
	}
	firstProbe := childArgv(w.exec.runs, "bash")
	firstRestart := childArgv(w.exec.runs, "env")
	if firstProbe == nil || firstRestart == nil {
		t.Fatalf("the first run did not run the probe and the restart: %q", w.exec.runs)
	}
	runsBefore, readsBefore := len(w.exec.runs), len(w.exec.calls)

	w.resetWrites()
	tokensBefore := w.snapshotTokens()
	w.stdout.Reset()
	w.stderr.Reset()
	if code := w.run(t, context.Background()); code != ExitOK {
		t.Fatalf("the second Run = %d, want 0 (output: %s)", code, w.output())
	}
	if !strings.Contains(w.output(), "reusing the promote key") {
		t.Errorf("the second run did not reuse the promote key:\n%s", w.output())
	}

	if len(w.fake.Writes()) != 0 {
		t.Errorf("the second run sent %d write(s) to Forgejo: %v", len(w.fake.Writes()), w.fake.Writes())
	}
	if len(w.git.writes) != 0 {
		t.Errorf("the second run changed the remotes: %q", w.git.writes)
	}
	if len(w.files.writes) != 0 || len(w.files.renames) != 0 {
		t.Errorf("the second run wrote files: %q, renamed %q", w.files.writes, w.files.renames)
	}
	if len(w.exec.writes) != 0 {
		t.Errorf("the second run ran the write children %q", w.exec.writes)
	}
	// The two children a converged run still runs — the probe and the daemon
	// restart — ran with exactly the first run's argv, and no other child ran.
	// Making either step write, adding a child, or changing its argv turns this
	// red.
	secondRuns := w.exec.runs[runsBefore:]
	if len(secondRuns) != 2 {
		t.Fatalf("the second run ran %d child(ren) %q, want the probe and the restart", len(secondRuns), secondRuns)
	}
	if !slices.Equal(secondRuns[0], firstProbe) {
		t.Errorf("the second run's probe = %q, want the first run's %q", secondRuns[0], firstProbe)
	}
	if !slices.Equal(secondRuns[1], firstRestart) {
		t.Errorf("the second run's restart = %q, want the first run's %q", secondRuns[1], firstRestart)
	}
	secondReads := w.exec.calls[readsBefore:]
	if len(secondReads) == 0 {
		t.Error("the second run did not re-read the landing queue")
	}
	for _, argv := range secondReads {
		if len(argv) == 0 || argv[0] != "bd" {
			t.Errorf("the second run ran a read other than the landing-queue read: %q", argv)
		}
	}
	if got := w.snapshotTokens(); !reflect.DeepEqual(got, tokensBefore) {
		t.Errorf("the second run changed the token files: %+v, want %+v", got, tokensBefore)
	}
}

// TestRunStopsAtTheFirstFailingStepAndSaysWhich makes each step fail in turn and
// holds the run to stopping there: exit 1, the step named, and none of the
// operator notes the last step prints. Continuing past a failed step, or wording
// the failure without the step, turns a row red.
func TestRunStopsAtTheFirstFailingStepAndSaysWhich(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prepare func(*runWorld)
		// gone is a line a later step prints: the run must stop before it.
		gone string
	}{
		{
			name: "step 1 (refusing a landing in flight)",
			prepare: func(w *runWorld) {
				w.exec.counts = []string{"2"}
			},
			gone: "probing acme",
		},
		{
			name: "step 1 (the probe)",
			prepare: func(w *runWorld) {
				w.exec.probeErr = errExit
			},
			gone: "importing every GitHub ref",
		},
		{
			name: "step 2 (the ref import)",
			prepare: func(w *runWorld) {
				w.git.fail["ls-remote"] = errExit
			},
			gone: "provisioning acme/rig",
		},
		{
			name: "step 3 (provisioning)",
			prepare: func(w *runWorld) {
				w.env.ProvisionRun = func(context.Context, *forgejoprov.Options, forgejoprov.Env) int {
					return forgejoprov.ExitFailure
				}
			},
			gone: "created the promote key",
		},
		{
			name: "step 4 (the promote key)",
			prepare: func(w *runWorld) {
				w.exec.mintErr = errExit
			},
			gone: "remote set-url",
		},
		{
			name: "step 5 (repointing the remotes)",
			prepare: func(w *runWorld) {
				w.git.fail["set-url"] = errExit
			},
			gone: "set registry.rigs",
		},
		{
			name: "step 5 (the town's git_url)",
			prepare: func(w *runWorld) {
				w.files.addFile(runTownRoot+"/mayor/town.json", `{"registry":{"rigs":{}}}`, 0o644)
			},
			gone: "wrote merge_queue.forgejo",
		},
		{
			name: "step 6 (the settings)",
			prepare: func(w *runWorld) {
				w.files.addFile(w.opts.RigRoot+"/settings/config.json", "not json", 0o644)
			},
			gone: "daemon restart",
		},
		{
			name: "step 7 (the daemon restart)",
			prepare: func(w *runWorld) {
				w.exec.restartErr = errExit
			},
			gone: "Finish the cutover by hand:",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newRunWorld(t, false)
			tt.prepare(w)

			code := w.run(t, context.Background())
			if code != ExitFailure {
				t.Fatalf("Run with %s failing = %d, want 1 (output: %s)", tt.name, code, w.output())
			}
			if !strings.Contains(w.stderr.String(), tt.name) {
				t.Errorf("stderr does not name %q:\n%s", tt.name, w.stderr.String())
			}
			if strings.Contains(w.output(), tt.gone) {
				t.Errorf("the run continued past %s to a later step's line %q:\n%s", tt.name, tt.gone, w.output())
			}
			if strings.Contains(w.output(), "cutover of "+runRig+" complete") {
				t.Errorf("the run claimed completion after %s failed:\n%s", tt.name, w.output())
			}
		})
	}
}

// TestRunPromoteKey is step 4 through Run: a minted key is mode 600 with a mode
// 644 public half, the private half never reaches an output line, the gh command
// names only the public half, an existing key is reused without running
// ssh-keygen, and a key missing its public half has one derived instead of being
// replaced.
func TestRunPromoteKey(t *testing.T) {
	t.Parallel()
	t.Run("mints a key and prints only its public half", func(t *testing.T) {
		t.Parallel()
		w := newRunWorld(t, false)
		if code := w.run(t, context.Background()); code != ExitOK {
			t.Fatalf("Run = %d, want 0 (output: %s)", code, w.output())
		}
		if got := w.files.modes[runKeyPath]; got != 0o600 {
			t.Errorf("the private key mode = %v, want 0600", got)
		}
		if got := w.files.modes[runKeyPath+".pub"]; got != 0o644 {
			t.Errorf("the public half mode = %v, want 0644", got)
		}
		mint := w.exec.writes
		if len(mint) != 1 || !strings.Contains(mint[0], runKeyPath) {
			t.Fatalf("the ssh-keygen calls = %q, want one mint at %s", mint, runKeyPath)
		}
		var mintArgv []string
		for _, argv := range w.exec.runs {
			if len(argv) > 0 && argv[0] == "ssh-keygen" {
				mintArgv = argv
			}
		}
		if !slices.Contains(mintArgv, "-t") || !slices.Contains(mintArgv, "ed25519") || !slices.Contains(mintArgv, runKeyPath) {
			t.Errorf("the mint argv = %q, want -t ed25519 -f %s", mintArgv, runKeyPath)
		}
		if strings.Contains(w.output(), runPrivateKey) {
			t.Fatalf("an output line carries the private key material:\n%s", w.output())
		}
		for _, want := range []string{
			"gh repo deploy-key add " + runKeyPath + ".pub",
			"--repo " + runRepoName,
			"--title promote-" + runRig,
			"--allow-write",
		} {
			if !strings.Contains(w.stdout.String(), want) {
				t.Errorf("the gh note does not hold %q:\n%s", want, w.stdout.String())
			}
		}
	})

	t.Run("reuses an existing key without running ssh-keygen", func(t *testing.T) {
		t.Parallel()
		w := newRunWorld(t, false)
		w.files.addFile(runKeyPath, runPrivateKey, 0o600)
		w.files.addFile(runKeyPath+".pub", runPublicKey+"\n", 0o644)
		if code := w.run(t, context.Background()); code != ExitOK {
			t.Fatalf("Run = %d, want 0 (output: %s)", code, w.output())
		}
		if len(w.exec.writes) != 0 {
			t.Errorf("the run ran ssh-keygen for an existing key: %q", w.exec.writes)
		}
		if !strings.Contains(w.output(), "reusing the promote key") {
			t.Errorf("the run did not say it reused the key:\n%s", w.output())
		}
	})

	t.Run("sets a reused key whose mode drifted back to 600", func(t *testing.T) {
		t.Parallel()
		w := newRunWorld(t, false)
		w.files.addFile(runKeyPath, runPrivateKey, 0o644)
		w.files.addFile(runKeyPath+".pub", runPublicKey+"\n", 0o644)
		if code := w.run(t, context.Background()); code != ExitOK {
			t.Fatalf("Run = %d, want 0 (output: %s)", code, w.output())
		}
		if got := w.files.modes[runKeyPath]; got != 0o600 {
			t.Errorf("the reused private key mode = %v, want 0600", got)
		}
		if got := string(w.files.files[runKeyPath+".pub"]); got != runPublicKey+"\n" {
			t.Errorf("the run rewrote the public half: %q", got)
		}
		if len(w.exec.writes) != 0 {
			t.Errorf("the run minted a key over an existing one: %q", w.exec.writes)
		}
	})

	t.Run("derives a missing public half rather than a new key", func(t *testing.T) {
		t.Parallel()
		w := newRunWorld(t, false)
		w.files.addFile(runKeyPath, runPrivateKey, 0o644)
		if code := w.run(t, context.Background()); code != ExitOK {
			t.Fatalf("Run = %d, want 0 (output: %s)", code, w.output())
		}
		if got := string(w.files.files[runKeyPath+".pub"]); !strings.Contains(got, "AAAARUNPUBLICKEY") {
			t.Errorf("the derived public half = %q, want the key ssh-keygen printed", got)
		}
		if got := w.files.modes[runKeyPath+".pub"]; got != 0o644 {
			t.Errorf("the derived public half mode = %v, want 0644", got)
		}
		if got := w.files.modes[runKeyPath]; got != 0o600 {
			t.Errorf("the reused private key mode = %v, want 0600", got)
		}
		for _, argv := range w.exec.runs {
			if len(argv) > 0 && argv[0] == "ssh-keygen" {
				t.Fatalf("the run minted a new keypair instead of deriving: %q", argv)
			}
		}
		if !strings.Contains(w.output(), "derived "+runKeyPath+".pub") {
			t.Errorf("the run did not say it derived the public half:\n%s", w.output())
		}
	})
}

// TestRunPromoteKeyFailures drives every filesystem and child refusal step 4
// has: each names what it could not do and stops the run. Removing a check turns
// its row red with a nil error from Run.
func TestRunPromoteKeyFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(*runWorld)
		want  string
	}{
		{
			name:  "the private half cannot be looked at",
			setup: func(w *runWorld) { w.failFile("stat", runKeyPath) },
			want:  "could not look at the promote key",
		},
		{
			name:  "the key directory cannot be made",
			setup: func(w *runWorld) { w.failFile("mkdir", runConfigDir+"/gt") },
			want:  "could not make the promote key's directory",
		},
		{
			name:  "ssh-keygen fails",
			setup: func(w *runWorld) { w.exec.mintErr = errExit },
			want:  "could not be created",
		},
		{
			name: "the private half cannot be set to mode 600",
			setup: func(w *runWorld) {
				w.failFile("chmod", runKeyPath)
			},
			want: "mode 600",
		},
		{
			name: "the public half cannot be set to mode 644",
			setup: func(w *runWorld) {
				w.failFile("chmod", runKeyPath+".pub")
			},
			want: "mode 644",
		},
		{
			name: "the existing public half cannot be looked at",
			setup: func(w *runWorld) {
				w.files.addFile(runKeyPath, runPrivateKey, 0o600)
				w.failFile("stat", runKeyPath+".pub")
			},
			want: "could not look at the public half",
		},
		{
			name: "the existing key cannot be set to mode 600",
			setup: func(w *runWorld) {
				w.files.addFile(runKeyPath, runPrivateKey, 0o644)
				w.failFile("chmod", runKeyPath)
			},
			want: "mode 600",
		},
		{
			name: "the reused key cannot be set to mode 600 when its public half exists",
			setup: func(w *runWorld) {
				w.files.addFile(runKeyPath, runPrivateKey, 0o644)
				w.files.addFile(runKeyPath+".pub", runPublicKey+"\n", 0o644)
				w.failFile("chmod", runKeyPath)
			},
			want: "mode 600",
		},
		{
			name: "ssh-keygen cannot derive the public half",
			setup: func(w *runWorld) {
				w.files.addFile(runKeyPath, runPrivateKey, 0o600)
				w.exec.deriveErr = errExit
			},
			want: "could not derive the public half",
		},
		{
			name: "the derived public half cannot be written",
			setup: func(w *runWorld) {
				w.files.addFile(runKeyPath, runPrivateKey, 0o600)
				w.failFile("write", runKeyPath+".pub.tmp")
			},
			want: "could not write the public half",
		},
		{
			name: "the derived public half cannot be set to mode 644",
			setup: func(w *runWorld) {
				w.files.addFile(runKeyPath, runPrivateKey, 0o600)
				w.failFile("chmod", runKeyPath+".pub.tmp")
			},
			want: "mode 644",
		},
		{
			name: "the derived public half cannot be moved into place",
			setup: func(w *runWorld) {
				w.files.addFile(runKeyPath, runPrivateKey, 0o600)
				w.failFile("rename", runKeyPath+".pub")
			},
			want: "could not move the public half",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newRunWorld(t, false)
			tt.setup(w)
			code := w.run(t, context.Background())
			if code != ExitFailure {
				t.Fatalf("Run = %d, want 1 (output: %s)", code, w.output())
			}
			if !strings.Contains(w.stderr.String(), tt.want) {
				t.Errorf("stderr does not say %q:\n%s", tt.want, w.stderr.String())
			}
			if !strings.Contains(w.stderr.String(), "step 4 (the promote key)") {
				t.Errorf("stderr does not name step 4:\n%s", w.stderr.String())
			}
		})
	}
}

// TestRunExitCodes pins the run's exit contract: a missing admin token is a
// failed run (1), and the run never returns the usage code — a refused command
// line is the command's, before the run. The command's own TestRunExitCodes covers
// the 2.
func TestRunExitCodes(t *testing.T) {
	t.Parallel()
	t.Run("no admin token exits 1 and sends no request", func(t *testing.T) {
		t.Parallel()
		w := newRunWorld(t, false)
		const decoy = "decoy-token-in-an-unrelated-file"
		const absent = "/fixture/absent.env"
		w.opts.AdminTokenFile = absent
		var read []string
		w.env.ProvisionEnv.ReadFile = func(path string) ([]byte, error) {
			read = append(read, path)
			if path == absent {
				return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
			}
			// A token in any other file is one the run must never look at.
			return []byte("FORGEJO_ADMIN_TOKEN=" + decoy + "\n"), nil
		}
		if code := w.run(t, context.Background()); code != ExitFailure {
			t.Fatalf("Run = %d, want 1 (output: %s)", code, w.output())
		}
		if !strings.Contains(w.stderr.String(), "sudo -u gt-forge gt-approve run "+forgejoprov.CutoverRule+" -- <its arguments>") {
			t.Errorf("the failure does not name this command's broker rule:\n%s", w.stderr.String())
		}
		if strings.Contains(w.stderr.String(), "run forgejo-cutover --") {
			t.Errorf("the failure names the retired shell script's broker rule:\n%s", w.stderr.String())
		}
		if strings.Contains(w.output(), decoy) {
			t.Fatalf("the failure echoes a token from a file it should not read:\n%s", w.output())
		}
		if !slices.Equal(read, []string{absent}) {
			t.Errorf("the run read %q, want only the named token file %q", read, absent)
		}
		if len(w.exec.calls) != 0 || len(w.exec.runs) != 0 || len(w.git.calls) != 0 {
			t.Errorf("a run with no token reached the outside world: bd %q, children %q, git %q", w.exec.calls, w.exec.runs, w.git.calls)
		}
	})

	t.Run("a token the provisioner refuses exits 1 without echoing it", func(t *testing.T) {
		t.Parallel()
		w := newRunWorld(t, false)
		w.env.ProvisionEnv.Getenv = func(name string) string {
			if name == "FORGEJO_ADMIN_TOKEN" {
				return "sec ret"
			}
			return ""
		}
		if code := w.run(t, context.Background()); code != ExitFailure {
			t.Fatalf("Run = %d, want 1 (output: %s)", code, w.output())
		}
		if !strings.Contains(w.stderr.String(), "holds whitespace, a quote or a backslash") {
			t.Errorf("the failure does not name the rule:\n%s", w.stderr.String())
		}
		if strings.Contains(w.output(), "sec ret") {
			t.Fatalf("the failure echoes the token:\n%s", w.output())
		}
	})
}

// TestRunDivergedRef: a ref whose two sides have diverged stops the run before
// anything is pushed, in a real run and in a dry run alike. The dry run reports
// the stop it would be and still writes nothing.
func TestRunDivergedRef(t *testing.T) {
	t.Parallel()
	for _, dryRun := range []bool{false, true} {
		name := "a real run"
		if dryRun {
			name = "a dry run"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newRunWorld(t, dryRun)
			w.git.refs[runGitHubURL] = map[string]string{"refs/heads/main": shaTip}
			w.git.refs[runAdminGitURL] = map[string]string{"refs/heads/main": shaSide}

			if code := w.run(t, context.Background()); code != ExitFailure {
				t.Fatalf("Run = %d, want 1 (output: %s)", code, w.output())
			}
			if !strings.Contains(w.output(), "diverged: refs/heads/main") {
				t.Errorf("the run does not name the diverged ref:\n%s", w.output())
			}
			if !strings.Contains(w.stderr.String(), "step 2 (the ref import)") {
				t.Errorf("the failure does not name step 2:\n%s", w.stderr.String())
			}
			if len(w.git.writes) != 0 {
				t.Errorf("the run pushed before stopping: %q", w.git.writes)
			}
			if len(w.files.writes) != 0 {
				t.Errorf("the run wrote after the divergence: %q", w.files.writes)
			}
			for _, path := range []string{runKeyPath, runKeyPath + ".pub"} {
				if _, ok := w.files.files[path]; ok {
					t.Errorf("the run wrote the key file %s after the divergence", path)
				}
			}
		})
	}
}

// TestRunRestartLeftToTheOperator: a landing queued when step 7 re-reads the
// queue costs the restart, not the cutover. The run finishes 0, the rig is cut
// over, the daemon is not restarted, and the operator is told the command to run.
func TestRunRestartLeftToTheOperator(t *testing.T) {
	t.Parallel()
	w := newRunWorld(t, false)
	w.exec.counts = []string{"0", "1"}
	if code := w.run(t, context.Background()); code != ExitOK {
		t.Fatalf("Run = %d, want 0 (output: %s)", code, w.output())
	}
	if !strings.Contains(w.output(), "1 open gt:ready-to-land") {
		t.Errorf("the run does not say the queue is not empty:\n%s", w.output())
	}
	if !strings.Contains(w.output(), "gt daemon restart") {
		t.Errorf("the run does not print the restart command for later:\n%s", w.output())
	}
	for _, argv := range w.exec.runs {
		if len(argv) > 0 && argv[0] == "env" {
			t.Errorf("the daemon was restarted over a queued landing: %q", argv)
		}
	}
	if settings := string(w.files.files[w.opts.RigRoot+"/settings/config.json"]); !strings.Contains(settings, runForgejoURL) {
		t.Errorf("the rig was not cut over:\n%s", settings)
	}
}

// TestRunSummaryNamesTheRestart: the run's closing line reads step 7's outcome,
// so a finished cutover says whether the daemon was restarted or left to the
// operator. A summary that ignores the outcome — or step 7 that reports a skip
// as a restart — turns the queued row red.
func TestRunSummaryNamesTheRestart(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		count string
		want  string
	}{
		{"an empty queue", "0", "the daemon was restarted"},
		{"a queued landing", "1", "the daemon was not restarted: a landing is queued"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newRunWorld(t, false)
			w.exec.counts = []string{"0", tt.count}

			if code := w.run(t, context.Background()); code != ExitOK {
				t.Fatalf("Run = %d, want 0 (output: %s)", code, w.output())
			}
			want := "cutover of " + runRig + " complete; " + tt.want
			if !strings.Contains(w.output(), want) {
				t.Errorf("the summary line does not say %q:\n%s", want, w.output())
			}
		})
	}
}

// TestRunBrokerToken: the token a broker's run act hands over in the environment,
// and the stand-in's FORGEJO_ADMIN_TOKEN, are the two sources a run with no token
// file reads. Either way every request carries it, no argv, output line or file
// the run writes holds it, and the probe and the provisioner are handed no token
// file at all.
func TestRunBrokerToken(t *testing.T) {
	t.Parallel()
	const token = forgejotest.AdminToken
	tests := []struct{ name, variable string }{
		{"the broker's run act", "GT_AUTH_FORGEJO_TOKEN"},
		{"the environment override", "FORGEJO_ADMIN_TOKEN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newRunWorld(t, false)
			w.opts.AdminTokenFile = "" // the broker names no token file
			w.env.ProvisionEnv.Getenv = func(name string) string {
				switch name {
				case tt.variable:
					return token
				case "XDG_CONFIG_HOME":
					return runConfigDir
				}
				return ""
			}
			var provisioned *forgejoprov.Options
			w.env.ProvisionRun = func(ctx context.Context, opts *forgejoprov.Options, env forgejoprov.Env) int {
				provisioned = opts
				return forgejoprov.Run(ctx, opts, env)
			}
			if code := w.run(t, context.Background()); code != ExitOK {
				t.Fatalf("Run = %d, want 0 (output: %s)", code, w.output())
			}
			if provisioned == nil {
				t.Fatal("the provisioner was not handed its options")
			}
			if provisioned.AdminTokenFile != "" {
				t.Errorf("the provisioner was handed the token file %q with none named", provisioned.AdminTokenFile)
			}
			if violations := w.fake.Violations(); len(violations) != 0 {
				t.Errorf("%d request(s) reached Forgejo without the token: %v", len(violations), violations)
			}
			for _, argv := range append(append(slices.Clone(w.exec.calls), w.exec.runs...), w.git.calls...) {
				if slices.Contains(argv, token) || strings.Contains(strings.Join(argv, " "), token) {
					t.Errorf("an argv carries the token: %q", argv)
				}
			}
			if strings.Contains(w.output(), token) {
				t.Fatalf("an output line carries the token:\n%s", w.output())
			}
			for path, content := range w.files.files {
				if strings.Contains(string(content), token) {
					t.Errorf("%s holds the token", path)
				}
			}
			for path, content := range w.pfiles.written {
				if strings.Contains(string(content), token) {
					t.Errorf("%s holds the token", path)
				}
			}
			var probes int
			for _, argv := range w.exec.runs {
				if len(argv) == 0 || argv[0] != "bash" {
					continue
				}
				probes++
				if slices.Contains(argv, "--admin-token-file") {
					t.Errorf("the probe was handed a token file with none named: %q", argv)
				}
			}
			if probes != 1 {
				t.Errorf("the probe ran %d times, want once", probes)
			}
			// The probe is the one child handed the token, and it is handed the
			// resolved value; no other child's call carries one.
			if len(w.exec.tokens) != 1 || w.exec.tokens[0].Reveal() != token {
				t.Errorf("the probe was handed %d token(s), want exactly the resolved one", len(w.exec.tokens))
			}
			if settings := string(w.files.files[w.opts.RigRoot+"/settings/config.json"]); !strings.Contains(settings, runForgejoURL) {
				t.Errorf("the run did not converge the rig's settings:\n%s", settings)
			}
		})
	}
}

// TestRunRefusesAnUnwiredSeam: a cutover with no exec seam, or no environment
// seam for the admin token, refuses before any step rather than skip the step that
// needed it.
func TestRunRefusesAnUnwiredSeam(t *testing.T) {
	t.Parallel()
	t.Run("no exec seam", func(t *testing.T) {
		t.Parallel()
		w := newRunWorld(t, false)
		w.env.Runner = nil
		if code := Run(context.Background(), w.opts, w.env); code != ExitFailure {
			t.Fatalf("Run = %d, want 1", code)
		}
		if !strings.Contains(w.stderr.String(), "exec seam is not wired") {
			t.Errorf("stderr = %q, want the unwired seam named", w.stderr.String())
		}
	})
	t.Run("no environment seam", func(t *testing.T) {
		t.Parallel()
		w := newRunWorld(t, false)
		w.env.ProvisionEnv.Getenv = nil
		if code := Run(context.Background(), w.opts, w.env); code != ExitFailure {
			t.Fatalf("Run = %d, want 1", code)
		}
		if !strings.Contains(w.stderr.String(), errProvisionEnvUnwired.Error()) {
			t.Errorf("stderr = %q, want the unwired environment named", w.stderr.String())
		}
	})
	t.Run("no options", func(t *testing.T) {
		t.Parallel()
		w := newRunWorld(t, false)
		if code := Run(context.Background(), nil, w.env); code != ExitFailure {
			t.Fatalf("Run = %d, want 1", code)
		}
		if !strings.Contains(w.stderr.String(), "no options") {
			t.Errorf("stderr = %q, want the missing options named", w.stderr.String())
		}
	})
}

// TestRunContextReachesTheProvisioningRun: the in-process provisioning run gets
// the cutover's own context, so an interrupt reaches the requests it makes.
func TestRunContextReachesTheProvisioningRun(t *testing.T) {
	t.Parallel()
	type ctxKey struct{}
	w := newRunWorld(t, false)
	var got context.Context
	w.env.ProvisionRun = func(ctx context.Context, _ *forgejoprov.Options, _ forgejoprov.Env) int {
		got = ctx
		return forgejoprov.ExitOK
	}
	ctx := context.WithValue(context.Background(), ctxKey{}, "the run's context")
	if code := w.run(t, ctx); code != ExitOK {
		t.Fatalf("Run = %d, want 0 (output: %s)", code, w.output())
	}
	if got == nil || got.Value(ctxKey{}) != "the run's context" {
		t.Error("the provisioning run got a context the cutover was not given")
	}
}

// TestRunContextReachesTheGitCalls: every git call the run makes — the ref
// listings, the scratch repository, the plan's reads, the push and the repoint —
// runs under the cutover's own context, so an interrupt reaches a call in flight.
// Running a call under context.Background instead turns it red.
func TestRunContextReachesTheGitCalls(t *testing.T) {
	t.Parallel()
	type ctxKey struct{}
	const want = "the run's context"
	w := newRunWorld(t, false)
	ctx := context.WithValue(context.Background(), ctxKey{}, want)
	if code := w.run(t, ctx); code != ExitOK {
		t.Fatalf("Run = %d, want 0 (output: %s)", code, w.output())
	}
	if len(w.git.contexts) != len(w.git.calls) || len(w.git.calls) == 0 {
		t.Fatalf("recorded %d context(s) for %d git call(s)", len(w.git.contexts), len(w.git.calls))
	}
	for i, got := range w.git.contexts {
		if got == nil || got.Value(ctxKey{}) != want {
			t.Errorf("git call %d ran under a context the cutover was not given: %q", i, w.git.calls[i])
		}
	}
}

// TestRunSendsNoMirrorRequest: the port dropped --mirror, so a default run sends
// Forgejo no mirror request and writes no mirror target. Re-adding a mirror
// call turns it red.
func TestRunSendsNoMirrorRequest(t *testing.T) {
	t.Parallel()
	w := newRunWorld(t, false)
	if code := w.run(t, context.Background()); code != ExitOK {
		t.Fatalf("Run = %d, want 0 (output: %s)", code, w.output())
	}
	for _, call := range w.fake.CallLog() {
		if strings.Contains(strings.ToLower(call.Path), "mirror") {
			t.Errorf("a default run sent %s %s, a mirror request", call.Method, call.Path)
		}
	}
	if settings := string(w.files.files[w.opts.RigRoot+"/settings/config.json"]); strings.Contains(settings, "mirror") {
		t.Errorf("the settings name a mirror:\n%s", settings)
	}
}

// TestRunCanceledContext: a canceled context stops the run before the step it
// names, so nothing past the operator's stop is done.
func TestRunCanceledContext(t *testing.T) {
	t.Parallel()
	w := newRunWorld(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := w.run(t, ctx); code != ExitFailure {
		t.Fatalf("Run = %d, want 1 (output: %s)", code, w.output())
	}
	if !strings.Contains(w.stderr.String(), "canceled before step 1 (refusing a landing in flight)") {
		t.Errorf("stderr = %q, want the step the run stopped before", w.stderr.String())
	}
	if len(w.exec.calls) != 0 || len(w.git.calls) != 0 {
		t.Errorf("a canceled run reached the outside world: bd %q, git %q", w.exec.calls, w.git.calls)
	}
}
