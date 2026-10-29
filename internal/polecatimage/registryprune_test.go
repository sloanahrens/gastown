package polecatimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// roundTrip serves registry requests from handler in process: no listener, no
// network, so a unit test needs neither.
type roundTrip http.HandlerFunc

func (h roundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	http.HandlerFunc(h).ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// failingRoundTrip fails every request, the way an unreachable registry does.
type failingRoundTrip struct{ err error }

func (f failingRoundTrip) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

// registryTag is one tag the fake registry holds.
type registryTag struct {
	tag     string
	digest  string
	created time.Time
}

// fakeRegistry serves the registry's v2 API in process and records every
// request, so a test can say what a run asked for and whether it deleted.
type fakeRegistry struct {
	mu       sync.Mutex
	requests []string // "METHOD /path"
	deletes  []string // digests deleted
	tags     []registryTag
	trace    *[]string // appended to as well, when a test orders calls
}

func newFakeRegistry(tags ...registryTag) *fakeRegistry {
	return &fakeRegistry{tags: tags}
}

// requested is every request the run made, in order.
func (f *fakeRegistry) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// deleted is every digest the run deleted, in order.
func (f *fakeRegistry) deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
}

func (f *fakeRegistry) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if f.trace != nil {
		*f.trace = append(*f.trace, r.Method+" "+r.URL.Path)
	}
	f.mu.Unlock()
	switch {
	case r.URL.Path == "/v2/_catalog":
		writeJSONBody(w, map[string][]string{"repositories": {"gastown-polecat"}})
	case strings.HasSuffix(r.URL.Path, "/tags/list"):
		names := make([]string, len(f.tags))
		for i, tag := range f.tags {
			names[i] = tag.tag
		}
		writeJSONBody(w, map[string]any{"name": "gastown-polecat", "tags": names})
	case strings.Contains(r.URL.Path, "/manifests/"):
		ref := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		tag, ok := f.find(ref)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodDelete {
			f.mu.Lock()
			f.deletes = append(f.deletes, tag.digest)
			f.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Docker-Content-Digest", tag.digest)
		fmt.Fprintf(w, `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":%q,"size":10},"layers":[{"digest":%q,"size":20}]}`,
			configOfDigest(tag.digest), registryDigest("layer"))
	case strings.Contains(r.URL.Path, "/blobs/"):
		config := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		for _, tag := range f.tags {
			if configOfDigest(tag.digest) != config {
				continue
			}
			fmt.Fprintf(w, `{"created":%q}`, tag.created.UTC().Format(time.RFC3339Nano))
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// find returns the tag a ref names, by tag or by digest.
func (f *fakeRegistry) find(ref string) (registryTag, bool) {
	for _, tag := range f.tags {
		if tag.tag == ref || tag.digest == ref {
			return tag, true
		}
	}
	return registryTag{}, false
}

// registryDigest builds the digest the fixture tag label stands for: a sha256
// of the label, so a test can name the digest a tag it also names.
func registryDigest(label string) string {
	sum := sha256.Sum256([]byte(label))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// tagName is the nth candidate tag: seven lowercase hex characters, the shape
// the planner reads as a commit tag and the installed commit can name.
func tagName(n int) string { return fmt.Sprintf("%x%06x", n, n) }

// configOfDigest is the config digest the fake serves for a manifest, so each
// tag's blob carries that tag's own created time.
func configOfDigest(manifestDigest string) string {
	sum := sha256.Sum256([]byte(manifestDigest))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeJSONBody(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// registryTagAt is the nth fixture tag: created times rise with n, so the
// newest-KEEP order is the reverse of the argument order.
func registryTagAt(n int) registryTag {
	tag := tagName(n)
	return registryTag{tag: tag, digest: registryDigest(tag), created: time.Date(2026, 10, 20+n, 9, 0, 0, 0, time.UTC)}
}

// devTag is the moving dev tag, which every plan needs as its anchor.
func devTag() registryTag {
	return registryTag{tag: "dev", digest: registryDigest("dev"), created: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)}
}

// newRegistryRun is a run that holds the plugin lock, with reg answering the
// retention's HTTP calls, an installed gt at installed, and a kubectl listing
// no pods.
func newRegistryRun(t *testing.T, reg *fakeRegistry, installed string) *testRun {
	t.Helper()
	tr := newTestRun(t)
	tr.Cfg.Registry = "registry.example:5000"
	tr.Cfg.RegistryKeep = "1"
	tr.Cfg.RegistryKubeconfig = filepath.Join(tr.root, "runtime", "sbx-k8s", "kubeconfig")
	tr.Cfg.RegistryLedger = filepath.Join(tr.root, "runtime", "diskmaint", "ledger.jsonl")
	tr.HaveLock = true
	tr.web = &http.Client{Transport: roundTrip(reg.handler)}
	tr.reply = registryReply(installed, `{"items":[]}`)
	if err := os.MkdirAll(tr.Cfg.SandboxRecords, 0o755); err != nil {
		t.Fatal(err)
	}
	return tr
}

// registryReply answers the programs a registry prune calls.
func registryReply(installed, pods string) func(command) result {
	return func(c command) result {
		switch {
		case c.name == "gt" && hasArg(c.args, "version"):
			return result{stdout: "gt version (dev: " + installed + ")\n"}
		case c.name == "kubectl":
			return result{stdout: pods}
		case c.name == "pgrep":
			// pgrep's status 1: no crane push is running.
			return result{exit: 1}
		}
		return result{}
	}
}

// TestPruneRegistryOffDoesNothing is the switch's first position: off issues no
// request at all, so a town can turn retention off without a registry in reach.
func TestPruneRegistryOffDoesNothing(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(devTag(), registryTagAt(1))
	tr := newRegistryRun(t, reg, tagName(1))
	tr.Cfg.RegistryPrune = registryPruneOff

	tr.PruneRegistry()

	if got := reg.requested(); len(got) != 0 {
		t.Fatalf("POLECAT_IMAGE_REGISTRY_PRUNE=off made %v, want no request", got)
	}
	tr.wantLogged(t, "POLECAT_IMAGE_REGISTRY_PRUNE=off")
}

// TestPruneRegistryDefaultsToDryRun pins the default: a run that names no mode
// plans the prune and says what it would delete, and sends no DELETE.
func TestPruneRegistryDefaultsToDryRun(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(devTag(),
		registryTagAt(1), registryTagAt(2), registryTagAt(3),
		registryTagAt(4), registryTagAt(5), registryTagAt(6),
		registryTagAt(7))
	tr := newRegistryRun(t, reg, tagName(7))
	tr.Cfg.RegistryKeep = "" // the knob's own default, 5
	tr.Cfg.RegistryPrune = ""

	tr.PruneRegistry()

	for _, request := range reg.requested() {
		if strings.HasPrefix(request, "DELETE ") {
			t.Fatalf("the default mode deleted: %v", reg.requested())
		}
	}
	// Seven candidates and the default keep of five leave the two oldest.
	tr.wantLogged(t, "would delete gastown-polecat@"+registryDigest(tagName(1)))
	tr.wantLogged(t, "would delete gastown-polecat@"+registryDigest(tagName(2)))
	tr.wantLogged(t, "dry run, 2 manifest(s) would be deleted")
}

// TestPruneRegistryApplyDeletes is the apply path: the plan's manifests are
// deleted, garbage collect follows under the run's own docker, and the ledger
// records the run.
func TestPruneRegistryApplyDeletes(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(devTag(),
		registryTagAt(1), registryTagAt(2), registryTagAt(3))
	tr := newRegistryRun(t, reg, tagName(3))
	tr.Cfg.RegistryPrune = registryPruneApply

	tr.PruneRegistry()

	want := []string{registryDigest(tagName(1)), registryDigest(tagName(2))}
	if got := reg.deleted(); !slices.Equal(got, want) {
		t.Fatalf("deleted %v, want the two oldest digests %v", got, want)
	}
	// The collect's container timeout follows the deadline the retention runs under
	// (registryPruneBudget), less the grace, so the argv is pinned in shape and the
	// timeout as a bound: just under the budget, never the registry package's ceiling.
	var gc []string
	for _, c := range tr.callsTo("docker") {
		if len(c.args) == 8 && c.args[0] == "exec" && c.args[4] == "registry" && c.args[5] == "garbage-collect" {
			gc = c.args
		}
	}
	if gc == nil {
		t.Fatalf("no docker garbage-collect call; calls: %v", callLines(tr, "docker"))
	}
	if gc[1] != registryContainer || gc[2] != "timeout" || gc[6] != "--delete-untagged" || gc[7] != "/etc/docker/registry/config.yml" {
		t.Errorf("garbage-collect argv = %q, want the registry container, timeout, and the fixed collect command", gc)
	}
	budget := int(registryPruneBudget / time.Second)
	if secs, err := strconv.Atoi(gc[3]); err != nil || secs < budget-60 || secs >= budget {
		t.Errorf("garbage-collect timeout = %q, want a number of seconds just under the %d-second budget", gc[3], budget)
	}
	tr.wantLogged(t, "deleted 2 of 2 manifest(s)")

	entries, err := diskmaint.NewLedger(tr.Cfg.RegistryLedger, registryLedgerMax).Tail(1)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	if len(entries) != 1 || entries[0].Target != "registry" || entries[0].Done != 2 {
		t.Fatalf("ledger = %+v, want one registry entry with 2 deletes", entries)
	}
}

// TestPruneRegistryNeedsTheRunsLock is the lock rule: a run that does not hold
// the plugin lock leaves the registry alone, so a delete cannot race a push.
func TestPruneRegistryNeedsTheRunsLock(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(devTag(), registryTagAt(1), registryTagAt(2))
	tr := newRegistryRun(t, reg, tagName(2))
	tr.HaveLock = false
	tr.Cfg.RegistryPrune = registryPruneApply

	tr.PruneRegistry()

	if got := reg.requested(); len(got) != 0 {
		t.Fatalf("a run without the lock made %v, want no request", got)
	}
	tr.wantLogged(t, "does not hold the plugin lock")
}

// TestPruneRegistryWithoutRegistryConfiguredSkips is a town that publishes
// nowhere: there is no registry to prune, which is not a fault.
func TestPruneRegistryWithoutRegistryConfiguredSkips(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(devTag())
	tr := newRegistryRun(t, reg, tagName(1))
	tr.Cfg.Registry = ""

	tr.PruneRegistry()

	if got := reg.requested(); len(got) != 0 {
		t.Fatalf("a run with no registry made %v, want no request", got)
	}
	tr.wantLogged(t, "no registry is configured")
}

// TestPruneRegistryBadKeepStopsThisPruneOnly pins the knob's blast radius: a
// count that is not a number prunes nothing and names the variable, rather than
// reading as "keep nothing" and untagging what a sandbox runs.
func TestPruneRegistryBadKeepStopsThisPruneOnly(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(devTag(), registryTagAt(1), registryTagAt(2))
	tr := newRegistryRun(t, reg, tagName(2))
	tr.Cfg.RegistryKeep = "abc"
	tr.Cfg.RegistryPrune = registryPruneApply

	tr.PruneRegistry()

	if got := reg.requested(); len(got) != 0 {
		t.Fatalf("a keep count of abc made %v, want no request", got)
	}
	tr.wantLogged(t, "POLECAT_IMAGE_REGISTRY_KEEP=abc is not a whole number")
}

// TestPruneRegistryRefusesAnUnknownMode keeps a typo from reading as apply: a
// mode outside the three names prunes nothing.
func TestPruneRegistryRefusesAnUnknownMode(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(devTag(), registryTagAt(1), registryTagAt(2))
	tr := newRegistryRun(t, reg, tagName(2))
	tr.Cfg.RegistryPrune = "yes"

	tr.PruneRegistry()

	if got := reg.requested(); len(got) != 0 {
		t.Fatalf("mode yes made %v, want no request", got)
	}
	tr.wantLogged(t, "POLECAT_IMAGE_REGISTRY_PRUNE=yes is not off, dry-run or apply")
}

// TestPruneRegistryReadsTheInstalledCommitFromGTVersion pins the one source of
// the installed tag: the tag gt version names is kept whatever the keep window
// says, and a gt version that names no commit refuses the plan.
func TestPruneRegistryReadsTheInstalledCommitFromGTVersion(t *testing.T) {
	t.Parallel()
	tags := []registryTag{devTag(),
		registryTagAt(1), registryTagAt(2), registryTagAt(3)}

	t.Run("the installed tag is kept", func(t *testing.T) {
		t.Parallel()
		reg := newFakeRegistry(tags...)
		tr := newRegistryRun(t, reg, tagName(2))
		tr.Cfg.RegistryKeep = "0"
		tr.Cfg.RegistryPrune = registryPruneApply

		tr.PruneRegistry()

		for _, digest := range reg.deleted() {
			if digest == registryDigest(tagName(2)) {
				t.Fatalf("deleted the installed commit's digest; deleted %v", reg.deleted())
			}
		}
		if got := reg.deleted(); len(got) != 2 {
			t.Fatalf("deleted %v, want the two tags beside dev and the installed one", got)
		}
	})

	t.Run("an unreadable gt version refuses the plan", func(t *testing.T) {
		t.Parallel()
		reg := newFakeRegistry(tags...)
		tr := newRegistryRun(t, reg, "")
		tr.Cfg.RegistryPrune = registryPruneApply

		tr.PruneRegistry()

		if got := reg.deleted(); len(got) != 0 {
			t.Fatalf("deleted %v with no installed commit, want nothing", got)
		}
		tr.wantLogged(t, "the installed commit is unknown (gt version reported none)")
	})

	t.Run("a gt version that exits nonzero refuses the plan", func(t *testing.T) {
		t.Parallel()
		reg := newFakeRegistry(tags...)
		tr := newRegistryRun(t, reg, tagName(2))
		tr.Cfg.RegistryPrune = registryPruneApply
		answer := tr.reply
		tr.reply = func(c command) result {
			if c.name == "gt" {
				return result{exit: 1, stderr: "gt: panic: secret\n"}
			}
			return answer(c)
		}

		tr.PruneRegistry()

		if got := reg.deleted(); len(got) != 0 {
			t.Fatalf("deleted %v with a failing gt version, want nothing", got)
		}
		tr.wantLogged(t, "installed: gt version exited 1")
		if strings.Contains(tr.output(), "secret") {
			t.Fatal("the refusal echoed gt version's stderr")
		}
	})
}

// TestPruneRegistryRefusesWhatItCannotRead is the fail-closed half: an in-use
// read that fails, and the records directory a town that runs sandboxes has,
// both refuse the plan rather than letting it delete a running image.
func TestPruneRegistryRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		wreck func(*testRun)
		want  string
	}{
		{
			name: "kubectl fails",
			wreck: func(tr *testRun) {
				tr.reply = func(c command) result {
					if c.name == "kubectl" {
						return result{exit: 1, stderr: "connection refused\n"}
					}
					return registryReply(tagName(3), "")(c)
				}
			},
			want: "kubectl",
		},
		{
			name: "the records directory is missing",
			wreck: func(tr *testRun) {
				if err := os.RemoveAll(tr.Cfg.SandboxRecords); err != nil {
					t.Fatal(err)
				}
			},
			want: "sandbox records",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := newFakeRegistry(devTag(),
				registryTagAt(1), registryTagAt(2), registryTagAt(3))
			tr := newRegistryRun(t, reg, tagName(3))
			tr.Cfg.RegistryPrune = registryPruneApply
			tc.wreck(tr)

			tr.PruneRegistry()

			if got := reg.deleted(); len(got) != 0 {
				t.Fatalf("deleted %v after a failed read, want nothing", got)
			}
			tr.wantLogged(t, "retention refused")
			tr.wantLogged(t, tc.want)
		})
	}
}

// TestProgramCallsPruneRegistryAfterTrims pins the order the run's steps take:
// the registry retention reads the registry only after the tag, build-cache and
// cache-volume trims have run, still under the lock the trims took.
func TestProgramCallsPruneRegistryAfterTrims(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(devTag(), registryTagAt(1), registryTagAt(2))
	tr := newTestRun(t)
	tr.Cfg.Registry = "registry.example:5000"
	tr.Cfg.RegistryKeep = "5"
	tr.web = &http.Client{Transport: roundTrip(reg.handler)}
	if err := os.MkdirAll(tr.Cfg.SandboxRecords, 0o755); err != nil {
		t.Fatal(err)
	}
	order := &[]string{}
	reg.trace = order
	reply := newRunReply(tagName(1))
	reply.label = "9999999"
	reply.pgrep = result{exit: 0} // a sling is running: the run defers, and the trims still follow
	base := programAnswer(t, tr, reply)
	tr.reply = func(c command) result {
		*order = append(*order, c.name+" "+strings.Join(c.args, " "))
		if c.name == "kubectl" {
			return result{stdout: `{"items":[]}`}
		}
		return base(c)
	}
	t.Cleanup(tr.Cleanup)

	if err := tr.Program(); err != nil {
		t.Fatalf("Program() failed: %v", err)
	}

	lastTrim, firstRegistry := -1, -1
	for i, call := range *order {
		switch {
		case call == "docker volume ls --format {{.Name}} --filter name=^gt-cache-":
			lastTrim = i
		case strings.HasPrefix(call, "GET /v2/"):
			if firstRegistry < 0 {
				firstRegistry = i
			}
		}
	}
	if lastTrim < 0 {
		t.Fatalf("the cache-volume trim never ran: %v", *order)
	}
	if firstRegistry < 0 {
		t.Fatalf("the registry retention read nothing: %v", *order)
	}
	if firstRegistry < lastTrim {
		t.Fatalf("the registry was read at %d, before the cache trim at %d: %v", firstRegistry, lastTrim, *order)
	}
	tr.wantCall(t, "docker", "builder", "prune", "--keep-storage", "8GB", "-f")
	tr.wantLogged(t, "deferred: sandbox sling in progress")
}

// TestPruneRegistryNeverFailsTheRun pins the blast radius of the whole step: a
// registry the run cannot reach is logged and the run still succeeds, so a
// retention problem never leaves the image unpushed.
func TestPruneRegistryNeverFailsTheRun(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.Registry = "registry.example:5000"
	tr.Cfg.RegistryPrune = registryPruneApply
	tr.web = &http.Client{Transport: failingRoundTrip{err: fmt.Errorf("connect: connection refused")}}
	if err := os.MkdirAll(tr.Cfg.SandboxRecords, 0o755); err != nil {
		t.Fatal(err)
	}
	reply := newRunReply(tagName(1))
	reply.label = "9999999"
	reply.pgrep = result{exit: 0}
	tr.reply = programAnswer(t, tr, reply)
	t.Cleanup(tr.Cleanup)

	if err := tr.Program(); err != nil {
		t.Fatalf("Program() = %v, want the run to succeed despite the registry", err)
	}
	tr.wantLogged(t, "retention refused")
	tr.wantLogged(t, "connection refused")
}

// TestRegistryRunnerCarriesAValuedEnvironment is the retention's command seam:
// an environment entry with a value reaches the command, and an entry with no
// value is dropped, so the command inherits this process's value for it.
func TestRegistryRunnerCarriesAValuedEnvironment(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = func(c command) result {
		if c.name == "kubectl" {
			return result{stdout: `{"items":[]}`}
		}
		return result{}
	}
	run := tr.registryRunner()

	out, err := run(context.Background(), []string{"DOCKER_HOST=unix:///run/colima.sock", "EMPTY="}, "kubectl", "get", "pods", "-A", "-o", "json")
	if err != nil || string(out) != `{"items":[]}` {
		t.Fatalf("run = %q, %v; want the program's output", out, err)
	}
	calls := tr.callsTo("kubectl")
	if len(calls) != 1 {
		t.Fatalf("kubectl ran %d times, want 1", len(calls))
	}
	if want := []string{"DOCKER_HOST=unix:///run/colima.sock"}; !slices.Equal(calls[0].env, want) {
		t.Fatalf("the command's environment is %v, want %v", calls[0].env, want)
	}
}

// TestRegistryRunnerSkipsACancelledCommand: a context that is already done
// never starts the program.
func TestRegistryRunnerSkipsACancelledCommand(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	run := tr.registryRunner()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := run(cancelled, nil, "kubectl", "get", "pods"); err == nil {
		t.Fatal("a cancelled run returned no error")
	}
	if calls := tr.callsTo("kubectl"); len(calls) != 0 {
		t.Fatalf("kubectl ran %d time(s) under a cancelled context: %v", len(calls), callLines(tr, "kubectl"))
	}
}

// TestPruneRegistryBoundsEveryCommandWithTheBudget: every command an apply runs
// carries a deadline, so none can hold the plugin lock past the budget. docker
// is the one exception, as its own test pins: once garbage collect starts, it
// is not cut short.
func TestPruneRegistryBoundsEveryCommandWithTheBudget(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(devTag(),
		registryTagAt(1), registryTagAt(2), registryTagAt(3))
	tr := newRegistryRun(t, reg, tagName(3))
	tr.Cfg.RegistryPrune = registryPruneApply

	tr.PruneRegistry()

	if len(tr.calls) == 0 {
		t.Fatal("the retention ran no command, want the installed-commit and in-use reads")
	}
	for _, c := range tr.calls {
		if c.name == "docker" {
			continue
		}
		if c.ctx == nil {
			t.Fatalf("%s ran with no context, so no budget bounds it", c.name)
		}
		if _, ok := c.ctx.Deadline(); !ok {
			t.Fatalf("%s ran with a context that has no deadline", c.name)
		}
	}
}

// cancellableBudget is a retention budget the test ends by hand, so the budget
// expires at a point the test chooses rather than on a clock. cancel is set by
// the budget when the retention starts.
type cancellableBudget struct {
	cancel context.CancelFunc
}

func (b *cancellableBudget) start() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	return ctx, cancel
}

// TestPruneRegistryBudgetEndingBeforeCollectSkipsIt: a budget spent on the
// deletes leaves no collect to start, so nothing runs beside a push that got in
// once the lock is gone.
func TestPruneRegistryBudgetEndingBeforeCollectSkipsIt(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(devTag(),
		registryTagAt(1), registryTagAt(2), registryTagAt(3))
	tr := newRegistryRun(t, reg, tagName(3))
	tr.Cfg.RegistryPrune = registryPruneApply
	budget := &cancellableBudget{}
	tr.budget = budget.start
	// The first delete spends the budget, so the collect that follows starts
	// with it already gone.
	tr.web = &http.Client{Transport: roundTrip(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			budget.cancel()
		}
		reg.handler(w, r)
	})}

	tr.PruneRegistry()

	if calls := tr.callsTo("docker"); len(calls) != 0 {
		t.Fatalf("garbage collect started after the budget ended: %v", callLines(tr, "docker"))
	}
	entries, err := diskmaint.NewLedger(tr.Cfg.RegistryLedger, registryLedgerMax).Tail(1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("read the ledger: %v (%d entries)", err, len(entries))
	}
	if !slices.ContainsFunc(entries[0].Errors, func(e string) bool { return strings.Contains(e, "collect:") }) {
		t.Fatalf("ledger errors %v, want the skipped collect named", entries[0].Errors)
	}
}

