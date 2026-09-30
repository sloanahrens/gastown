package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/scheduler/capacity"
)

// slingHarness is a gt sling with every collaborator faked: beads live in a
// map, every write and side effect lands in one ordered call log, and the
// run's output is a buffer. It spawns nothing and touches no package global,
// file, PATH, cwd or environment variable, so its tests run in parallel.
//
// Defaults describe a quiet town: rig "gastown" exists, a rig target spawns
// polecat Toast, every write succeeds and no agent is dead. A test edits the
// world (h.beads, h.env, ...) or replaces one dep on h.run before h.sling.
type slingHarness struct {
	t   *testing.T
	run *slingRun
	out *bytes.Buffer

	mu             sync.Mutex
	calls          []string
	beads          map[string]*beadInfo
	env            map[string]string
	rigs           map[string]bool
	formulas       map[string]bool
	dead           map[string]bool
	molecules      map[string][]string
	convoys        map[string]string // bead -> tracking convoy
	stored         map[string][]beadFieldUpdates
	crew           map[string]bool         // "<rig>/<name>" crew members on disk
	hookedFormulas map[string]*beads.Issue // agent -> formula wisp hooked to it
	batchOpts      slingOptions            // what the last batch path was handed
}

const slingTestTown = "/town"

func newSlingHarness(t *testing.T) *slingHarness {
	t.Helper()
	h := &slingHarness{
		t:              t,
		out:            &bytes.Buffer{},
		beads:          map[string]*beadInfo{},
		env:            map[string]string{"GT_ROLE": "mayor"},
		rigs:           map[string]bool{"gastown": true},
		formulas:       map[string]bool{},
		dead:           map[string]bool{},
		molecules:      map[string][]string{},
		convoys:        map[string]string{},
		stored:         map[string][]beadFieldUpdates{},
		crew:           map[string]bool{},
		hookedFormulas: map[string]*beads.Issue{},
	}
	d := &slingDeps{
		getenv: func(k string) string { h.mu.Lock(); defer h.mu.Unlock(); return h.env[k] },
		out:    h.out,
		stdin:  strings.NewReader(""),

		autoCommitOff: func() func() {
			h.record("autocommit off")
			return func() { h.record("autocommit restore") }
		},
		releaseSeat: func() { h.record("release seat") },
		findTown:    func() (string, error) { return slingTestTown, nil },
		townOrEnv:   func() (string, error) { return slingTestTown, nil },

		resolvePRBranch:        func(pr int) (string, error) { return "", fmt.Errorf("unexpected PR lookup %d", pr) },
		workflowTargetOverride: func(args []string) ([]string, error) { return args, nil },
		shouldDefer:            func() (bool, error) { return false, nil },
		isRigName: func(target string) (string, bool) {
			if h.rigs[target] {
				return target, true
			}
			return "", false
		},
		idType: func(string) (string, error) { return "task", nil },
		batchSchedule: func(o slingOptions, ids []string, rig, _ string) error {
			h.record("batch schedule %s -> %s", strings.Join(ids, ","), rig)
			h.batchOpts = o
			return nil
		},
		batchSling: func(o slingOptions, ids []string, rig, _ string) error {
			h.record("batch sling %s -> %s", strings.Join(ids, ","), rig)
			h.batchOpts = o
			return nil
		},
		rigFromBeadIDs: func([]string, string) (string, error) { return "gastown", nil },
		scheduleBead: func(id, rig string, opts ScheduleOptions) error {
			h.record("schedule %s -> %s formula=%s", id, rig, opts.Formula)
			return nil
		},
		rigForBead:  func(string, string) string { return "gastown" },
		rigBeadsDir: func(town, rig string) (string, bool) { return town + "/" + rig + "/.beads", true },
		resolveFormula: func(explicit string, hookRaw bool, _, _ string) string {
			switch {
			case hookRaw:
				return ""
			case explicit != "":
				return explicit
			}
			return "mol-polecat-work"
		},
		slingFormula: func(_ context.Context, args []string) error {
			h.record("sling formula %s", strings.Join(args, " "))
			return nil
		},
		convoySchedule: func(id string, _ convoyScheduleOpts) error { h.record("convoy schedule %s", id); return nil },
		convoySling:    func(id string, _ convoyScheduleOpts) error { h.record("convoy sling %s", id); return nil },
		epicSchedule:   func(id string, _ epicScheduleOpts) error { h.record("epic schedule %s", id); return nil },
		epicSling:      func(id string, _ epicScheduleOpts) error { h.record("epic sling %s", id); return nil },

		verifyBead: func(id string) error {
			if h.bead(id) == nil {
				return fmt.Errorf("bead '%s' not found", id)
			}
			return nil
		},
		verifyFormula: func(name, _, _ string) error {
			if !h.formulas[name] {
				return fmt.Errorf("formula '%s' not found", name)
			}
			return nil
		},
		verifyInTargetRig: func(id, rig, _ string) error {
			if h.bead(id) == nil {
				return fmt.Errorf("bead %s is not present in target rig %q", id, rig)
			}
			return nil
		},
		beadInfo:       func(id string) (*beadInfo, error) { return h.info(id) },
		beadInfoInTown: func(_, id string) (*beadInfo, error) { return h.info(id) },
		lockBead: func(_, id string) (func(), error) {
			h.record("lock bead %s", id)
			return func() { h.record("unlock bead %s", id) }, nil
		},
		lockAssignee: func(_, agent string) (func(), error) {
			h.record("lock assignee %s", agent)
			return func() { h.record("unlock assignee %s", agent) }, nil
		},
		rigParked: func(string, string) (bool, string) { return false, "" },
		agentDead: func(a string) bool { h.mu.Lock(); defer h.mu.Unlock(); return h.dead[a] },
		survivingWorkGuard: func(_, id, holder string) error {
			h.record("survival guard %s %s", id, holder)
			return nil
		},
		checkDuplicates: func(string, string, *beadInfo) (*duplicateCandidate, []duplicateMatch, error) {
			return nil, nil, nil
		},
		noteDispatched: func(string, *duplicateCandidate) {},
		crossRigGuard: func(id, agent, _ string) error {
			h.record("cross-rig guard %s %s", id, agent)
			return nil
		},

		resolveSelf:   func() (string, string, string, error) { return "", "", "", errors.New("not in an agent session") },
		resolveTarget: h.resolveTarget,
		spawnPolecat: func(rig string, _ SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
			h.record("spawn %s", rig)
			return h.newSpawn(rig), nil
		},
		admitPolecat: func(_, rig, id, _ string) (*polecatAdmissionHandle, polecatCapacitySnapshot, error) {
			h.record("admit %s %s", rig, id)
			return &polecatAdmissionHandle{disabled: true}, polecatCapacitySnapshot{}, nil
		},
		startSession: func(s *SpawnedPolecatInfo) (string, error) {
			h.record("start session %s", s.PolecatName)
			return "%1", nil
		},
		startDelayedDog: func(dog *DogDispatchInfo) (string, error) {
			h.record("start dog %s", dog.DogName)
			return "%2", nil
		},
		cleanupSpawned: func(s *SpawnedPolecatInfo, _, convoyID string) {
			h.record("cleanup spawn %s convoy=%s", s.PolecatName, convoyID)
		},
		resolveAgent: func(target string) (string, string, string, error) {
			h.record("resolve agent %s", target)
			return "", "", "", errors.New("no session")
		},
		dispatchDog: func(name string, opts DogDispatchOptions) (*DogDispatchInfo, error) {
			if name == "" {
				name = "alpha"
			}
			h.record("dispatch dog %s", name)
			return &DogDispatchInfo{DogName: name, AgentID: "deacon/dogs/" + name,
				sessionDelayed: true, workDesc: opts.WorkDesc, ownsWork: true}, nil
		},
		cwdTown: func() string { return slingTestTown },
		crewExists: func(_, rig, name string) bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.crew[rig+"/"+name]
		},
		peekPool: func(string, string, string) (string, string, error) { return "", "", nil },
		wakeRig:  func(rig string) { h.record("wake rig %s", rig) },

		requester:       func() string { return "tester" },
		clearReassigned: func(_, assignee string) { h.record("clear reassigned %s", assignee) },
		unhook:          func(_, id string) error { h.record("unhook %s", id); return nil },
		recordReassignment: func(_, id, from, to, _ string) {
			h.record("reassign %s %s -> %s", id, from, to)
		},

		trackedByConvoy: func(id string) string {
			h.record("convoy lookup %s", id)
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.convoys[id]
		},
		createConvoy: func(id, _ string, _ bool, _, _, _, _ string) (string, error) {
			h.record("create convoy %s", id)
			return "hq-cv-auto", nil
		},
		collectMolecules: func(_ *beadInfo, id, _ string) ([]string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.molecules[id], nil
		},
		burnMolecules: func(mols []string, id, _ string) error {
			h.record("burn %s %s", id, strings.Join(mols, ","))
			return nil
		},
		rigCommandVars: func(string, string) []string { return nil },
		priorAttempt:   func(string, string) []string { return nil },
		cook:           func(name, _, _ string) error { h.record("cook %s", name); return nil },
		instantiateFormula: func(_ context.Context, name, id, _, _, _ string, _ bool, vars []string) (*FormulaOnBeadResult, error) {
			h.record("instantiate %s on %s vars=%s", name, id, strings.Join(vars, ","))
			return &FormulaOnBeadResult{WispRootID: "gt-wisp-new", BeadToHook: id}, nil
		},
		actor: func() string { return "mayor" },
		hookDir: func(town, _, workDir string) string {
			if workDir != "" {
				return workDir
			}
			return town
		},
		storeFields: func(_, id string, u beadFieldUpdates) error {
			h.mu.Lock()
			h.stored[id] = append(h.stored[id], u)
			h.mu.Unlock()
			h.record("store fields %s", id)
			return nil
		},
		hook: func(id, agent, _, _ string) error {
			h.mu.Lock()
			if b := h.beads[id]; b != nil {
				b.Status, b.Assignee = "hooked", agent
			}
			h.mu.Unlock()
			h.record("hook %s %s", id, agent)
			return nil
		},
		clearOrphanLabels: func(_, id, _ string) { h.record("clear orphan labels %s", id) },
		updateAgentHook:   func(agent, id, _, _ string) { h.record("agent hook %s %s", agent, id) },
		updateAgentMode:   func(agent, mode, _, _ string) { h.record("agent mode %s %s", agent, mode) },
		logFeed:           func(typ, _ string, _ map[string]interface{}) error { h.record("feed %s", typ); return nil },
		enqueueNudge: func(_, session string, n nudge.QueuedNudge) error {
			h.record("queue nudge %s: %s", session, n.Message)
			return nil
		},

		findHookedFormula: func(_, agent, _ string) (*beads.Issue, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.hookedFormulas[agent], nil
		},
		cookFormula: func(name, _, _ string) error { h.record("cook formula %s", name); return nil },
		createWisp: func(name, _, _ string, vars []string) ([]byte, error) {
			h.record("create wisp %s vars=%s", name, strings.Join(vars, ","))
			return []byte(`{"root_id":"gt-wisp-new"}`), nil
		},
		hookWisp:             func(id, agent, _ string) error { h.record("hook %s %s", id, agent); return nil },
		burnWisp:             func(id, _ string) error { h.record("burn wisp %s", id); return nil },
		cleanupFailedDogWisp: func(id, _ string) error { h.record("cleanup dog wisp %s", id); return nil },
		cleanupStaleDogWisp:  func(id, _ string) error { h.record("cleanup stale dog wisp %s", id); return nil },
		clearDogWork: func(dog *DogDispatchInfo) error {
			if dog != nil && dog.ownsWork {
				h.record("clear dog work %s", dog.DogName)
			}
			return nil
		},
		nudgeSession: func(session, msg string) error { h.record("nudge session %s: %s", session, msg); return nil },
		nudgePane:    func(pane, msg string) error { h.record("nudge pane %s: %s", pane, msg); return nil },

		slingContexts: func(string) slingContextStore { return fakeSlingContexts{h} },

		rollbackArtifacts: func(s *SpawnedPolecatInfo, id, _, convoyID string) {
			name := ""
			if s != nil {
				name = s.PolecatName
			}
			h.record("rollback %s bead=%s convoy=%s", name, id, convoyID)
		},
		restoreRawFields: func(id, _, _ string, _ *beadInfo) { h.record("restore raw fields %s", id) },
		restorePinned:    func(_, id, assignee string) { h.record("restore pinned %s %s", id, assignee) },

		sessionFromPane:  func(string) string { return "" },
		ensureAgentReady: func(string) error { return nil },
		injectStartPrompt: func(pane, id, _, _ string) error {
			h.record("nudge %s %s", pane, id)
			return nil
		},
	}
	h.run = &slingRun{slingDeps: d, townRoot: slingTestTown, opts: slingOptions{noBoot: true}}
	return h
}

