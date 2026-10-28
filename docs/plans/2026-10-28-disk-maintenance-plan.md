> Status: historical (2026-10). Merged in: gt-i7hxu (design and plan; the implementation beads are filed from the plan). Not maintained.

# Town Disk Maintenance Implementation Plan

> **For agentic workers:** every task below is written to be filed as exactly one Gas Town bead (twenty minutes of one polecat's work). Steps use checkbox syntax for tracking. Read the design first.

**Goal:** a tested Go pruner, `internal/diskmaint`, whose first target deletes old `gastown-polecat` images from the sandbox registry by digest, with a disk watch, a daily patrol, a `gt maint` command, and a hook in the `polecat-image` plugin.

**Architecture:** one small package defines `Target` (Measure, Plan, Apply), a plan type, and a JSONL ledger. A `registry` subpackage implements the first target against the registry's HTTP API. Three entry points call the same code: the `polecat-image` plugin (after its trims, under its lock), a daemon patrol (daily, quiet window), and `gt maint plan|apply`. Dry-run is the default everywhere, and every apply runs inside `polecatimage.WithLock`.

**Tech Stack:** Go (standard library `net/http`, `httptest`, `encoding/json`); no new dependencies, no new shell scripts. The only shell edit is `deploy/k8s/kind/up.sh` and its test, which already exist.

**Spec:** `docs/plans/2026-10-28-disk-maintenance-design.md`.

## Global Constraints

These restate rules from the design so an executor sees them in one place. **Where they differ from the design, the design wins and the plan is the bug.**

- Go with unit tests; **no new shell scripts**. Edit `deploy/k8s/kind/up.sh` and `deploy/k8s/kind/kind_test.sh` only where Task 11 says.
- Dry-run is the default. An unreadable input gives an **empty plan with `Refused` set**, never a guess.
- Registry deletes act on a **digest** and remove every tag that points at it. Never delete a digest that a kept tag shares.
- Every name from the registry or the sandbox records is validated (Task 2's validators) before it reaches a URL or a command.
- `internal/daemon`, `internal/cmd`, `internal/polecat` and `internal/beads` are container-suite packages: acceptance commands name a narrow `-run` filter, never the whole package.
- No `time.Sleep`, `time.After` or `t.Setenv` in unit tests; inject clocks, runners and HTTP bases.
- Commit authorship is the agent's own name; never the operator's name in code, comments, docs or fixtures (use placeholder names such as `repo-a`, `host-a`).
- The patrol's config key is read at daemon boot: Task 10 ships the code, and the key goes into the live config only after an installed `gt` knows it.

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `internal/diskmaint/types.go`, `ledger.go` | `Target`, `Plan`, `Action`, `Kept`, `Measurement`, `Result`, `Failure`, `Policy`, `Runner`; JSONL ledger | 1 |
| `internal/diskmaint/registry/names.go`, `client.go` | name validators; catalog, tags, delete | 2 |
| `internal/diskmaint/registry/manifest.go` | `GetManifest` (digest, created, index manifests) | 2b |
| `internal/diskmaint/registry/plan.go` | pure planner `BuildPlan` core rules | 3 |
| `internal/diskmaint/registry/plan_stray.go` | stray repos, name validation in the planner | 3b |
| `internal/diskmaint/registry/inuse.go` | in-use digests, tags and repos from pods and sandbox records | 4 |
| `internal/diskmaint/registry/target.go` | `Target`: `Measure` and `Plan` assembly | 5 |
| `internal/polecatimage/lock.go` | exported `WithLock` | 6a |
| `internal/diskmaint/registry/apply.go` | `Apply`: deletes, ledger, lock | 6 |
| `internal/diskmaint/registry/gc.go` | garbage collect | 6b |
| `internal/polecatimage/registryprune.go` | `PruneRegistry` hook and config | 7 |
| `internal/cmd/maint.go` | `gt maint plan|apply` | 8 |
| `internal/diskmaint/disks/disks.go` | `df` parsing, 80%/70% alert logic, unreadable alert, keep policy | 9 |
| `internal/daemon/disk_maintenance.go` | patrol: config, window, quiet gate, mode | 10 |
| `internal/daemon/disk_maintenance_disks.go` | patrol: disk reads, alerts, keep tightening | 10b |
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
	Collected  bool   // registry garbage collect ran
	Deferred   string // non-empty: nothing was applied, and why (lock held, push in flight)
}

type Policy struct {
	Keep  int  // newest per-commit tags to keep
	Apply bool // false is a dry run
	Now   time.Time
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
	Deferred   string    `json:"deferred,omitempty"`
	Errors     []string  `json:"errors,omitempty"`
}

type Ledger struct{ /* path, maxBytes */ }
func NewLedger(path string, maxBytes int64) *Ledger
func (l *Ledger) Append(e LedgerEntry) error
func (l *Ledger) Tail(n int) ([]LedgerEntry, error)
```

```go
// package registry (internal/diskmaint/registry)
const (
	RepoMain = "gastown-polecat"
	TagDev   = "dev"
)
var StrayRepos = []string{"gastown-polecat-measure", "seedtest/polecat"}
var (
	ErrDeletesDisabled = errors.New("registry deletes are disabled (HTTP 405)")
	ErrNotFound        = errors.New("registry: not found (HTTP 404)")
	ErrBadName         = errors.New("registry: name failed validation")
)

// Validators (names.go). Patterns are in the design, "The registry target", rule 7.
func ValidRepo(s string) bool
func ValidTag(s string) bool
func ValidDigest(s string) bool
func ValidContainer(s string) bool

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
// Tags, GetManifest and DeleteManifest return ErrBadName before any request when
// repo, ref or digest fails validation.

type TagInfo struct {
	Repo, Tag, Digest string
	Created           time.Time
	Bytes             int64
}
type PlanInput struct {
	Repos           map[string][]TagInfo // repo -> its tags
	Keep            int
	InstalledCommit string          // full sha from gt version; matched by tag prefix
	InUseDigests    map[string]bool // digests running pods use (tags already resolved to digests)
	InUseRepos      map[string]bool // repos named by sandbox records
	Errs            []string        // any failed read; non-empty means refuse
}
func BuildPlan(in PlanInput) diskmaint.Plan

type InUse struct {
	Digests map[string]bool // from containerStatuses[].imageID
	Tags    map[string]bool // "repo:tag" from container image fields
	Repos   map[string]bool
}
func ReadInUse(ctx context.Context, run diskmaint.Runner, kubeconfig, recordsDir string) (InUse, error)

type Config struct {
	Base, Kubeconfig, RecordsDir string
	DockerHost, Container        string // GC runs "docker exec <Container> ..." with DOCKER_HOST=<DockerHost>
	Keep                         int
	Installed                    func() (string, error) // full commit of the installed gt (from gt version)
	PushInFlight                 func() bool
	Lock                         func(ctx context.Context, fn func(context.Context) error) error // nil: the caller already holds the lock
	Run                          diskmaint.Runner
	HTTP                         *http.Client
	Ledger                       *diskmaint.Ledger
}
func NewTarget(cfg Config) *Target // *Target implements diskmaint.Target, Name() == "registry"
```

```go
// package polecatimage (internal/polecatimage), Task 6a
var ErrLockHeld = errors.New("polecat-image lock is held")
// WithLock runs fn while holding the plugin's lock, or returns ErrLockHeld.
func WithLock(ctx context.Context, cfg Config, fn func(context.Context) error) error
```

```go
// package disks (internal/diskmaint/disks), Task 9
const (
	RaiseAt = 80.0
	ClearAt = 70.0
)
type Reading struct {
	Name    string  // "mac", "colima", "docker-desktop"
	UsedPct float64
	Err     error   // non-nil: the disk could not be read
}
type Alert struct {
	Key    string // "diskmaint:<name>" or "diskmaint:<name>:unreadable"
	Raise  bool
	Clear  bool
	Detail string
}
func ParseDF(out string) (float64, error)
func Evaluate(readings []Reading, active map[string]bool) []Alert
func KeepFor(readings []Reading, base int) int
```

---

## Slice 1: the registry

### Task 1: diskmaint core types and ledger

**Files:** create `internal/diskmaint/types.go`, `internal/diskmaint/ledger.go`; test `internal/diskmaint/ledger_test.go`.
**Depends on:** nothing. **Produces:** everything in "Shared interfaces / package diskmaint".

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
```

Three more tests, same file: `TestLedgerRotatesAtMaxBytes` (append until the file would pass `maxBytes`; afterwards the live file is smaller than `maxBytes`, `<path>.1` exists, and `Tail` still returns the newest entries across both files); `TestLedgerTailOfMissingFileIsEmpty` (no file: nil slice, nil error); `TestLedgerSkipsTornLine` (a half-written last line is skipped and the earlier lines come back).

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/diskmaint -run 'TestLedger' -count=1` (undefined names).
- [ ] **Step 3: Implement** the types exactly as in "Shared interfaces", and the ledger: `Append` marshals one JSON line and appends with `O_APPEND`; when the file would pass `maxBytes` it renames the file to `<path>.1` (replacing an older `.1`) first; `Tail(n)` reads the live file (and `.1` if fewer than n are found), skips lines that do not decode, returns the last n oldest-first.
- [ ] **Step 4: Run, expect PASS** (same command).
- [ ] **Step 5: Commit** `feat: diskmaint core types and JSONL ledger`.

**Acceptance (bead):**
- [ ] `internal/diskmaint` declares `Target`, `Plan`, `Action`, `Kept`, `Measurement`, `Result`, `Failure`, `Policy`, `Runner`, `LedgerEntry` with the exact fields in the plan
- [ ] `go test ./internal/diskmaint -run 'TestLedger' -count=1` passes
- [ ] a torn last line and a missing file are tested and do not error
- [ ] rotation to `<path>.1` is tested

### Task 2: registry names and basic client (catalog, tags, delete)

**Files:** create `internal/diskmaint/registry/names.go`, `internal/diskmaint/registry/client.go`; test `names_test.go`, `client_test.go` in the same directory.
**Depends on:** Task 1 (the package directory and module path). **Produces:** `ValidRepo/ValidTag/ValidDigest/ValidContainer`, `Client`, `ErrDeletesDisabled`, `ErrNotFound`, `ErrBadName`, `Catalog`, `Tags`, `DeleteManifest`.

- [ ] **Step 1: Write failing tests.** Names (table-driven; each validator accepts and rejects):

```go
func TestValidRepo(t *testing.T) {
	ok := []string{"gastown-polecat", "seedtest/polecat", "a/b/c", "repo.a_b-c"}
	bad := []string{"", "../x", "a//b", "A", "a b", "a/", "/a", "a/../b", "a;rm", "a$(x)"}
	// every ok passes ValidRepo, every bad fails
}
```

Same shape for `TestValidTag` (accepts `dev`, `2d5a48dcb`, `build-3`; rejects empty, `-x`, `a b`, `a/b`, 129 characters), `TestValidDigest` (accepts `sha256:` plus 64 lowercase hex; rejects uppercase, 63 hex, `md5:..`), `TestValidContainer` (accepts `gt-registry`; rejects `-x`, `a b`, `a;b`). Client, with an `httptest.Server`:

```go
func TestCatalogAndTags(t *testing.T)                // both decode; a null "tags" is an empty slice
func TestTagsRejectsBadRepoBeforeRequest(t *testing.T)   // ErrBadName and the server saw no request
func TestDeleteManifestAccepted(t *testing.T)        // 202 is success
func TestDeleteManifest405MeansDisabled(t *testing.T)    // errors.Is(err, ErrDeletesDisabled)
func TestDeleteManifest404IsErrNotFound(t *testing.T)
func TestDeleteManifestRejectsBadDigest(t *testing.T)    // ErrBadName, no request
func TestClientErrorsOnOtherStatuses(t *testing.T)
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/diskmaint/registry -run 'TestValid|TestCatalog|TestTags|TestDeleteManifest|TestClientErrors' -count=1`.
- [ ] **Step 3: Implement** the patterns from the design (repo `^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`, tag `^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`, digest `^sha256:[0-9a-f]{64}$`, container `^[A-Za-z0-9][A-Za-z0-9_.-]*$`) and the client: `Catalog` (`GET /v2/_catalog`), `Tags` (`GET /v2/<repo>/tags/list`), `DeleteManifest` (`DELETE /v2/<repo>/manifests/<digest>`), each validating its arguments before building the URL.
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Mutation check:** remove the validation call from `Tags` → `TestTagsRejectsBadRepoBeforeRequest` fails; restore.
- [ ] **Step 6: Commit** `feat: registry name validators and basic v2 client`.

**Acceptance (bead):**
- [ ] the four validators exist and reject the bad cases listed in the tests
- [ ] `Catalog`, `Tags` and `DeleteManifest` exist with the plan's signatures and validate their arguments before any request
- [ ] `go test ./internal/diskmaint/registry -run 'TestValid|TestCatalog|TestTags|TestDeleteManifest|TestClientErrors' -count=1` passes
- [ ] 405 maps to `ErrDeletesDisabled` and 404 to `ErrNotFound`, tested with `httptest` only

### Task 2b: registry client: manifests

**Files:** create `internal/diskmaint/registry/manifest.go`; test `manifest_test.go`.
**Depends on:** Task 2. **Produces:** `Manifest`, `(*Client).GetManifest`.

- [ ] **Step 1: Write failing tests** (`httptest` server serving manifests and config blobs):

```go
func TestGetManifestImage(t *testing.T)               // Digest from Docker-Content-Digest; Created from the config blob "created"; Bytes = config + layer sizes
func TestGetManifestIndexTakesNewestChild(t *testing.T)   // index: newest child's created wins; Digest is the index digest
func TestGetManifestSendsAcceptHeaders(t *testing.T)  // the request asks for oci index, oci manifest and docker v2 manifest
func TestGetManifestRejectsBadRefBeforeRequest(t *testing.T)
func TestGetManifestMissingCreatedIsError(t *testing.T)   // a config blob with no "created": an error, never a zero time
func TestGetManifest404IsErrNotFound(t *testing.T)
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/diskmaint/registry -run 'TestGetManifest' -count=1`.
- [ ] **Step 3: Implement** `GetManifest`: `GET /v2/<repo>/manifests/<ref>` with `Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json`; for an image manifest read the config blob (`/v2/<repo>/blobs/<config digest>`) and its `created`; for an index take the newest child's; validate `repo` and `ref` (a ref is a valid tag or a valid digest).
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat: registry client manifests`.

**Acceptance (bead):**
- [ ] `GetManifest(ctx, repo, ref)` returns digest, creation time and size for an image manifest and for an index
- [ ] `go test ./internal/diskmaint/registry -run 'TestGetManifest' -count=1` passes
- [ ] a missing `created` is an error, not a zero time
- [ ] the Accept header is asserted in a test

### Task 3: registry planner: core retention rules (pure)

**Files:** create `internal/diskmaint/registry/plan.go`; test `plan_test.go`.
**Depends on:** Task 1. **Produces:** `TagInfo`, `PlanInput`, `BuildPlan`, `RepoMain`, `TagDev`, `StrayRepos`.

The rules in this task:

1. Candidates are tags of `RepoMain` that are hex and 7 to 12 characters. Every other tag is never a candidate and its digest is kept (`Rule: "named tag"`).
2. Keep set of digests: the digest of tag `dev`; the digest of the tag that is a prefix of `InstalledCommit`; every digest in `InUseDigests`; the digests of the newest `Keep` candidates by `Created` (ties broken by tag name).
3. A manifest is deleted only if its digest is outside the keep set. A candidate whose digest equals a kept digest is kept (`Rule: "shares a kept digest"`). Two candidate tags with one digest produce one action.
4. The plan refuses (`Refused` set, no actions) when `len(Errs) > 0`, when `InstalledCommit` is empty, when `RepoMain` has no tags, or when tag `dev` is absent from `RepoMain`.

- [ ] **Step 1: Write failing tests** (table-driven; a `fixture` helper builds `TagInfo` lists):

```go
func TestPlanDeletesOldCandidatesBeyondKeep(t *testing.T)     // 8 candidates, Keep 5: the 3 oldest digests are actions
func TestPlanKeepsTagSharingDevDigest(t *testing.T)           // old tag aaa1111 has dev's digest: no action, Kept rule "shares a kept digest"
func TestPlanKeepsInstalledCommitTag(t *testing.T)            // tag is a prefix of InstalledCommit: kept even if the oldest
func TestPlanKeepsInUseDigest(t *testing.T)                   // digest in InUseDigests kept even if it is the oldest
func TestPlanNeverTouchesNonHexTags(t *testing.T)             // "dev", "latest", "build-3": never an action
func TestPlanTwoTagsOneDigestCountOnce(t *testing.T)
func TestPlanKeepZeroStillKeepsDevAndInstalled(t *testing.T)
func TestPlanRefusesOnAnyReadError(t *testing.T)              // Errs non-empty: Refused set, zero actions
func TestPlanRefusesWithoutInstalledCommit(t *testing.T)
func TestPlanRefusesOnEmptyMainRepo(t *testing.T)
func TestPlanRefusesWhenDevTagAbsent(t *testing.T)            // a registry without dev is mid-rebuild or broken
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/diskmaint/registry -run 'TestPlan' -count=1`.
- [ ] **Step 3: Implement `BuildPlan`** for these rules (stray repos arrive in Task 3b; until then `Repos` entries other than `RepoMain` are ignored). Every `Kept` entry names its rule; `Action.Why` names the age rank.
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Mutation checks (each must turn a named test red; then restore; list them in the commit message):**
  - remove the "shares a kept digest" guard → `TestPlanKeepsTagSharingDevDigest` fails
  - remove the in-use check → `TestPlanKeepsInUseDigest` fails
  - treat non-hex tags as candidates → `TestPlanNeverTouchesNonHexTags` fails
  - drop the `dev`-absent refusal → `TestPlanRefusesWhenDevTagAbsent` fails
- [ ] **Step 6: Commit** `feat: registry retention planner by digest`.

**Acceptance (bead):**
- [ ] `BuildPlan` keeps `dev`, the installed commit's tag, in-use digests and the newest `Keep`, and never deletes a digest a kept tag shares
- [ ] `go test ./internal/diskmaint/registry -run 'TestPlan' -count=1` passes
- [ ] the four mutations in the plan each turn a named test red and are restored
- [ ] a failed read, an empty main repo or a missing `dev` tag returns a plan with `Refused` set and no actions

### Task 3b: registry planner: stray repos and name validation

**Files:** create `internal/diskmaint/registry/plan_stray.go`; modify `plan.go` (call the stray step; validate input names); test `plan_stray_test.go`.
**Depends on:** Tasks 2 and 3.

- [ ] **Step 1: Write failing tests:**

```go
func TestPlanStrayRepoDeletedWhenUnreferenced(t *testing.T)      // every digest of its tags is an action, Why "stray repo"
func TestPlanStrayRepoKeptWhenNamedBySandboxRecord(t *testing.T) // InUseRepos has it: Kept rule "named by a sandbox record"
func TestPlanStrayRepoDigestSharedWithKeptTagIsKept(t *testing.T)    // a stray repo's digest equal to a kept main-repo digest is not deleted
func TestPlanUnknownRepoIsNeverTouched(t *testing.T)             // a repo not in StrayRepos and not RepoMain: ignored
func TestPlanRefusesOnInvalidNames(t *testing.T)                 // a repo, tag or digest failing the validators in Repos: Refused names which
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/diskmaint/registry -run 'TestPlanStray|TestPlanUnknown|TestPlanRefusesOnInvalid' -count=1`.
- [ ] **Step 3: Implement** the stray-repo step (only `StrayRepos`, only when not in `InUseRepos`) and run `ValidRepo/ValidTag/ValidDigest` over every name in `PlanInput.Repos` first, refusing on the first failure.
- [ ] **Step 4: Run, expect PASS** (also re-run Task 3's `-run 'TestPlan'`).
- [ ] **Step 5: Mutation checks:** remove the `InUseRepos` guard → `TestPlanStrayRepoKeptWhenNamedBySandboxRecord` fails; remove the validation pass → `TestPlanRefusesOnInvalidNames` fails; restore both.
- [ ] **Step 6: Commit** `feat: registry planner stray repos and name validation`.

**Acceptance (bead):**
- [ ] stray repos are deleted only when no sandbox record names them and only digests no kept tag shares
- [ ] `go test ./internal/diskmaint/registry -run 'TestPlan' -count=1` passes (Task 3's and this task's tests)
- [ ] an invalid repo, tag or digest name refuses the plan and names the offender
- [ ] the two mutations in the plan each turn a named test red and are restored

### Task 4: in-use provider

**Files:** create `internal/diskmaint/registry/inuse.go`; test `inuse_test.go`.
**Depends on:** Tasks 1 and 2 (`diskmaint.Runner`, the validators). **Produces:** `InUse`, `ReadInUse`.

- [ ] **Step 1: Write failing tests** with a fake `Runner` returning canned `kubectl get pods -A -o json` and a temp records directory. Fixtures use three `imageID` shapes the kind (containerd) nodes produce: `localhost:5001/gastown-polecat@sha256:<64 hex>`, `docker-pullable://localhost:5001/gastown-polecat@sha256:<64 hex>`, and a bare `sha256:<64 hex>` (no repository, a config digest).

```go
func TestReadInUseCollectsDigestsFromEachImageIDShape(t *testing.T)  // the first two give Digests; the bare one is ignored for Digests (it names no manifest) and noted
func TestReadInUseCollectsTagsFromImageFields(t *testing.T)           // image "localhost:5001/gastown-polecat:2d5a48dcb" gives Tags["gastown-polecat:2d5a48dcb"]; init containers too
func TestReadInUseCollectsReposFromRefs(t *testing.T)                 // "localhost:5001/seedtest/polecat:x" gives Repos["seedtest/polecat"]
func TestReadInUseReadsSandboxRecords(t *testing.T)                   // a record naming an image adds its repo and tag
func TestReadInUseFailsClosedOnKubectlError(t *testing.T)
func TestReadInUseFailsClosedOnBadJSON(t *testing.T)
func TestReadInUseMissingRecordsDirIsAnError(t *testing.T)            // the town runs sandboxes: the directory should exist, so absence refuses
func TestReadInUseUnreadableRecordsDirIsAnError(t *testing.T)
func TestReadInUseDropsNamesFailingValidation(t *testing.T)           // a ref with a bad repo or tag yields an error (ErrBadName), never a quiet skip
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/diskmaint/registry -run 'TestReadInUse' -count=1`.
- [ ] **Step 3: Implement:** run `kubectl --kubeconfig <kubeconfig> get pods -A -o json` through `run`; walk `status.containerStatuses[]` and `initContainerStatuses[]` (`imageID`, `image`) and `spec.containers[]`/`initContainers[]` (`image`); split `host:port/<repo>:<tag>` and `<repo>@sha256:<digest>`; read each regular file in `recordsDir` as text and add any `host:port/<repo>[:tag]` reference it contains; validate every extracted repo, tag and digest.
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Mutation check:** make a kubectl error return an empty `InUse` instead of an error → `TestReadInUseFailsClosedOnKubectlError` fails; restore.
- [ ] **Step 6: Commit** `feat: registry in-use digests, tags and repos from pods and sandbox records`.

**Acceptance (bead):**
- [ ] `ReadInUse` returns the digests, tags and repos that running pods and sandbox records use, for the three `imageID` shapes
- [ ] `go test ./internal/diskmaint/registry -run 'TestReadInUse' -count=1` passes
- [ ] a kubectl error, bad JSON, a missing or unreadable records directory and a name failing validation each return an error, never an empty set
- [ ] the fail-closed mutation in the plan turns its test red

### Task 5: registry target: Measure and Plan assembly

**Files:** create `internal/diskmaint/registry/target.go`; test `target_test.go`.
**Depends on:** Tasks 2b, 3b, 4. **Produces:** `Config`, `Target`, `NewTarget`, `Name`, `Measure`, `Plan`. `Apply` is a stub that returns `errors.New("registry apply: not implemented")` until Task 6.

- [ ] **Step 1: Write failing tests** with an `httptest` registry (two repos, tags with creation times) and fakes:

```go
func TestTargetNameIsRegistry(t *testing.T)
func TestPlanAssemblesInputAndBuildsPlan(t *testing.T)         // lists repos and tags, fetches manifests, calls the in-use and installed fakes; the plan equals BuildPlan on the same data
func TestPlanResolvesInUseTagsToDigests(t *testing.T)          // a pod using tag X keeps X's digest even though no imageID names it
func TestPlanRefusesWhenAnInUseTagCannotBeResolved(t *testing.T)
func TestPlanSkipsManifestFetchWhenTagCountSmall(t *testing.T) // main repo has <= Keep+1 tags and no stray repo: only tags/list is requested
func TestPlanRefusesWhenCatalogFails(t *testing.T)
func TestPlanRefusesWhenOneManifestFetchFails(t *testing.T)
func TestPlanRefusesWhenInstalledCommitUnreadable(t *testing.T)
func TestMeasureReportsTagCountAndDetail(t *testing.T)
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/diskmaint/registry -run 'TestTarget|TestPlanAssembles|TestPlanResolves|TestPlanSkips|TestPlanRefuses|TestMeasure' -count=1`.
- [ ] **Step 3: Implement** `Plan`: `Catalog`; for each repo `Tags`; fetch manifests for `RepoMain` and the stray repos only when the main repo has more than `Keep+1` tags or a stray repo exists; resolve each `InUse.Tags` entry through `GetManifest` and merge its digest into `InUseDigests`; collect every error into `Errs`; read `Installed()` and `ReadInUse`; call `BuildPlan` with `Keep` from `Policy.Keep` (falling back to `Config.Keep`). `Measure` returns the tag count and the sum of manifest bytes in `Detail`.
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat: registry target measure and plan`.

**Acceptance (bead):**
- [ ] `NewTarget(cfg)` returns a value implementing `diskmaint.Target` with `Name() == "registry"`
- [ ] `go test ./internal/diskmaint/registry -run 'TestTarget|TestPlanAssembles|TestPlanResolves|TestPlanSkips|TestPlanRefuses|TestMeasure' -count=1` passes
- [ ] an in-use tag's digest is kept, and an unresolvable in-use tag refuses the plan
- [ ] when the main repo has `Keep+1` tags or fewer and there is no stray repo, the test shows only the tags request was made

### Task 6a: `polecatimage.WithLock`

**Files:** modify `internal/polecatimage/lock.go`; test `internal/polecatimage/lock_test.go` (follow the existing lock tests).
**Depends on:** nothing. **Produces:** `ErrLockHeld`, `WithLock`.

- [ ] **Step 1: Write failing tests:**

```go
func TestWithLockRunsFnWhileHoldingTheLock(t *testing.T)     // inside fn the lock directory exists with this pid
func TestWithLockReleasesAfterFn(t *testing.T)               // after return the lock is gone, also when fn returns an error
func TestWithLockReturnsErrLockHeldWhenAnotherRunHoldsIt(t *testing.T)
func TestWithLockTakesOverAStaleLock(t *testing.T)           // a lock older than Cfg.StaleAfter is taken over like a plugin run does
func TestWithLockPassesFnsErrorThrough(t *testing.T)
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/polecatimage -run 'TestWithLock' -count=1`.
- [ ] **Step 3: Implement** `WithLock` on top of the existing `ClaimLock`/`TakeLock`/`Cleanup` logic, with `ErrLockHeld` for the held case; do not change how a plugin run takes the lock.
- [ ] **Step 4: Run, expect PASS** (also `go test ./internal/polecatimage -run 'Lock' -count=1`).
- [ ] **Step 5: Commit** `feat: polecatimage.WithLock for callers outside a plugin run`.

**Acceptance (bead):**
- [ ] `WithLock(ctx, cfg, fn)` holds the plugin lock while `fn` runs and releases it after, also on error
- [ ] `go test ./internal/polecatimage -run 'TestWithLock' -count=1` passes
- [ ] a held lock returns `ErrLockHeld` without running `fn`; a stale lock is taken over as the plugin does
- [ ] existing lock tests still pass (`go test ./internal/polecatimage -run 'Lock' -count=1`)

### Task 6: registry target: Apply (deletes, ledger, lock)

**Files:** modify `internal/diskmaint/registry/target.go` (remove the `Apply` stub); create `internal/diskmaint/registry/apply.go`; test `apply_test.go`.
**Depends on:** Tasks 5 and 6a. **Produces:** `(*Target).Apply(ctx, plan diskmaint.Plan) (diskmaint.Result, error)`. `Action.Ref` is `repo@sha256:...`. `Apply` calls `gc` (Task 6b); until 6b lands, `gc` is a no-op method returning `(false, nil)` defined in `apply.go`, and 6b replaces it in `gc.go`.

- [ ] **Step 1: Write failing tests** (`httptest` registry recording deletes; fake `Lock`):

```go
func TestApplyDeletesEachManifest(t *testing.T)
func TestApplyContinuesAfterOneDeleteFails(t *testing.T)     // the failure is in Result.Failed; the others are Done
func TestApplyDeletesDisabledStopsAtFirstDelete(t *testing.T)    // ErrDeletesDisabled: no more deletes, an error line in the ledger
func TestApplyRunsInsideTheLock(t *testing.T)                // the fake Lock records fn running between acquire and release
func TestApplyDefersWhenLockHeld(t *testing.T)               // Lock returns polecatimage-style ErrLockHeld: no deletes, Result.Deferred set, a ledger line
func TestApplyWithNilLockRunsDirectly(t *testing.T)          // nil means the caller already holds it
func TestApplyWritesALedgerLine(t *testing.T)
func TestApplyOfRefusedPlanDoesNothing(t *testing.T)
func TestApplyRejectsAnActionRefWithABadName(t *testing.T)   // a ref failing the validators is a Failure, no request sent
```

`Config.Lock` is a function, so the registry package does not import `polecatimage`; the test's fake returns its own `errLockHeld`, and the target treats **any** error from `Lock` that wraps the sentinel `ErrDeferred` (defined in `registry`, `var ErrDeferred = errors.New("registry apply deferred")`) as a deferral. Production wiring (Tasks 7, 8, 10) wraps `polecatimage.ErrLockHeld` with `ErrDeferred`.

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/diskmaint/registry -run 'TestApply' -count=1`.
- [ ] **Step 3: Implement** `Apply`: return immediately for a plan with `Refused` or no actions; run the body through `Config.Lock` (or directly when nil); inside: for each action split `repo@digest`, validate, call `DeleteManifest`; stop on `ErrDeletesDisabled`; count `Done`/`Failed` and `BytesFreed`; call `t.gc(ctx, result)`; append one `LedgerEntry` (mode `apply`; `Deferred` set on a deferral).
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat: registry apply under the lock`.

**Acceptance (bead):**
- [ ] `Apply` deletes manifests by digest, records failures and continues, and writes one ledger line
- [ ] `go test ./internal/diskmaint/registry -run 'TestApply' -count=1` passes
- [ ] the deletes run inside `Config.Lock`, and a held lock defers with `Result.Deferred` set and no request sent
- [ ] `ErrDeletesDisabled` stops further deletes and is recorded

### Task 6b: registry target: garbage collect

**Files:** create `internal/diskmaint/registry/gc.go` (replaces the no-op `gc`); test `gc_test.go`.
**Depends on:** Task 6.

- [ ] **Step 1: Write failing tests** (fake `Runner`, records its calls; call `(*Target).gc` through `Apply`):

```go
func TestGCRunsOnlyAfterASuccessfulDelete(t *testing.T)
func TestGCSkippedWhenEveryDeleteFailed(t *testing.T)
func TestGCUsesColimaDockerHost(t *testing.T)    // env has DOCKER_HOST=<cfg.DockerHost>; argv is exactly: exec <Container> registry garbage-collect --delete-untagged /etc/docker/registry/config.yml
func TestGCSkippedWhenPushInFlight(t *testing.T) // deletes stay Done, Collected false, Result.Deferred-style note in the ledger
func TestGCRejectsBadContainerName(t *testing.T) // Config.Container failing ValidContainer: no command run, an error recorded
func TestGCFailureIsRecordedNotFatal(t *testing.T)   // the runner errors: Result.Collected false, the ledger carries the error, Apply still returns the Result
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/diskmaint/registry -run 'TestGC' -count=1`.
- [ ] **Step 3: Implement** `gc`: skip when `len(Done) == 0` or `PushInFlight()`; validate `Container`; run `docker exec <Container> registry garbage-collect --delete-untagged /etc/docker/registry/config.yml` through `Config.Run` with env `DOCKER_HOST=<DockerHost>`; set `Result.Collected`.
- [ ] **Step 4: Run, expect PASS** (also re-run `-run 'TestApply'`).
- [ ] **Step 5: Mutation check:** run garbage collect even when no delete succeeded → `TestGCSkippedWhenEveryDeleteFailed` fails; restore.
- [ ] **Step 6: Commit** `feat: registry garbage collect after deletes`.

**Acceptance (bead):**
- [ ] garbage collect runs only when at least one delete succeeded and no push is in flight, with `DOCKER_HOST` from the config and the exact argv in the plan
- [ ] `go test ./internal/diskmaint/registry -run 'TestGC|TestApply' -count=1` passes
- [ ] a bad container name runs nothing; a collect failure is recorded and does not fail the apply
- [ ] the mutation in the plan turns its test red and is restored

### Task 7: `polecat-image` hook

**Files:** create `internal/polecatimage/registryprune.go`; modify `internal/polecatimage/program.go` (call after `TrimCacheVolumes`; read the knobs in `ConfigFromEnv`), `internal/polecatimage/run.go` (Config fields), `plugins/polecat-image/plugin.md` (section); test `registryprune_test.go`.
**Depends on:** Task 6b. **Produces:** `Config.RegistryKeep string`, `Config.RegistryPrune string`, `(*Run).PruneRegistry()`.

- [ ] **Step 1: Write failing tests** (follow `prune_test.go`'s runner seam):

```go
func TestPruneRegistryOffDoesNothing(t *testing.T)          // POLECAT_IMAGE_REGISTRY_PRUNE=off: no HTTP request
func TestPruneRegistryDefaultsToDryRun(t *testing.T)        // unset: plans, logs the plan, sends no DELETE
func TestPruneRegistryApplyDeletes(t *testing.T)
func TestPruneRegistryNeedsTheRunsLock(t *testing.T)        // r.HaveLock false: skips with a logged reason
func TestPruneRegistryWithoutRegistryConfiguredSkips(t *testing.T)   // Cfg.Registry empty
func TestPruneRegistryBadKeepStopsThisPruneOnly(t *testing.T)        // "abc": logs why, no error returned
func TestPruneRegistryReadsTheInstalledCommitFromGTVersion(t *testing.T)   // Installed uses the same gt version read Program's first step uses, not the dev image label
func TestProgramCallsPruneRegistryAfterTrims(t *testing.T)  // order: PruneOldImages, PruneBuildCache, TrimCacheVolumes, PruneRegistry
func TestPruneRegistryNeverFailsTheRun(t *testing.T)        // an error inside it is logged; Program still returns nil
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/polecatimage -run 'TestPruneRegistry|TestProgramCallsPruneRegistry' -count=1`.
- [ ] **Step 3: Implement** `PruneRegistry`: read `Cfg.RegistryPrune` (`off|dry-run|apply`, default `dry-run`) and `Cfg.RegistryKeep` (default `5`, parsed with the existing `KeepCount`); require `r.HaveLock`; build the registry target with `Base = "http://" + Cfg.Registry`, `Lock: nil` (this run already holds the plugin lock), `Installed` = the existing read of the installed gt commit (`gt version`); log each action and the ledger summary; never return an error to `Program`. Add the two `Config` fields, read `POLECAT_IMAGE_REGISTRY_KEEP` and `POLECAT_IMAGE_REGISTRY_PRUNE` in `ConfigFromEnv`, and document both in a "Registry retention" section of `plugin.md`.
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat: polecat-image prunes the sandbox registry (dry-run by default)`.

**Acceptance (bead):**
- [ ] `Program` calls `PruneRegistry` after `TrimCacheVolumes`, only while the run holds the lock
- [ ] `go test ./internal/polecatimage -run 'TestPruneRegistry|TestProgramCallsPruneRegistry' -count=1` passes
- [ ] unset `POLECAT_IMAGE_REGISTRY_PRUNE` is a dry run that issues no DELETE request
- [ ] the installed commit comes from `gt version`, as in the design
- [ ] `plugin.md` has a "Registry retention" section naming both variables and their defaults

### Task 8: `gt maint plan|apply`

**Files:** create `internal/cmd/maint.go`; test `internal/cmd/maint_test.go`.
**Depends on:** Tasks 6a and 6b. **Produces:** `maintCmd` (registered under `GroupConfig`) and:

```go
type maintDeps struct {
	Targets func() (map[string]diskmaint.Target, error) // built from the town config in production, with Lock = polecatimage.WithLock wrapped in registry.ErrDeferred
	Now     func() time.Time
	Keep    int
}
func runMaint(out io.Writer, deps maintDeps, mode, target string) error // mode is "plan" or "apply"
```

- [ ] **Step 1: Write failing tests** (names start with `TestMaint`):

```go
func TestMaintPlanPrintsActionsAndKept(t *testing.T)
func TestMaintPlanNeverApplies(t *testing.T)             // a fake Target records Apply calls: zero
func TestMaintApplyRunsApplyOnThePlan(t *testing.T)
func TestMaintApplyRefusesARefusedPlan(t *testing.T)     // prints the Refused reason, returns an error, no Apply call
func TestMaintApplyReportsDeferralWhenLockHeld(t *testing.T) // Result.Deferred printed, exit error says to retry
func TestMaintUnknownTargetListsKnownOnes(t *testing.T)
func TestMaintNoTargetRunsEveryTarget(t *testing.T)
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/cmd -run 'TestMaint' -count=1`.
- [ ] **Step 3: Implement** `runMaint` and the thin cobra command `gt maint plan [target]` / `gt maint apply [target]`; production `Targets` builds the registry target from `settings/config.json` the way `polecatimage` reads its registry settings, with `Lock` wrapping `polecatimage.WithLock`.
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat: gt maint plan and apply`.

**Acceptance (bead):**
- [ ] `gt maint plan` prints a plan and never calls `Apply`; `gt maint apply` applies the plan it just printed
- [ ] `go test ./internal/cmd -run 'TestMaint' -count=1` passes (never the whole package)
- [ ] an unknown target lists the known targets; a refused plan and a held lock are reported with their reasons
- [ ] help text says `plan` is a dry run

### Task 9: disk readings, alerts and the keep policy

**Files:** create `internal/diskmaint/disks/disks.go`; test `disks_test.go`.
**Depends on:** nothing. **Produces:** `Reading`, `Alert`, `ParseDF`, `Evaluate`, `KeepFor`, `RaiseAt`, `ClearAt`.

- [ ] **Step 1: Write failing tests:**

```go
func TestParseDFPortableOutput(t *testing.T)       // "overlay 131787236 72250308 52809712 58% /" gives 58
func TestParseDFRejectsGarbage(t *testing.T)
func TestEvaluateRaisesAtEighty(t *testing.T)
func TestEvaluateDoesNotRaiseBelowEighty(t *testing.T)
func TestEvaluateDoesNotRaiseTwice(t *testing.T)            // already active: no new Raise
func TestEvaluateClearsOnlyBelowSeventy(t *testing.T)       // 75% while active: neither raise nor clear
func TestEvaluateUnreadableDiskRaisesItsOwnAlert(t *testing.T)   // Err set: Key "diskmaint:<name>:unreadable", Raise true
func TestEvaluateUnreadableAlertClearsWhenReadAgain(t *testing.T)
func TestEvaluateUnreadableDoesNotTouchTheUsageAlert(t *testing.T)   // an active usage alert stays active while the disk is unreadable
func TestKeepForTightensOverEighty(t *testing.T)
func TestKeepForKeepsBaseBelowEighty(t *testing.T)
func TestKeepForUnreadableDiskKeepsBase(t *testing.T)       // an unreadable disk does not tighten the keep: it cannot be judged
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/diskmaint/disks -count=1`.
- [ ] **Step 3: Implement** the four functions (pure; no I/O) per "Shared interfaces / package disks".
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Mutation checks:** change the clear rule so 75% clears → `TestEvaluateClearsOnlyBelowSeventy` fails; make an unreadable reading raise nothing → `TestEvaluateUnreadableDiskRaisesItsOwnAlert` fails; restore both.
- [ ] **Step 6: Commit** `feat: disk readings, 80/70 alert logic, unreadable alert and keep policy`.

**Acceptance (bead):**
- [ ] `ParseDF`, `Evaluate` and `KeepFor` exist with the signatures in the plan
- [ ] `go test ./internal/diskmaint/disks -count=1` passes
- [ ] 75% while an alert is active neither raises nor clears; an unreadable disk raises `diskmaint:<name>:unreadable`
- [ ] the two mutations in the plan each turn a named test red and are restored

### Task 10: `disk_maintenance` patrol: config, window, gate, mode

**Files:** create `internal/daemon/disk_maintenance.go`; modify `internal/daemon/lifecycle_defaults.go` (the patrol table, beside `scheduled_maintenance`), `internal/daemon/daemon.go` (call beside `d.runScheduledMaintenance()`), the `PatrolsConfig` type (new field); test `internal/daemon/disk_maintenance_test.go`.
**Depends on:** Tasks 6a and 6b. **Consumes:** `(*Daemon).maintenanceQuiet() (bool, string)`. **Produces:**

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

- [ ] **Step 1: Write failing tests** (names start with `TestDiskMaintenance`; build the daemon the way `scheduled_maintenance_test.go` does; inject fake targets and a clock through a `d.diskMaint` seams struct):

```go
func TestDiskMaintenanceDisabledByDefault(t *testing.T)
func TestDiskMaintenanceRunsOnlyInItsWindow(t *testing.T)
func TestDiskMaintenanceRunsOncePerWindow(t *testing.T)
func TestDiskMaintenanceSkipsWhenNotQuiet(t *testing.T)       // maintenanceQuiet false: no plan, a log line
func TestDiskMaintenanceDryRunNeverApplies(t *testing.T)
func TestDiskMaintenanceApplyModeAppliesUnderTheLock(t *testing.T)   // the fake target's Lock is called; ErrLockHeld defers, logged, no error
func TestDiskMaintenanceConfigDecodes(t *testing.T)           // the JSON in the design decodes; an unknown mode is rejected
func TestDiskMaintenanceShippedDefaultDoesNotEnableIt(t *testing.T)  // lifecycle default has the patrol disabled
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/daemon -run 'TestDiskMaintenance' -count=1`.
- [ ] **Step 3: Implement** the config type, the patrol table entry, and `runDiskMaintenance` (gate on `Enabled`, the window and one run per day; skip when `d.maintenanceQuiet()` is false; `Plan` each target, and in `apply` mode `Apply` with `Lock` wrapping `polecatimage.WithLock` and `registry.ErrDeferred`; log one summary line). Disk reads and alerts arrive in Task 10b: leave an injectable `d.diskMaint.readDisks` that returns nothing for now.
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat: disk_maintenance patrol (window, quiet gate, mode)`.

**Acceptance (bead):**
- [ ] the patrol is off unless `daemon.patrols.disk_maintenance.enabled` is true, and dry-run unless `mode` is `apply`
- [ ] `go test ./internal/daemon -run 'TestDiskMaintenance' -count=1` passes (never the whole package)
- [ ] it skips when `maintenanceQuiet()` is false, runs once per window, and applies only under the lock
- [ ] the shipped default config does NOT enable it (the live config gets the key after install)

### Task 10b: `disk_maintenance` patrol: disk reads, alerts, keep tightening

**Files:** create `internal/daemon/disk_maintenance_disks.go`; modify `disk_maintenance.go` (call it); test `internal/daemon/disk_maintenance_disks_test.go`.
**Depends on:** Tasks 9 and 10. **Consumes:** `(*Daemon).escalateAlert(key, source, message string)`, `(*Daemon).clearAlerts(reason string, keys ...string)`, `disks.*`.

- [ ] **Step 1: Write failing tests** (names start with `TestDiskMaintenance`):

```go
func TestDiskMaintenanceRaisesAlertAtEighty(t *testing.T)       // key "diskmaint:colima", source "disk_maintenance"
func TestDiskMaintenanceClearsAlertBelowSeventy(t *testing.T)
func TestDiskMaintenanceUnreadableDiskRaisesUnreadableAlert(t *testing.T)   // key "diskmaint:colima:unreadable"; it is not silent
func TestDiskMaintenanceTightensKeepOverEighty(t *testing.T)    // the target's Policy.Keep is 3
func TestDiskMaintenanceReadsTheThreeDisks(t *testing.T)        // the injected readers for mac, colima, docker-desktop are each called once
func TestDiskMaintenanceDiskAlertsDoNotNeedApplyMode(t *testing.T)  // dry_run still alerts
```

- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/daemon -run 'TestDiskMaintenance' -count=1`.
- [ ] **Step 3: Implement** the disk reads behind injected `DFReader` funcs (production: `df -P /`; `colima ssh -p gt-sbx -- df -P /mnt/lima-colima-gt-sbx`; `docker exec forgejo-runner-daemon-1 df -P /`) wrapping `disks.ParseDF`; call `disks.Evaluate` with the active-key set the patrol keeps in memory (rebuilt from open alerts at boot is out of scope: a restart re-raises once), raise with `escalateAlert`, clear with `clearAlerts`; pass `disks.KeepFor(readings, base)` as `Policy.Keep`.
- [ ] **Step 4: Run, expect PASS** (also Task 10's tests).
- [ ] **Step 5: Commit** `feat: disk_maintenance disk readings, alerts and keep tightening`.

**Acceptance (bead):**
- [ ] an alert is raised at 80% and cleared only below 70%, keyed `diskmaint:<disk>`
- [ ] an unreadable disk raises `diskmaint:<disk>:unreadable` instead of staying silent
- [ ] `go test ./internal/daemon -run 'TestDiskMaintenance' -count=1` passes (never the whole package)
- [ ] over 80% the target is planned with `Keep` 3; alerts fire in dry-run mode too

### Task 11: registry creation: named volume and delete flag

**Files:** modify `deploy/k8s/kind/up.sh` (the `vmdocker run ... registry:2` line), `deploy/k8s/kind/kind_test.sh` (the expectation around line 306).
**Depends on:** nothing. Existing shell files only; no new scripts.

- [ ] **Step 1: Update the test first.** In `kind_test.sh` change the expected `run` line to contain `-v gt-registry-data:/var/lib/registry -e REGISTRY_STORAGE_DELETE_ENABLED=true` alongside the existing flags.
- [ ] **Step 2: Run the test, expect FAIL:** `bash deploy/k8s/kind/kind_test.sh` (it runs against a fake docker; it creates no containers).
- [ ] **Step 3: Edit `up.sh`:** `vmdocker run -d --restart=always -p "127.0.0.1:$REGISTRY_PORT:5000" -v gt-registry-data:/var/lib/registry -e REGISTRY_STORAGE_DELETE_ENABLED=true --name "$REGISTRY_NAME" registry:2`. Change nothing else; the network connect step stays as it is.
- [ ] **Step 4: Run the test, expect PASS.**
- [ ] **Step 5: Commit** `fix: the kind registry gets a named data volume and deletes enabled`. Put the history (an existing container was recreated by hand with deletes enabled on 2026-10-28 and `up.sh` leaves an existing container alone) in the commit message, not in the script.

**Acceptance (bead):**
- [ ] `up.sh` starts the registry with `-v gt-registry-data:/var/lib/registry` and `-e REGISTRY_STORAGE_DELETE_ENABLED=true`
- [ ] `bash deploy/k8s/kind/kind_test.sh` passes
- [ ] no other line of `up.sh` changes and no new script is added
- [ ] a one-line comment above the run line says what the named volume and the delete flag are for (what they do, not when they were added)

### Task 12: docs

**Files:** create `docs/guides/disk-maintenance.md`; modify `docs/reference.md` (config rows).
**Depends on:** Tasks 7, 8 and 10b (the names it documents). Docs only.

- [ ] **Step 1: Write the guide:** what the pruner deletes and never deletes, the digest rule in plain words, the lock and where it applies, the three entry points, the dry-run to apply switch (`POLECAT_IMAGE_REGISTRY_PRUNE`, `daemon.patrols.disk_maintenance.mode`), how to read the ledger (`~/gt/.runtime/diskmaint/ledger.jsonl`), the alert keys (`diskmaint:<disk>` and `diskmaint:<disk>:unreadable`), and the rollback (rebuild from the commit and re-push).
- [ ] **Step 2: Add reference rows** for `POLECAT_IMAGE_REGISTRY_KEEP`, `POLECAT_IMAGE_REGISTRY_PRUNE` and `daemon.patrols.disk_maintenance`.
- [ ] **Step 3: Run `make docs-lint`, expect exit 0.**
- [ ] **Step 4: Commit** `docs: disk maintenance guide and config reference`.

**Acceptance (bead):**
- [ ] `docs/guides/disk-maintenance.md` covers the keep rules, the lock, the three entry points, the dry-run switch, the ledger, the alert keys and the rollback
- [ ] `docs/reference.md` lists the two environment knobs and the patrol key with their defaults
- [ ] `make docs-lint` exits 0
- [ ] no personal name appears in either file

---

## Slices 2 and 3 (bead stubs, filed after slice 1 has landed and run in dry-run)

Each stub becomes a full task in its own plan pass once slice 1 has proved the package.

| ID | Title | Goal | Depends on |
|---|---|---|---|
| S2-1 | worktree target: planner | Pure planner over a list of worktrees (path, branch, dirty count, last commit age, pushed or landed, owner by directory name): remove only `~/gt/<rig>/crew/overseer-*` that are clean, pushed or landed, older than 2 days; all others report-only. | T1 |
| S2-2 | worktree target: discovery and apply | Discover worktrees with `git worktree list --porcelain` per crew clone, remove with `git worktree remove`, delete the merged local branch; ledger lines; the stale overseer worktrees are the first real run. | S2-1 |
| S2-3 | `.bak` file target | Port the handbook's pre-approved rule to Go: keep the union of the newest 30 and every copy under 7 days per file; skip a file with an open handle (`lsof`); dry-run first. | T1 |
| S2-4 | old-binary target | Keep the two newest `*.bak-*` of one binary and delete a stand-alone `bd.<sha>` once a newer install replaced it; never the installed binary. | S2-3 |
| S2-5 | report-only sizes target | Report size, age and owner of the Dolt `.bak` directories, cache-trash directories and `~/gt-backups`; never delete; one ledger line per run. | T1 |
| S2-6 | `hm sync` schedule | Run `hm sync` on a schedule (a patrol target or a LaunchAgent managed by hm's own `make` targets) and alert when every source is STALE. | T1 |
| S3-1 | nodeprune as a target | Wrap `cmd/nodeprune` as a Target (keep images a pod uses and the `dev` tag); retires the opt-in `POLE_NODE_PRUNE` question and closes `gt-06ni7.2`. | T1 |
| S3-2 | Colima docker image target | Plan `docker image prune` candidates on the Colima daemon with the same keep-in-use rule. | T1 |
| S3-3 | CI volume trim target | Port `trim-runner-caches.sh` (whole `<module>@<version>` directories, never `pkg/mod/cache`) to Go and retire the shell script. | T1 |

## Filing the beads

File from `~/gt/gastown` after the design and this plan have **landed on main** (a polecat reads them from `origin/main`). Each task above is one bead: the task title is the bead title, the "Depends on" line gives the `bd dep add` edges, the task body gives `## Goal` and `## Constraints`, and the **Acceptance (bead)** list goes in `--acceptance`. Run `gt spec lint <id>` on each. Seventeen beads:

```
T1   (no deps)
T6a  (no deps)
T9   (no deps)
T11  (no deps)
T2   depends on T1
T3   depends on T1
T2b  depends on T2
T3b  depends on T2, T3
T4   depends on T1, T2
T5   depends on T2b, T3b, T4
T6   depends on T5, T6a
T6b  depends on T6
T7   depends on T6b
T8   depends on T6a, T6b
T10  depends on T6a, T6b
T10b depends on T9, T10
T12  depends on T7, T8, T10b
```

`bd dep add <issue> <depends-on>` for each edge, for example `bd dep add T5 T2b`, `bd dep add T5 T3b`, `bd dep add T5 T4`. Tasks 2 and 3 can run in parallel after Task 1; Tasks 6 and 6b both touch the `registry` package and must run in order; Tasks 10 and 10b both touch the patrol and must run in order. Hold the whole chain out of dispatch until the operator lifts the dispatch hold.

## Rollout

1. **Land and install.** Slice 1 lands task by task through the normal pipeline; install as usual. After the install, `gt maint plan` works.
2. **Dry-run.** `POLECAT_IMAGE_REGISTRY_PRUNE` defaults to `dry-run`, so every plugin run logs what it would delete and writes a ledger line, and deletes nothing. Read `~/gt/.runtime/diskmaint/ledger.jsonl` and `gt maint plan` for a day. Confirm the kept set contains `dev`, the installed commit and every pod image.
3. **Enable the patrol (dry-run).** Add `daemon.patrols.disk_maintenance` `{"enabled": true, "interval": "daily", "window": "04:30", "mode": "dry_run"}` to the town config only after the installed `gt` knows the key; restart on a quiet town. The disk alerts are live from this point.
4. **Apply.** The operator flips `POLECAT_IMAGE_REGISTRY_PRUNE=apply` (and the patrol `mode` to `apply`) when the dry runs look right.
5. **Rollback.** A deleted image is rebuilt from its commit (`make polecat-image` in a worktree at that commit, then the plugin's normal publish), so a wrong delete costs a rebuild, not data. To stop pruning at once, set `POLECAT_IMAGE_REGISTRY_PRUNE=off`; to stop the patrol, set `enabled` to false. The ledger names every manifest digest deleted and when.