// TestPruneRegistryBudgetEndingDuringCollectDoesNotCutItShort: a budget that
// ends while garbage collect runs must not end the collect. The docker client
// would die and the in-container collect would keep running after the lock is
// released, so the run waits for the collect to finish.
func TestPruneRegistryBudgetEndingDuringCollectDoesNotCutItShort(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(devTag(),
		registryTagAt(1), registryTagAt(2), registryTagAt(3))
	tr := newRegistryRun(t, reg, tagName(3))
	tr.Cfg.RegistryPrune = registryPruneApply
	budget := &cancellableBudget{}
	tr.budget = budget.start
	answer := tr.reply
	var collectCtxErr error
	tr.reply = func(c command) result {
		if c.name == "docker" {
			budget.cancel()
			collectCtxErr = c.ctx.Err()
			return result{}
		}
		return answer(c)
	}

	tr.PruneRegistry()

	if len(tr.callsTo("docker")) != 1 {
		t.Fatalf("docker ran %d time(s), want the one garbage collect", len(tr.callsTo("docker")))
	}
	if collectCtxErr != nil {
		t.Fatalf("the budget ending cancelled garbage collect: %v", collectCtxErr)
	}
	entries, err := diskmaint.NewLedger(tr.Cfg.RegistryLedger, registryLedgerMax).Tail(1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("read the ledger: %v (%d entries)", err, len(entries))
	}
	if len(entries[0].Errors) != 0 {
		t.Fatalf("ledger errors %v, want the collect to have run clean", entries[0].Errors)
	}
}