// resolveTarget is the fake target resolution: a rig spawns polecat Toast
// (without --dry-run), anything else is an existing agent in pane %9.
func (h *slingHarness) resolveTarget(target string, opts ResolveTargetOptions) (*ResolvedTarget, error) {
	h.record("resolve target %q", target)
	if h.rigs[target] {
		if opts.DryRun {
			return &ResolvedTarget{Agent: target + "/polecats/<new>"}, nil
		}
		spawn := h.newSpawn(target)
		return &ResolvedTarget{Agent: spawn.AgentID(), WorkDir: spawn.ClonePath, NewPolecatInfo: spawn}, nil
	}
	if target == "" || target == "." {
		agent, pane, root, err := h.run.resolveSelf()
		if err != nil {
			return nil, err
		}
		return &ResolvedTarget{Agent: agent, Pane: pane, WorkDir: root, IsSelfSling: true}, nil
	}
	if target == "mayor" {
		target = "mayor/" // the town singleton's address
	}
	return &ResolvedTarget{Agent: target, Pane: "%9"}, nil
}

// fakeSlingContexts is a rig's sling contexts: none exist, and every lookup
// and write lands in the call log.
type fakeSlingContexts struct{ h *slingHarness }

func (f fakeSlingContexts) FindOpenSlingContext(id string) (*beads.Issue, *capacity.SlingContextFields, error) {
	f.h.record("find context %s", id)
	return nil, nil, nil
}

