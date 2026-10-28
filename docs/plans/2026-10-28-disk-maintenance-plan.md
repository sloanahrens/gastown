> Status: plan approved by the operator (2026-10-28). Design: `2026-10-28-disk-maintenance-design.md`. Tracked in gt-i7hxu.

# Town Disk Maintenance Implementation Plan

> **For agentic workers:** every task below is written to be filed as exactly one Gas Town bead (twenty minutes of one polecat's work). Steps use checkbox syntax for tracking. Read the design first.

**Goal:** a tested Go pruner, `internal/diskmaint`, whose first target deletes old `gastown-polecat` images from the sandbox registry by digest, with a disk watch, a daily patrol, a `gt maint` command, and a hook in the `polecat-image` plugin.

**Architecture:** one small package defines `Target` (Measure, Plan, Apply), a plan type, and a JSONL ledger. A `registry` subpackage implements the first target against the registry's HTTP API. Three entry points call the same code: the `polecat-image` plugin (after its trims, under its lock), a daemon patrol (daily, quiet window), and `gt maint plan|apply`. Dry-run is the default everywhere.

**Tech Stack:** Go (standard library `net/http`, `httptest`, `encoding/json`); no new dependencies, no new shell scripts. The only shell edit is `deploy/k8s/kind/up.sh` and its test, which already exist.

**Spec:** `docs/plans/2026-10-28-disk-maintenance-design.md`.

## Global Constraints

- Go with unit tests; **no new shell scripts**. Edit `deploy/k8s/kind/up.sh` and `deploy/k8s/kind/kind_test.sh` only where Task 11 says.
- Dry-run is the default. An unreadable input gives an **empty plan with `Refused` set**, never a guess.
- Registry deletes act on a **digest** and remove every tag that points at it. Never delete a digest that a kept tag shares.
- `internal/daemon`, `internal/cmd`, `internal/polecat` and `internal/beads` are container-suite packages: acceptance commands name a narrow `-run` filter, never the whole package.
- No `time.Sleep`, `time.After` or `t.Setenv` in unit tests; inject clocks, runners and HTTP bases.
- Commit authorship is the agent's own name; never the operator's name in code, comments, docs or fixtures (use placeholder names such as `repo-a`, `host-a`).
- The patrol's config key is read at daemon boot: Task 10 ships the code, and the key goes into the live config only after an installed `gt` knows it.

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `internal/diskmaint/types.go` | `Target`, `Plan`, `Action`, `Kept`, `Measurement`, `Result`, `Failure`, `Policy`, `Runner` | 1 |
| `internal/diskmaint/ledger.go` | JSONL ledger: append, tail, size rotation | 1 |
| `internal/diskmaint/registry/client.go` | registry v2 HTTP client | 2 |
| `internal/diskmaint/registry/plan.go` | pure planner `BuildPlan` | 3 |
| `internal/diskmaint/registry/inuse.go` | in-use digests and repos from pods and sandbox records | 4 |
| `internal/diskmaint/registry/target.go` | `Target`: `Measure` and `Plan` assembly | 5 |
| `internal/diskmaint/registry/apply.go` | `Apply`: deletes and garbage collect | 6 |
| `internal/polecatimage/registryprune.go` | `PruneRegistry` hook and config | 7 |
| `internal/cmd/maint.go` | `gt maint plan|apply` | 8 |
| `internal/diskmaint/disks/disks.go` | `df` parsing, 80%/70% alert logic, keep policy | 9 |
| `internal/daemon/disk_maintenance.go` | patrol, config, alerts | 10 |
| `deploy/k8s/kind/up.sh`, `kind_test.sh` | named registry volume and delete flag | 11 |
| `docs/guides/disk-maintenance.md`, `docs/reference.md` | operator guide and config rows | 12 |

## Shared interfaces (every task uses these names verbatim)

```go
// package diskmaint (internal/diskmaint)

// Runner runs one program and returns its stdout. env is appended to the
// process environment. A non-zero exit is an error that carries stderr.
type Runner func(ctx context.Context, env []string, name string, args ...string) ([]byte, error)

type Measurement struct {
	Used, Capacity int64  // bytes; Capacity 0 means unknown
	Detail         string // one human line
}

type Action struct {
	Kind  string // "delete-manifest"
	Ref   string // "repo@sha256:..."
	Bytes int64  // best-effort size, 0 if unknown
	Why   string
}

type Kept struct{ Ref, Rule string } // Ref is "repo:tag" or "repo@digest"

type Plan struct {
	Target  string
	Actions []Action
	Kept    []Kept
	Refused string // non-empty: the plan is empty on purpose, and why
}

type Failure struct {
	Action Action
	Err    string
}

type Result struct {
	Target     string
	Done       []Action
	Failed     []Failure
	BytesFreed int64
	Collected  bool // registry garbage collect ran
}

type Policy struct {
	Keep   int  // newest per-commit tags to keep
	Apply  bool // false is a dry run
	Now    time.Time
}

type Target interface {
	Name() string
	Measure(ctx context.Context) (Measurement, error)
	Plan(ctx context.Context, p Policy) (Plan, error)
	Apply(ctx context.Context, plan Plan) (Result, error)
}

type LedgerEntry struct {
	TS         time.Time `json:"ts"`
	Target     string    `json:"target"`
	Mode       string    `json:"mode"` // "dry-run" | "apply"
	Planned    int       `json:"planned"`
	Done       int       `json:"done"`
	BytesFreed int64     `json:"bytes_freed"`
	Refused    string    `json:"refused,omitempty"`
	Errors     []string  `json:"errors,omitempty"`
}

type Ledger struct{ /* path, maxBytes */ }
func NewLedger(path string, maxBytes int64) *Ledger
func (l *Ledger) Append(e LedgerEntry) error
func (l *Ledger) Tail(n int) ([]LedgerEntry, error)
```

```go
// package registry (internal/diskmaint/registry)
const RepoMain = "gastown-polecat"
var StrayRepos = []string{"gastown-polecat-measure", "seedtest/polecat"}
var ErrDeletesDisabled = errors.New("registry deletes are disabled (HTTP 405)")

type Manifest struct {
	Digest  string
	Created time.Time
	Bytes   int64
}
type Client struct {
	Base string // "http://localhost:5001"
	HTTP *http.Client
}
func (c *Client) Catalog(ctx context.Context) ([]string, error)
func (c *Client) Tags(ctx context.Context, repo string) ([]string, error)
func (c *Client) GetManifest(ctx context.Context, repo, ref string) (Manifest, error)
func (c *Client) DeleteManifest(ctx context.Context, repo, digest string) error

type TagInfo struct {
	Repo, Tag, Digest string
	Created           time.Time
	Bytes             int64
}
type PlanInput struct {
	Repos           map[string][]TagInfo // repo -> its tags
	Keep            int
	InstalledCommit string          // full sha from gt version; matched by tag prefix
	InUseDigests    map[string]bool // digests running pods use
	InUseRepos      map[string]bool // repos named by sandbox records
	Errs            []string        // any failed read; non-empty means refuse
}
func BuildPlan(in PlanInput) diskmaint.Plan

type InUse struct {
	Digests map[string]bool
	Repos   map[string]bool
}
func ReadInUse(ctx context.Context, run diskmaint.Runner, kubeconfig, recordsDir string) (InUse, error)

type Config struct {
	Base, Kubeconfig, RecordsDir string
	DockerHost, Container        string // GC runs "docker exec <Container> ..." with DOCKER_HOST=<DockerHost>
	Keep                         int
	Installed                    func() (string, error) // full commit of the installed gt
	PushInFlight                 func() bool
	Run                          diskmaint.Runner
	HTTP                         *http.Client
	Ledger                       *diskmaint.Ledger
}
func NewTarget(cfg Config) *Target // *Target implements diskmaint.Target, Name() == "registry"
```

---

## Slice 1: the registry

### Task 1: diskmaint core types and ledger

**Files:**
- Create: `internal/diskmaint/types.go`, `internal/diskmaint/ledger.go`
- Test: `internal/diskmaint/ledger_test.go`

**Depends on:** nothing.
**Interfaces:** Produces everything in "Shared interfaces / package diskmaint".

- [ ] **Step 1: Write the failing tests** in `ledger_test.go`:

```go
func TestLedgerAppendAndTail(t *testing.T) {
	l := NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), 1<<20)
	for i := 0; i < 5; i++ {
		if err := l.Append(LedgerEntry{TS: time.Unix(int64(i), 0).UTC(), Target: "registry", Mode: "dry-run", Planned: i}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.Tail(3)
	if err != nil || len(got) != 3 || got[0].Planned != 2 || got[2].Planned != 4 {
		t.Fatalf("tail = %+v, %v; want entries 2..4 in order", got, err)
	}
}

func TestLedgerRotatesAtMaxBytes(t *testing.T) { /* append until the file passes maxBytes; the live file is
	smaller than maxBytes afterwards, ledger.jsonl.1 exists, and Tail still returns the newest entries */ }

func TestLedgerTailOfMissingFileIsEmpty(t *testing.T) { /* no file: nil slice, nil error */ }

func TestLedgerSkipsTornLine(t *testing.T) { /* a half-written last line is skipped, earlier lines returned */ }
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/diskmaint -run 'TestLedger' -count=1` (undefined names).
- [ ] **Step 3: Implement** the types exactly as in "Shared interfaces", and the ledger: `Append` marshals one JSON line and appends with `O_APPEND`; when the file would pass `maxBytes` it renames the file to `<path>.1` (replacing an older `.1`) first; `Tail(n)` reads the live file (and `.1` if fewer than n are found), skips lines that do not decode, returns the last n oldest-first.
- [ ] **Step 4: Run, expect PASS** (same command).
- [ ] **Step 5: Commit** `feat: diskmaint core types and JSONL ledger`.

**Acceptance (bead):**
- [ ] `internal/diskmaint` declares `Target`, `Plan`, `Action`, `Kept`, `Measurement`, `Result`, `Failure`, `Policy`, `Runner`, `LedgerEntry` with the exact fields in the plan
- [ ] `go test ./internal/diskmaint -run 'TestLedger' -count=1` passes
- [ ] a torn last line and a missing file are tested and do not error
- [ ] rotation to `<path>.1` is tested

### Task 2: registry HTTP client

**Files:**
- Create: `internal/diskmaint/registry/client.go`
- Test: `internal/diskmaint/registry/client_test.go`

**Depends on:** Task 1 (package exists; no type use beyond the standard library).
**Interfaces:** Produces `Client`, `Manifest`, `ErrDeletesDisabled` and the four methods.

- [ ] **Step 1: Write failing tests** against an `httptest.Server`:

```go
func TestCatalogAndTags(t *testing.T)            // /v2/_catalog and /v2/<repo>/tags/list decode; a null "tags" is an empty slice
func TestGetManifestImage(t *testing.T)           // image manifest: Digest from Docker-Content-Digest, Created from the config blob's "created", Bytes = config + layers sizes
func TestGetManifestIndexTakesNewestChild(t *testing.T) // index manifest: newest child's created wins; Digest is the index digest
func TestGetManifestSendsAcceptHeaders(t *testing.T)    // the request asks for oci index, oci manifest and docker v2 manifest
func TestDeleteManifestOK(t *testing.T)           // 202 is success
func TestDeleteManifest405MeansDisabled(t *testing.T)   // errors.Is(err, ErrDeletesDisabled)
func TestDeleteManifest404IsNotFound(t *testing.T)      // returns a distinguishable not-found error
func TestClientErrorsOnNon2xx(t *testing.T)
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/diskmaint/registry -run 'TestCatalog|TestGetManifest|TestDeleteManifest|TestClientErrors' -count=1`.
- [ ] **Step 3: Implement** the client: `GET` with `Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json`; digest from the `Docker-Content-Digest` header; for an image manifest fetch the config blob (`/v2/<repo>/blobs/<digest>`) and read `created`; for an index take the newest child's `created`; `DeleteManifest` sends `DELETE /v2/<repo>/manifests/<digest>`.
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat: diskmaint registry v2 client`.

**Acceptance (bead):**
- [ ] `Client` exposes `Catalog`, `Tags`, `GetManifest`, `DeleteManifest` with the signatures in the plan
- [ ] `go test ./internal/diskmaint/registry -run 'TestCatalog|TestGetManifest|TestDeleteManifest|TestClientErrors' -count=1` passes
- [ ] an index manifest and a 405 delete answer are tested
- [ ] the tests use `httptest`, no network and no sleeps

### Task 3: registry planner (pure)

**Files:**
- Create: `internal/diskmaint/registry/plan.go`
- Test: `internal/diskmaint/registry/plan_test.go`

**Depends on:** Task 1 (`diskmaint.Plan`, `Action`, `Kept`).
**Interfaces:** Produces `TagInfo`, `PlanInput`, `BuildPlan`, `RepoMain`, `StrayRepos`.

The rules (all must be tested):

1. Candidates are tags of `RepoMain` that are hex and 7 to 12 characters. Every other tag (`dev`, any named tag) is never a candidate and its digest is kept (`Rule: "named tag"`).
2. Keep set of digests: the digest of tag `dev`; the digest of the tag that is a prefix of `InstalledCommit`; every digest in `InUseDigests`; the digests of the newest `Keep` candidates by `Created`, ties broken by tag name.
3. A manifest is deleted only if its digest is outside the keep set. A candidate whose digest equals a kept digest is kept (`Rule: "shares a kept digest"`).
4. Stray repos (`StrayRepos`) delete every digest of their tags, but only when the repo is not in `InUseRepos`; otherwise they are kept (`Rule: "named by a sandbox record"`).
5. If `len(Errs) > 0`, or `InstalledCommit` is empty, or `RepoMain` has zero tags, return `Plan{Refused: <reason>}` with no actions.

- [ ] **Step 1: Write failing tests** (table-driven; `fixture` helpers build `TagInfo` lists):

```go
func TestPlanDeletesOldCandidatesBeyondKeep(t *testing.T)     // 8 candidates, Keep 5: the 3 oldest digests are actions
func TestPlanKeepsTagSharingDevDigest(t *testing.T)           // old tag aaa1111 has dev's digest: no action, Kept rule "shares a kept digest"
func TestPlanKeepsInstalledCommitTag(t *testing.T)            // tag is a prefix of InstalledCommit: kept even if the oldest
func TestPlanKeepsInUseDigest(t *testing.T)                   // digest in InUseDigests kept even if it is the oldest
func TestPlanNeverTouchesNonHexTags(t *testing.T)             // "dev", "latest", "build-3": never an action
func TestPlanTwoTagsOneDigestCountOnce(t *testing.T)          // two candidate tags with one digest: one action, not two
func TestPlanStrayRepoDeletedWhenUnreferenced(t *testing.T)
func TestPlanStrayRepoKeptWhenNamedBySandboxRecord(t *testing.T)
func TestPlanRefusesOnAnyReadError(t *testing.T)              // Errs non-empty: Refused set, zero actions
func TestPlanRefusesWithoutInstalledCommit(t *testing.T)
func TestPlanRefusesOnEmptyMainRepo(t *testing.T)
func TestPlanKeepZeroStillKeepsDevAndInstalled(t *testing.T)
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/diskmaint/registry -run 'TestPlan' -count=1`.
- [ ] **Step 3: Implement `BuildPlan`** per the rules above; every `Kept` entry names its rule; `Action.Why` names the age rank.
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Mutation checks (do these before committing; each must turn a named test red, then restore):**
  - remove the "shares a kept digest" guard → `TestPlanKeepsTagSharingDevDigest` fails
  - remove the in-use check → `TestPlanKeepsInUseDigest` fails
  - treat non-hex tags as candidates → `TestPlanNeverTouchesNonHexTags` fails
  - drop the `Errs` refusal → `TestPlanRefusesOnAnyReadError` fails
- [ ] **Step 6: Commit** `feat: registry retention planner by digest`.

**Acceptance (bead):**
- [ ] `BuildPlan` keeps `dev`, the installed commit's tag, in-use digests and the newest `Keep`, and never deletes a digest a kept tag shares
- [ ] `go test ./internal/diskmaint/registry -run 'TestPlan' -count=1` passes
- [ ] the four mutations in the plan each turn a named test red and are restored (list them in the commit message)
- [ ] a failed read or an empty main repo returns a plan with `Refused` set and no actions

### Task 4: in-use provider

**Files:**
- Create: `internal/diskmaint/registry/inuse.go`
- Test: `internal/diskmaint/registry/inuse_test.go`

**Depends on:** Task 1 (`diskmaint.Runner`).
**Interfaces:** Produces `InUse` and `ReadInUse`.

- [ ] **Step 1: Write failing tests** with a fake `Runner` that returns canned `kubectl get pods -A -o json` and a temp records directory:

```go
func TestReadInUseCollectsPodImageIDs(t *testing.T)     // container "imageID" "localhost:5001/gastown-polecat@sha256:abc" gives Digests["sha256:abc"]
func TestReadInUseCollectsReposFromImageRefs(t *testing.T) // image "localhost:5001/seedtest/polecat:x" gives Repos["seedtest/polecat"]
func TestReadInUseReadsSandboxRecords(t *testing.T)     // a record file naming an image adds its repo
func TestReadInUseFailsClosedOnKubectlError(t *testing.T)   // runner error: returns an error, not an empty InUse
func TestReadInUseFailsClosedOnBadJSON(t *testing.T)
func TestReadInUseUnreadableRecordsDirIsError(t *testing.T) // the directory exists but cannot be read
func TestReadInUseMissingRecordsDirIsEmpty(t *testing.T)    // the directory does not exist: no sandboxes, not an error
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/diskmaint/registry -run 'TestReadInUse' -count=1`.
- [ ] **Step 3: Implement:** run `kubectl --kubeconfig <kubeconfig> get pods -A -o json` through `run`; walk `status.containerStatuses[].imageID` and `.image` and `spec.containers[].image` (and init containers); record the digest after `@` and the repo path after the registry host; read each regular file in `recordsDir` as text and add any `host:port/<repo>` reference it contains.
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Mutation check:** make the kubectl error return an empty `InUse` instead of an error → `TestReadInUseFailsClosedOnKubectlError` fails; restore.
- [ ] **Step 6: Commit** `feat: registry in-use digests from pods and sandbox records`.

**Acceptance (bead):**
- [ ] `ReadInUse(ctx, run, kubeconfig, recordsDir)` returns the digests and repos running pods and sandbox records use
- [ ] `go test ./internal/diskmaint/registry -run 'TestReadInUse' -count=1` passes
- [ ] a kubectl error, bad JSON and an unreadable records directory return errors, never an empty set
- [ ] the fail-closed mutation in the plan turns its test red

### Task 5: registry target: Measure and Plan assembly

**Files:**
- Create: `internal/diskmaint/registry/target.go`
- Test: `internal/diskmaint/registry/target_test.go`

**Depends on:** Tasks 2, 3, 4.
**Interfaces:** Consumes `Client`, `BuildPlan`, `ReadInUse`. Produces `Config`, `Target`, `NewTarget`, `(*Target).Name`, `(*Target).Measure`, `(*Target).Plan`. (`Apply` is Task 6; until then `Apply` returns `errors.New("not implemented")` and Task 6 replaces it.)

- [ ] **Step 1: Write failing tests** with an `httptest` registry (two repos, tags with creation times) and fakes:

```go
func TestTargetNameIsRegistry(t *testing.T)
func TestPlanAssemblesInputAndBuildsPlan(t *testing.T)   // lists repos+tags, fetches manifests, calls the in-use fake, installed fake; the result matches BuildPlan on the same data
func TestPlanSkipsManifestFetchWhenTagCountSmall(t *testing.T) // main repo has <= Keep+1 tags: only tags/list is requested (count requests on the server)
func TestPlanRefusesWhenCatalogFails(t *testing.T)
func TestPlanRefusesWhenOneManifestFetchFails(t *testing.T)
func TestPlanRefusesWhenInstalledCommitUnreadable(t *testing.T)
func TestMeasureReportsTagCountAndDetail(t *testing.T)
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/diskmaint/registry -run 'TestTarget|TestPlanAssembles|TestPlanSkips|TestPlanRefuses|TestMeasure' -count=1`.
- [ ] **Step 3: Implement** `Plan`: `Catalog`; for each repo `Tags`; for `RepoMain` and the stray repos fetch each tag's manifest only when the main repo has more than `Keep+1` tags or a stray repo exists; collect any error into `Errs`; call `Installed()` and `ReadInUse`; build `PlanInput` with `Keep` from `Policy.Keep` (falling back to `Config.Keep`); return `BuildPlan`. `Measure` returns the tag count and total manifest bytes as `Detail` (`Used` may be zero).
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat: registry target measure and plan`.

**Acceptance (bead):**
- [ ] `NewTarget(cfg)` returns a value implementing `diskmaint.Target` with `Name() == "registry"`
- [ ] `go test ./internal/diskmaint/registry -run 'TestTarget|TestPlanAssembles|TestPlanSkips|TestPlanRefuses|TestMeasure' -count=1` passes
- [ ] when the main repo has `Keep+1` tags or fewer and there is no stray repo, the test shows only the tags request was made
- [ ] a failed catalog, manifest fetch or installed-commit read gives a refused plan

### Task 6: registry target: Apply and garbage collect

**Files:**
- Modify: `internal/diskmaint/registry/target.go` (remove the stub `Apply`)
- Create: `internal/diskmaint/registry/apply.go`
- Test: `internal/diskmaint/registry/apply_test.go`

**Depends on:** Task 5.
**Interfaces:** Produces `(*Target).Apply(ctx, plan diskmaint.Plan) (diskmaint.Result, error)`. `Action.Ref` is `repo@sha256:...`.

- [ ] **Step 1: Write failing tests** (httptest registry that records deletes; fake `Runner` that records the garbage-collect call):

```go
func TestApplyDeletesEachManifest(t *testing.T)
func TestApplyContinuesAfterOneDeleteFails(t *testing.T)    // the failure is in Result.Failed; the others are Done
func TestApplyRunsGarbageCollectOnlyAfterASuccessfulDelete(t *testing.T)
func TestApplySkipsGarbageCollectWhenEveryDeleteFailed(t *testing.T)
func TestApplyGarbageCollectUsesColimaDockerHost(t *testing.T) // env contains DOCKER_HOST=<cfg.DockerHost>; args: exec <Container> registry garbage-collect --delete-untagged /etc/docker/registry/config.yml
func TestApplySkipsGarbageCollectWhenPushInFlight(t *testing.T) // PushInFlight() true: deletes still happen, collect is skipped and recorded in the ledger
func TestApplyDeletesDisabledStopsAtFirstDelete(t *testing.T)   // ErrDeletesDisabled: no more deletes, no collect, an error line in the ledger
func TestApplyWritesALedgerLine(t *testing.T)
func TestApplyOfRefusedPlanDoesNothing(t *testing.T)
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/diskmaint/registry -run 'TestApply' -count=1`.
- [ ] **Step 3: Implement** `Apply`: return immediately for a plan with `Refused` or no actions; for each action split `repo@digest` and call `DeleteManifest`; on `ErrDeletesDisabled` stop; count `Done`/`Failed` and `BytesFreed`; if `len(Done) > 0` and `!PushInFlight()` run the garbage collect through `Run` with `DOCKER_HOST` set to `Config.DockerHost`; append one `LedgerEntry` (mode `apply`).
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Mutation check:** run garbage collect even when no delete succeeded → `TestApplySkipsGarbageCollectWhenEveryDeleteFailed` fails; restore.
- [ ] **Step 6: Commit** `feat: registry apply with garbage collect`.

**Acceptance (bead):**
- [ ] `Apply` deletes manifests by digest, records failures and continues, and writes one ledger line
- [ ] `go test ./internal/diskmaint/registry -run 'TestApply' -count=1` passes
- [ ] garbage collect runs only when at least one delete succeeded and no push is in flight, with `DOCKER_HOST` from the config
- [ ] the mutation in the plan turns its test red and is restored

### Task 7: `polecat-image` hook

**Files:**
- Create: `internal/polecatimage/registryprune.go`
- Modify: `internal/polecatimage/program.go` (call after `TrimCacheVolumes`), `internal/polecatimage/run.go` (Config fields), `internal/polecatimage/program.go` `ConfigFromEnv` (read the knobs), `plugins/polecat-image/plugin.md` (section)
- Test: `internal/polecatimage/registryprune_test.go`

**Depends on:** Task 6.
**Interfaces:** Consumes `registry.NewTarget`, `registry.Config`. Produces `Config.RegistryKeep string`, `Config.RegistryPrune string`, `(*Run).PruneRegistry()`.

- [ ] **Step 1: Write failing tests** (existing tests use the `Run` runner seam; follow `prune_test.go`):

```go
func TestPruneRegistryOffDoesNothing(t *testing.T)          // POLECAT_IMAGE_REGISTRY_PRUNE=off: no HTTP request, no log line besides "off"
func TestPruneRegistryDefaultsToDryRun(t *testing.T)        // unset: plans, logs the plan, deletes nothing (no DELETE request)
func TestPruneRegistryApplyDeletes(t *testing.T)
func TestPruneRegistryNeedsALock(t *testing.T)              // r.HaveLock false: skips with a logged reason
func TestPruneRegistryWithoutRegistryConfiguredSkips(t *testing.T) // Cfg.Registry empty
func TestPruneRegistryBadKeepStopsThisPruneOnly(t *testing.T)   // "abc": logs why, prune skipped, no error returned
func TestProgramCallsPruneRegistryAfterTrims(t *testing.T)      // order: PruneOldImages, PruneBuildCache, TrimCacheVolumes, PruneRegistry
func TestPruneRegistryNeverFailsTheRun(t *testing.T)            // an error inside it is logged; Program still returns nil
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/polecatimage -run 'TestPruneRegistry|TestProgramCallsPruneRegistry' -count=1`.
- [ ] **Step 3: Implement** `PruneRegistry`: read `Cfg.RegistryPrune` (`off|dry-run|apply`, default `dry-run`) and `Cfg.RegistryKeep` (default `5`, parsed with the existing `KeepCount`); require `r.HaveLock`; build the registry target with `Base = "http://" + Cfg.Registry`; `Installed` reads the commit label of `gastown-polecat:dev`; log each action and the ledger summary; never return an error to `Program`. Add the two `Config` fields, read `POLECAT_IMAGE_REGISTRY_KEEP` and `POLECAT_IMAGE_REGISTRY_PRUNE` in `ConfigFromEnv` as plain strings, and document both in a "Registry retention" section of `plugin.md`.
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat: polecat-image prunes the sandbox registry (dry-run by default)`.

**Acceptance (bead):**
- [ ] `Program` calls `PruneRegistry` after `TrimCacheVolumes`, only while the run holds the lock
- [ ] `go test ./internal/polecatimage -run 'TestPruneRegistry|TestProgramCallsPruneRegistry' -count=1` passes
- [ ] unset `POLECAT_IMAGE_REGISTRY_PRUNE` is a dry run that issues no DELETE request
- [ ] `plugin.md` has a "Registry retention" section naming both variables and their defaults
- [ ] an error inside the prune is logged and does not fail the run

### Task 8: `gt maint plan|apply`

**Files:**
- Create: `internal/cmd/maint.go`
- Test: `internal/cmd/maint_test.go`

**Depends on:** Task 6.
**Interfaces:** Consumes `diskmaint.Target`, `registry.NewTarget`. Produces `maintCmd` (registered under `GroupConfig`), and `func runMaint(out io.Writer, deps maintDeps, mode, target string) error`.

```go
type maintDeps struct {
	Targets func() (map[string]diskmaint.Target, error) // built from the town config in production
	Now     func() time.Time
	Keep    int
}
```

- [ ] **Step 1: Write failing tests** (package `cmd`; names start with `TestMaint` so `-run 'TestMaint'` selects them):

```go
func TestMaintPlanPrintsActionsAndKept(t *testing.T)
func TestMaintPlanNeverApplies(t *testing.T)             // a fake Target records Apply calls: zero
func TestMaintApplyRunsApplyOnThePlan(t *testing.T)
func TestMaintApplyRefusesARefusedPlan(t *testing.T)     // prints the Refused reason, exit error, no Apply call
func TestMaintUnknownTargetListsKnownOnes(t *testing.T)
func TestMaintNoTargetRunsEveryTarget(t *testing.T)
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/cmd -run 'TestMaint' -count=1`.
- [ ] **Step 3: Implement** `runMaint` and a thin cobra command: `gt maint plan [target]`, `gt maint apply [target]`; production `Targets` builds the registry target from `settings/config.json` the way `polecatimage` reads its registry settings (reuse its town-config helper if it is exported; otherwise read `daemon.env` keys through the existing config loader).
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat: gt maint plan and apply`.

**Acceptance (bead):**
- [ ] `gt maint plan` prints a plan and never calls `Apply`; `gt maint apply` applies the plan it just printed
- [ ] `go test ./internal/cmd -run 'TestMaint' -count=1` passes (never the whole package)
- [ ] an unknown target lists the known targets; a refused plan is reported with its reason
- [ ] help text says plan is a dry run

### Task 9: disk readings, alerts and the keep policy

**Files:**
- Create: `internal/diskmaint/disks/disks.go`
- Test: `internal/diskmaint/disks/disks_test.go`

**Depends on:** nothing beyond the module (no dependency on other tasks).
**Interfaces:** Produces `Reading`, `ParseDF`, `Evaluate`, `KeepFor`, `RaiseAt`, `ClearAt`.

```go
package disks

const (
	RaiseAt = 80.0 // percent used
	ClearAt = 70.0
)

type Reading struct {
	Name    string  // "mac", "colima", "docker-desktop"
	UsedPct float64
}

type Alert struct {
	Key    string // "diskmaint:<name>"
	Raise  bool
	Clear  bool
	Detail string
}

// ParseDF reads the last line of "df -P" output and returns the Capacity column.
func ParseDF(out string) (float64, error)
// Evaluate compares readings with the keys already raised.
func Evaluate(readings []Reading, active map[string]bool) []Alert
// KeepFor returns 3 when any reading is at or over RaiseAt, else base.
func KeepFor(readings []Reading, base int) int
```

- [ ] **Step 1: Write failing tests:**

```go
func TestParseDFPortableOutput(t *testing.T)       // "overlay 131787236 72250308 52809712 58% /" gives 58
func TestParseDFRejectsGarbage(t *testing.T)
func TestEvaluateRaisesAtEighty(t *testing.T)
func TestEvaluateDoesNotRaiseBelowEighty(t *testing.T)
func TestEvaluateDoesNotRaiseTwice(t *testing.T)            // already active: no new Raise
func TestEvaluateClearsOnlyBelowSeventy(t *testing.T)       // 75% while active: neither raise nor clear
func TestKeepForTightensOverEighty(t *testing.T)
func TestKeepForKeepsBaseBelowEighty(t *testing.T)
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/diskmaint/disks -run 'Test' -count=1`.
- [ ] **Step 3: Implement** the four functions (pure; no I/O).
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Mutation check:** change `ClearAt` handling so a reading at 75% clears → `TestEvaluateClearsOnlyBelowSeventy` fails; restore.
- [ ] **Step 6: Commit** `feat: disk readings, 80/70 alert logic and keep policy`.

**Acceptance (bead):**
- [ ] `ParseDF`, `Evaluate` and `KeepFor` exist with the signatures in the plan
- [ ] `go test ./internal/diskmaint/disks -count=1` passes
- [ ] 75% while an alert is active neither raises nor clears
- [ ] the mutation in the plan turns its test red and is restored

### Task 10: `disk_maintenance` daemon patrol

**Files:**
- Create: `internal/daemon/disk_maintenance.go`
- Modify: `internal/daemon/lifecycle_defaults.go` (the patrol table, next to `scheduled_maintenance`), `internal/daemon/daemon.go` (call beside `d.runScheduledMaintenance()`), the `PatrolsConfig` type (new field)
- Test: `internal/daemon/disk_maintenance_test.go`

**Depends on:** Tasks 6 and 9.
**Interfaces:** Consumes `diskmaint.Target`, `disks.*`, `(*Daemon).maintenanceQuiet() (bool, string)`, `(*Daemon).escalateAlert(key, source, message string)`, `(*Daemon).clearAlerts(reason string, keys ...string)`. Produces:

```go
type DiskMaintenanceConfig struct {
	Enabled  bool   `json:"enabled"`
	Interval string `json:"interval"` // "daily"
	Window   string `json:"window"`   // "04:30"
	Mode     string `json:"mode"`     // "dry_run" (default) | "apply"
}
// PatrolsConfig gains: DiskMaintenance *DiskMaintenanceConfig `json:"disk_maintenance,omitempty"`
func (d *Daemon) runDiskMaintenance()
```

- [ ] **Step 1: Write failing tests** (names start with `TestDiskMaintenance`; build the daemon the way `scheduled_maintenance_test.go` does; inject fake targets, readers and a clock through a `d.diskMaint` seams struct):

```go
func TestDiskMaintenanceDisabledByDefault(t *testing.T)
func TestDiskMaintenanceRunsOnlyInItsWindow(t *testing.T)
func TestDiskMaintenanceRunsOncePerWindow(t *testing.T)
func TestDiskMaintenanceSkipsWhenNotQuiet(t *testing.T)       // maintenanceQuiet false: no plan, a log line
func TestDiskMaintenanceDryRunNeverApplies(t *testing.T)
func TestDiskMaintenanceApplyModeApplies(t *testing.T)
func TestDiskMaintenanceRaisesAlertAtEighty(t *testing.T)     // key "diskmaint:colima", source "disk_maintenance"
func TestDiskMaintenanceClearsAlertBelowSeventy(t *testing.T)
func TestDiskMaintenanceTightensKeepOverEighty(t *testing.T)  // the target's Policy.Keep is 3
func TestDiskMaintenanceUnreadableDiskRaisesNothing(t *testing.T)
func TestDiskMaintenanceConfigDecodes(t *testing.T)           // the JSON in the design decodes; an unknown mode is rejected
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/daemon -run 'TestDiskMaintenance' -count=1`.
- [ ] **Step 3: Implement** the config type, the patrol table entry, and `runDiskMaintenance`: gate on `Enabled`, the window and one run per day; skip when `d.maintenanceQuiet()` is false; read the three disks through injected `DFReader`s (production: `df -P /`; `colima ssh -p gt-sbx -- df -P /mnt/lima-colima-gt-sbx`; `docker exec forgejo-runner-daemon-1 df -P /`); call `disks.Evaluate` and raise or clear with `escalateAlert`/`clearAlerts`; compute `disks.KeepFor`; for each target `Plan`, and in `apply` mode `Apply`; log one summary line.
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat: disk_maintenance patrol with 80% alerts`.

**Acceptance (bead):**
- [ ] the patrol is off unless `daemon.patrols.disk_maintenance.enabled` is true and dry-run unless `mode` is `apply`
- [ ] `go test ./internal/daemon -run 'TestDiskMaintenance' -count=1` passes (never the whole package)
- [ ] it skips when `maintenanceQuiet()` is false and runs once per window
- [ ] an alert is raised at 80% and cleared only below 70%, keyed `diskmaint:<disk>`
- [ ] the shipped default config does NOT enable it (the live config gets the key after install)

### Task 11: registry creation: named volume and delete flag

**Files:**
- Modify: `deploy/k8s/kind/up.sh` (the `vmdocker run ... registry:2` line), `deploy/k8s/kind/kind_test.sh` (the expectation around line 306)

**Depends on:** nothing.
**Interfaces:** none. Existing shell files only; no new scripts.

- [ ] **Step 1: Update the test first.** In `kind_test.sh` change the expected `run` line to contain `-v gt-registry-data:/var/lib/registry -e REGISTRY_STORAGE_DELETE_ENABLED=true` alongside the existing flags.
- [ ] **Step 2: Run the test, expect FAIL:** `bash deploy/k8s/kind/kind_test.sh` (it runs against a fake docker; it creates no containers).
- [ ] **Step 3: Edit `up.sh`:** `vmdocker run -d --restart=always -p "127.0.0.1:$REGISTRY_PORT:5000" -v gt-registry-data:/var/lib/registry -e REGISTRY_STORAGE_DELETE_ENABLED=true --name "$REGISTRY_NAME" registry:2`. Do not change anything else; the network connect step stays as it is.
- [ ] **Step 4: Run the test, expect PASS.**
- [ ] **Step 5: Commit** `fix: the kind registry gets a named data volume and deletes enabled`.

**Acceptance (bead):**
- [ ] `up.sh` starts the registry with `-v gt-registry-data:/var/lib/registry` and `-e REGISTRY_STORAGE_DELETE_ENABLED=true`
- [ ] `bash deploy/k8s/kind/kind_test.sh` passes
- [ ] no other line of `up.sh` changes and no new script is added
- [ ] a comment above the line says an existing registry container is left alone (it was recreated by hand on 2026-10-28 with deletes enabled)

### Task 12: docs

**Files:**
- Create: `docs/guides/disk-maintenance.md`
- Modify: `docs/reference.md` (config rows)

**Depends on:** Tasks 7, 8, 10 (the names it documents).
**Interfaces:** none (docs only).

- [ ] **Step 1: Write the guide:** what the pruner deletes and never deletes, the digest rule in plain words, the three entry points, the dry-run to apply switch (`POLECAT_IMAGE_REGISTRY_PRUNE`, `daemon.patrols.disk_maintenance.mode`), how to read the ledger (`~/gt/.runtime/diskmaint/ledger.jsonl`), the 80%/70% alert keys, and the rollback (rebuild from the commit and re-push).
- [ ] **Step 2: Add reference rows** for `POLECAT_IMAGE_REGISTRY_KEEP`, `POLECAT_IMAGE_REGISTRY_PRUNE` and `daemon.patrols.disk_maintenance`.
- [ ] **Step 3: Run `make docs-lint`, expect exit 0.**
- [ ] **Step 4: Commit** `docs: disk maintenance guide and config reference`.

**Acceptance (bead):**
- [ ] `docs/guides/disk-maintenance.md` covers the keep rules, the three entry points, the dry-run switch, the ledger and the rollback
- [ ] `docs/reference.md` lists the two environment knobs and the patrol key with their defaults
- [ ] `make docs-lint` exits 0
- [ ] no personal name appears in either file

---

## Slices 2 and 3 (bead stubs, filed after slice 1 has landed and run in dry-run)

Each stub becomes a full task in its own plan pass once slice 1 has proved the package.

| ID | Title | Goal | Depends on |
|---|---|---|---|
| S2-1 | worktree target: planner | Pure planner over a list of worktrees (path, branch, dirty count, last commit age, pushed or landed, owner by directory name): remove only `~/gt/<rig>/crew/overseer-*` that are clean, pushed or landed, older than 2 days; all others report-only. | T1 |
| S2-2 | worktree target: discovery and apply | Discover worktrees with `git worktree list --porcelain` per crew clone, remove with `git worktree remove`, delete the merged local branch; ledger lines; the 11 stale overseer worktrees are the first real run. | S2-1 |
| S2-3 | `.bak` file target | Port the handbook's pre-approved rule to Go: keep the union of the newest 30 and every copy under 7 days per file; skip a file with an open handle (`lsof`); dry-run first. | T1 |
| S2-4 | old-binary target | Keep the two newest `*.bak-*` of one binary and delete a stand-alone `bd.<sha>` once a newer install replaced it; never the installed binary. | S2-3 |
| S2-5 | report-only sizes target | Report size, age and owner of the Dolt `.bak` directories, cache-trash directories and `~/gt-backups`; never delete; one ledger line per run. | T1 |
| S3-1 | nodeprune as a target | Wrap `cmd/nodeprune` as a Target (keep images a pod uses and the `dev` tag); retires the opt-in `POLE_NODE_PRUNE` question and closes `gt-06ni7.2`. | T1 |
| S3-2 | Colima docker image target | Plan `docker image prune` candidates on the Colima daemon with the same keep-in-use rule. | T1 |
| S3-3 | CI volume trim target | Port `trim-runner-caches.sh` (whole `<module>@<version>` directories, never `pkg/mod/cache`) to Go and retire the shell script. | T1 |

## Filing the beads

File from `~/gt/gastown` after the design and this plan have **landed on main** (a polecat reads them from `origin/main`). Each task above is one bead; use the task title as the bead title, the "Depends on" line as `bd dep add` edges, the task body as `## Goal`/`## Constraints`, and the **Acceptance (bead)** list as `--acceptance`. Run `gt spec lint <id>` on each. File Task 11 and Task 9 first (no dependencies), then in order:

```
T1  (no deps)
T9  (no deps)
T11 (no deps)
T2  depends on T1
T3  depends on T1
T4  depends on T1
T5  depends on T2, T3, T4
T6  depends on T5
T7  depends on T6
T8  depends on T6
T10 depends on T6, T9
T12 depends on T7, T8, T10
```

`bd dep add <issue> <depends-on>` for each edge, for example `bd dep add T5 T2`, `bd dep add T5 T3`, `bd dep add T5 T4`. Tasks 2, 3 and 4 are independent of each other and can run in parallel after Task 1; Task 6's package files overlap Task 5's `target.go`, so they must run in order. Hold the whole chain out of dispatch until the operator lifts the dispatch hold.

## Rollout

1. **Land and install.** Slice 1 lands task by task through the normal pipeline; install as usual. After the install, `gt maint plan` works.
2. **Dry-run.** `POLECAT_IMAGE_REGISTRY_PRUNE` defaults to `dry-run`, so every plugin run logs what it would delete and writes a ledger line, and deletes nothing. Read `~/gt/.runtime/diskmaint/ledger.jsonl` and `gt maint plan` for a day. Confirm the kept set contains `dev`, the installed commit and every pod image.
3. **Enable the patrol (dry-run).** Add `daemon.patrols.disk_maintenance` `{"enabled": true, "interval": "daily", "window": "04:30", "mode": "dry_run"}` to the town config only after the installed `gt` knows the key; restart on a quiet town. The disk alerts are live from this point.
4. **Apply.** The operator flips `POLECAT_IMAGE_REGISTRY_PRUNE=apply` (and the patrol `mode` to `apply`) when the dry runs look right.
5. **Rollback.** A deleted image is rebuilt from its commit (`make polecat-image` in a worktree at that commit, then the plugin's normal publish), so a wrong delete costs a rebuild, not data. To stop pruning at once, set `POLECAT_IMAGE_REGISTRY_PRUNE=off`; to stop the patrol, set `enabled` to false. The ledger names every manifest digest deleted and when.