// TestPruneRegistryHungCollectReturnsAtTheBudget: a docker client that never
// answers holds the plugin lock only as long as the retention's budget. The
// budget ends the wait for the client, and the collect that client started
// carries a container timeout shorter than the time the budget left, so the
// collect is over inside the container by the time the retention returns
// (gt-eg14z).
func TestPruneRegistryHungCollectReturnsAtTheBudget(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(devTag(),
		registryTagAt(1), registryTagAt(2), registryTagAt(3))
	tr := newRegistryRun(t, reg, tagName(3))
	tr.Cfg.RegistryPrune = registryPruneApply
	// A budget short enough for the test to reach its end, with enough left
	// after registry.collectGrace for the collect to have a whole second.
	const budget = 3 * time.Second
	var deadline time.Time
	tr.budget = func() (context.Context, context.CancelFunc) {
		deadline = time.Now().Add(budget)
		return context.WithDeadline(context.Background(), deadline)
	}
	// The collect answers only when the test ends, so only the budget can end
	// the wait for it.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	hung := make(chan []string, 1)
	var left time.Duration
	answer := tr.reply
	tr.reply = func(c command) result {
		if c.name != "docker" {
			return answer(c)
		}
		left = time.Until(deadline)
		hung <- c.args
		<-release
		return result{}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.PruneRegistry()
	}()
	// The bound on the wait, so a run that never returns fails the test instead
	// of hanging it.
	bound, cancel := context.WithTimeout(context.Background(), budget+5*time.Second)
	defer cancel()
	select {
	case <-done:
	case <-bound.Done():
		t.Fatal("PruneRegistry did not return at its budget; a collect that never answered held it")
	}

	var gc []string
	select {
	case gc = <-hung:
	default:
		t.Fatalf("no garbage collect started; calls: %v", callLines(tr, "docker"))
	}
	if len(gc) != 8 {
		t.Fatalf("garbage-collect argv = %q, want the registry collect command", gc)
	}
	secs, err := strconv.Atoi(gc[3])
	if err != nil || secs < 1 {
		t.Fatalf("garbage-collect timeout = %q, want a whole number of seconds", gc[3])
	}
	if whole := int(left / time.Second); secs >= whole {
		t.Fatalf("garbage-collect timeout = %ds with %v left on the budget, want a timeout shorter than the time left so the container ends the collect first", secs, left)
	}
	entries, err := diskmaint.NewLedger(tr.Cfg.RegistryLedger, registryLedgerMax).Tail(1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("read the ledger: %v (%d entries)", err, len(entries))
	}
	if !slices.ContainsFunc(entries[0].Errors, func(e string) bool {
		return strings.Contains(e, "no answer by the caller's deadline")
	}) {
		t.Fatalf("ledger errors %v, want the abandoned collect named", entries[0].Errors)
	}
}