func (f fakeSlingContexts) CreateSlingContext(_, id string, fields *capacity.SlingContextFields) (*beads.Issue, error) {
	f.h.record("create context %s -> %s", id, fields.TargetRig)
	return &beads.Issue{ID: "gt-ctx-new"}, nil
}

func (f fakeSlingContexts) UpdateSlingContextFields(id string, fields *capacity.SlingContextFields) error {
	f.h.record("update context %s convoy=%s", id, fields.Convoy)
	return nil
}

func (h *slingHarness) newSpawn(rig string) *SpawnedPolecatInfo {
	return &SpawnedPolecatInfo{RigName: rig, PolecatName: "Toast", ClonePath: slingTestTown + "/" + rig + "/polecats/Toast",
		Branch: "polecat/Toast/x", FreshSpawn: true, BranchCreated: true}
}

// sling runs gt sling with args.
func (h *slingHarness) sling(args ...string) error {
	return h.run.run(context.Background(), nil, args)
}

// addBead puts a bead in the world.
func (h *slingHarness) addBead(id string, info beadInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if info.Title == "" {
		info.Title = "Test issue"
	}
	if info.Status == "" {
		info.Status = "open"
	}
	h.beads[id] = &info
}

func (h *slingHarness) bead(id string) *beadInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.beads[id]
}