// TestPruneRegistryHoldsCollectWhileAPushRuns is the push gate: a crane push on
// this host stops garbage collect, while the deletes the plan made still apply
// under the lock.
func TestPruneRegistryHoldsCollectWhileAPushRuns(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(devTag(),
		registryTagAt(1), registryTagAt(2), registryTagAt(3))
	tr := newRegistryRun(t, reg, tagName(3))
	tr.Cfg.RegistryPrune = registryPruneApply
	answer := tr.reply
	tr.reply = func(c command) result {
		if c.name == "pgrep" {
			return result{exit: 0}
		}
		return answer(c)
	}

	tr.PruneRegistry()

	if got := reg.deleted(); len(got) != 2 {
		t.Fatalf("deleted %v, want the two plan digests", got)
	}
	if calls := tr.callsTo("docker"); len(calls) != 0 {
		t.Fatalf("garbage collect ran beside a push: %v", callLines(tr, "docker"))
	}
	entries, err := diskmaint.NewLedger(tr.Cfg.RegistryLedger, registryLedgerMax).Tail(1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("read the ledger: %v (%d entries)", err, len(entries))
	}
	if !slices.ContainsFunc(entries[0].Errors, func(e string) bool { return strings.Contains(e, "a push is in flight") }) {
		t.Fatalf("ledger errors %v, want the collect's refusal named", entries[0].Errors)
	}
}