func (h *slingHarness) info(id string) (*beadInfo, error) {
	b := h.bead(id)
	if b == nil {
		return nil, fmt.Errorf("bead '%s' not found", id)
	}
	c := *b
	return &c, nil
}

func (h *slingHarness) record(format string, args ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, fmt.Sprintf(format, args...))
}

// log is the call log so far.
func (h *slingHarness) log() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// matching is every logged call that starts with prefix.
func (h *slingHarness) matching(prefix string) []string {
	var out []string
	for _, c := range h.log() {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// wantCalls fails unless the log holds exactly want among the calls that
// start with prefix.
func (h *slingHarness) wantCalls(prefix string, want ...string) {
	h.t.Helper()
	if got := h.matching(prefix); strings.Join(got, "\n") != strings.Join(want, "\n") {
		h.t.Errorf("%q calls = %q, want %q\nfull log:\n  %s", prefix, got, want, strings.Join(h.log(), "\n  "))
	}
}

// wantNo fails if any logged call starts with prefix.
func (h *slingHarness) wantNo(prefix string) {
	h.t.Helper()
	h.wantCalls(prefix)
}

// wantErr fails unless err is non-nil and names sub.
func wantSlingErr(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil {
		t.Fatalf("sling succeeded, want an error naming %q", sub)
	}
	if !strings.Contains(err.Error(), sub) {
		t.Fatalf("sling error %q does not name %q", err, sub)
	}
}