// TestPruneRegistryCollectSendsNoDockerHost: garbage collect's empty DOCKER_HOST
// is the entry the target always sends, and it is dropped, so docker runs with
// this process's own environment, the daemon the pushes went to.
func TestPruneRegistryCollectSendsNoDockerHost(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(devTag(),
		registryTagAt(1), registryTagAt(2), registryTagAt(3))
	tr := newRegistryRun(t, reg, tagName(3))
	tr.Cfg.RegistryPrune = registryPruneApply

	tr.PruneRegistry()

	calls := tr.callsTo("docker")
	if len(calls) != 1 {
		t.Fatalf("docker ran %d times, want the one garbage collect: %v", len(calls), callLines(tr, "docker"))
	}
	if len(calls[0].env) != 0 {
		t.Fatalf("garbage collect carried environment %v, want none", calls[0].env)
	}
}

// TestRegistryPushInFlightFailsClosed: pgrep's status 1 means no push; 0 means a
// push; any other status is a probe that did not answer, and reads as a push so
// garbage collect refuses.
func TestRegistryPushInFlightFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		exit int
		want bool
	}{
		{"nothing matched", 1, false},
		{"a push matched", 0, true},
		{"pgrep failed", 2, true},
		{"pgrep did not start", 127, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.reply = func(command) result { return result{exit: tc.exit} }

			if got := tr.registryPushInFlight(context.Background()); got != tc.want {
				t.Fatalf("registryPushInFlight() = %v at pgrep status %d, want %v", got, tc.exit, tc.want)
			}
			if c := tr.calls[0]; c.name != tr.Cfg.Pgrep || !strings.Contains(strings.Join(c.args, " "), "crane push") {
				t.Fatalf("the push check ran %s %v, want pgrep for a crane push", c.name, c.args)
			}
		})
	}
}

// TestRegistryWebClientHasARequestTimeout: a registry that stops answering must
// not hold the plugin lock, so the default client carries a timeout.
func TestRegistryWebClientHasARequestTimeout(t *testing.T) {
	t.Parallel()
	if (&Run{}).webClient().Timeout <= 0 {
		t.Fatal("the registry client has no request timeout")
	}
}

// TestPluginDocNamesTheRetentionKnobs: the plugin contract is what an operator
// reads to set the retention, so its section names both variables and their
// defaults.
func TestPluginDocNamesTheRetentionKnobs(t *testing.T) {
	t.Parallel()
	doc, err := os.ReadFile(filepath.Join("..", "..", "plugins", "polecat-image", "plugin.md"))
	if err != nil {
		t.Fatalf("read the plugin contract: %v", err)
	}
	_, section, found := strings.Cut(string(doc), "## Registry retention\n")
	if !found {
		t.Fatal("the plugin contract has no Registry retention section")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	for _, want := range []string{
		"POLECAT_IMAGE_REGISTRY_KEEP",
		"(default `5`)",
		"POLECAT_IMAGE_REGISTRY_PRUNE",
		"`dry-run` (the default",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the Registry retention section does not contain %q", want)
		}
	}
}
