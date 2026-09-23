# polecat_pool N Agent Tiers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let `polecat_pool` cap any number of agent classes, and count every live polecat whether or not it is capped.

**Architecture:** `PolecatPool` gains an ordered `Tiers []PoolTier`. A single adapter, `EffectiveTiers()`, returns those tiers or synthesizes `[local, overflow]` from the four legacy fields, so every consumer reads one normalized list. `choosePoolAgent` keeps its existing cascade structure and iterates that list instead of two named fields.

**Tech Stack:** Go, `encoding/json` struct tags, Cobra CLI, testify-free table tests (match the style already in `internal/cmd/sling_pool_test.go`).

**Spec:** `docs/plans/2026-09-23-polecat-pool-n-tiers-design.md` (commit `694b823`)

**Bead:** gt-xmsqb (P2)

## Global Constraints

- No Claude/Anthropic/AI attribution in commits, code, comments or docs. All work appears human-authored.
- After every `git commit`, run `bd comments add gt-xmsqb "commit: <short-hash> — <summary>"`.
- Push with the full form: `git push origin <branch>`. Never a bare `git push`.
- **Never commit `settings/config.json`** — it carries API keys. Do not add it to any commit, and do not rewrite it mechanically.
- Test: `GOFLAGS=-p=8 make test` · Lint: `make lint` · Build: `make build`.
- `choosePoolAgent` must stay **pure**. Its one side effect, attaching `localAttemptLabel`, belongs to the caller. Do not add side effects to it.
- Preserve the cascade's *ordering*. Four closed bugs paid for it: gt-x40u, gt-4lbz, gt-nn7n, gt-gcrk. This change alters what the cascade iterates over, never when it refuses.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/config/types.go` (modify) | `PoolTier` type, `Tiers` field, `EffectiveTiers()` adapter |
| `internal/config/types_test.go` (modify) | legacy-synthesis behaviour of `EffectiveTiers()` |
| `internal/cmd/pool_tiers.go` (create) | tier lookup, per-tier counting, shape-routed tier order |
| `internal/cmd/pool_tiers_test.go` (create) | unit tests for the above, no routing involved |
| `internal/cmd/sling_pool.go` (modify) | `choosePoolAgent` and `claimFor` read tiers |
| `internal/cmd/sling_pool_test.go` (modify) | legacy parity, `request_only`, refusal cases |
| `internal/cmd/daemon_dispatch.go` (modify) | `dispatchSeats.Untiered`, `poolSeatPicture` counts everything |
| `internal/cmd/daemon_dispatch_test.go` (modify) | untiered counted but never capping |

`internal/cmd/sling_pool.go` is already 971 lines. The tier model goes in its own file so routing does not grow past ~1,100 lines and so the tier model can be tested without reaching through routing.

---

### Task 1: `PoolTier` and the `EffectiveTiers()` adapter

Pure config. No behaviour change anywhere yet — this task only makes the new shape representable and proves legacy configs synthesize correctly.

**Files:**
- Modify: `internal/config/types.go` (near `PolecatPool`, around line 2066)
- Test: `internal/config/types_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `config.PoolTier{Agent string; Max int; RequestOnly bool}` and `func (p *PolecatPool) EffectiveTiers() []PoolTier`.

- [ ] **Step 1: Write the failing test**

In `internal/config/types_test.go`:

```go
func TestEffectiveTiers(t *testing.T) {
	tests := []struct {
		name string
		pool *PolecatPool
		want []PoolTier
	}{
		{
			name: "nil pool has no tiers",
			pool: nil,
			want: nil,
		},
		{
			name: "explicit tiers are returned as written",
			pool: &PolecatPool{
				Tiers: []PoolTier{
					{Agent: "local-coder-polecat", Max: 1},
					{Agent: "deepseek-flash", Max: 1},
					{Agent: "claude-sonnet", Max: 1, RequestOnly: true},
				},
			},
			want: []PoolTier{
				{Agent: "local-coder-polecat", Max: 1},
				{Agent: "deepseek-flash", Max: 1},
				{Agent: "claude-sonnet", Max: 1, RequestOnly: true},
			},
		},
		{
			name: "legacy fields synthesize two tiers",
			pool: &PolecatPool{
				LocalAgent:    "local-coder-polecat",
				MaxLocal:      1,
				OverflowAgent: "deepseek-flash",
				MaxOverflow:   2,
			},
			want: []PoolTier{
				{Agent: "local-coder-polecat", Max: 1},
				{Agent: "deepseek-flash", Max: 2},
			},
		},
		{
			name: "legacy with no overflow agent synthesizes one tier",
			pool: &PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 1},
			want: []PoolTier{{Agent: "local-coder-polecat", Max: 1}},
		},
		{
			name: "legacy uncapped overflow keeps max zero",
			pool: &PolecatPool{
				LocalAgent:    "local-coder-polecat",
				MaxLocal:      1,
				OverflowAgent: "deepseek-flash",
			},
			want: []PoolTier{
				{Agent: "local-coder-polecat", Max: 1},
				{Agent: "deepseek-flash", Max: 0},
			},
		},
		{
			name: "explicit tiers win over legacy fields",
			pool: &PolecatPool{
				LocalAgent:    "legacy-local",
				MaxLocal:      9,
				OverflowAgent: "legacy-overflow",
				MaxOverflow:   9,
				Tiers:         []PoolTier{{Agent: "only-tier", Max: 1}},
			},
			want: []PoolTier{{Agent: "only-tier", Max: 1}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.pool.EffectiveTiers()
			if len(got) != len(tt.want) {
				t.Fatalf("EffectiveTiers() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("tier %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}
```

Note the fifth case: a legacy pool with an overflow agent and no `max_overflow` synthesizes `Max: 0`, which the tier model reads as **uncapped for a synthesized overflow tier**. Task 2 pins that meaning; do not "fix" it here.

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/config/ -run TestEffectiveTiers -v`
Expected: FAIL to compile — `undefined: PoolTier`.

- [ ] **Step 3: Add the type and the adapter**

In `internal/config/types.go`, immediately after the `PolecatPool` struct:

```go
// PoolTier is one agent class the pool admits and counts. Tiers are ordered:
// earlier tiers are preferred, and a bead that cannot take one falls through
// to the next.
type PoolTier struct {
	// Agent is the agent alias this tier admits.
	Agent string `json:"agent"`
	// Max is the number of live polecat sessions allowed on Agent.
	// Zero means closed for an explicitly configured tier, matching
	// max_local: 0; see EffectiveTiers for the synthesized-overflow case.
	Max int `json:"max"`
	// RequestOnly keeps a tier out of shape routing and out of the
	// fall-through cascade: it is reachable only by a caller naming its
	// agent with --agent. Without it an overflow-shaped bead would cascade
	// onto the most expensive seat configured.
	RequestOnly bool `json:"request_only,omitempty"`
}
```

Add the field to `PolecatPool`, after `IdleFill`:

```go
	// Tiers, when set, replaces LocalAgent/MaxLocal/OverflowAgent/MaxOverflow
	// as the pool's seat model. The legacy fields remain supported: see
	// EffectiveTiers.
	Tiers []PoolTier `json:"tiers,omitempty"`
```

Add the adapter after `IdleFillEnabled`:

```go
// EffectiveTiers returns the pool's seat model as an ordered tier list. A pool
// that sets Tiers is returned as written. A pool that does not is synthesized
// from the legacy local/overflow fields, so both config forms flow through one
// normalization point and cannot diverge in behaviour.
func (p *PolecatPool) EffectiveTiers() []PoolTier {
	if p == nil {
		return nil
	}
	if len(p.Tiers) > 0 {
		return p.Tiers
	}
	if p.LocalAgent == "" {
		return nil
	}
	tiers := []PoolTier{{Agent: p.LocalAgent, Max: p.MaxLocal}}
	if p.OverflowAgent != "" {
		tiers = append(tiers, PoolTier{Agent: p.OverflowAgent, Max: p.MaxOverflow})
	}
	return tiers
}
```

- [ ] **Step 4: Run the test and confirm it passes**

Run: `go test ./internal/config/ -run TestEffectiveTiers -v`
Expected: PASS, all six subtests.

- [ ] **Step 5: Commit**

```bash
git add internal/config/types.go internal/config/types_test.go
git commit -m "feat(config): add PoolTier and the EffectiveTiers adapter (gt-xmsqb)"
bd comments add gt-xmsqb "commit: $(git rev-parse --short HEAD) — PoolTier type and EffectiveTiers adapter; legacy four-field configs synthesize to tiers"
```

---

### Task 2: Tier lookup and per-tier counting

Pure functions in a new file. No routing changes yet.

**Files:**
- Create: `internal/cmd/pool_tiers.go`
- Test: `internal/cmd/pool_tiers_test.go`

**Interfaces:**
- Consumes: `config.PoolTier`, `config.PolecatPool.EffectiveTiers()` from Task 1; the existing `poolSession` struct in `internal/cmd/sling_pool.go` (`{name string; agent string; created time.Time}`).
- Produces:
  - `func poolTierIndex(tiers []config.PoolTier, agent string) int` — index, or `-1` when the agent is in no tier.
  - `func poolTierCapped(t config.PoolTier, synthesizedOverflow bool) bool`
  - `func poolSeatCountsByTier(tiers []config.PoolTier, sessions []poolSession) (perTier []int, untiered int, newestFirstTier time.Time)`
  - `func shapeRoutedTiers(tiers []config.PoolTier) []int` — indices of non-`RequestOnly` tiers, in order.

- [ ] **Step 1: Write the failing test**

In `internal/cmd/pool_tiers_test.go`:

```go
package cmd

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

func TestPoolTierIndex(t *testing.T) {
	tiers := []config.PoolTier{
		{Agent: "local-coder-polecat", Max: 1},
		{Agent: "deepseek-flash", Max: 1},
	}
	if got := poolTierIndex(tiers, "deepseek-flash"); got != 1 {
		t.Errorf("poolTierIndex(deepseek-flash) = %d, want 1", got)
	}
	if got := poolTierIndex(tiers, "claude-opus"); got != -1 {
		t.Errorf("poolTierIndex(claude-opus) = %d, want -1 for an untiered agent", got)
	}
	if got := poolTierIndex(nil, "anything"); got != -1 {
		t.Errorf("poolTierIndex(nil, ...) = %d, want -1", got)
	}
}

func TestPoolSeatCountsByTier(t *testing.T) {
	tiers := []config.PoolTier{
		{Agent: "local-coder-polecat", Max: 1},
		{Agent: "deepseek-flash", Max: 1},
		{Agent: "claude-sonnet", Max: 1, RequestOnly: true},
	}
	older := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 9, 23, 11, 0, 0, 0, time.UTC)
	sessions := []poolSession{
		{name: "mica", agent: "local-coder-polecat", created: older},
		{name: "marble", agent: "local-coder-polecat", created: newer},
		{name: "amber", agent: "deepseek-flash", created: newer},
		{name: "quartz", agent: "claude-sonnet", created: newer},
		{name: "oneoff", agent: "claude-opus", created: newer},
	}

	perTier, untiered, newestFirst := poolSeatCountsByTier(tiers, sessions)

	if len(perTier) != 3 {
		t.Fatalf("perTier length = %d, want 3", len(perTier))
	}
	if perTier[0] != 2 || perTier[1] != 1 || perTier[2] != 1 {
		t.Errorf("perTier = %v, want [2 1 1]", perTier)
	}
	// An untiered agent is COUNTED but not capped: this is the number that
	// today's capacity picture omits entirely.
	if untiered != 1 {
		t.Errorf("untiered = %d, want 1 (claude-opus is in no tier)", untiered)
	}
	if !newestFirst.Equal(newer) {
		t.Errorf("newestFirstTier = %v, want %v", newestFirst, newer)
	}
}

func TestShapeRoutedTiers(t *testing.T) {
	tiers := []config.PoolTier{
		{Agent: "local-coder-polecat", Max: 1},
		{Agent: "deepseek-flash", Max: 1},
		{Agent: "claude-sonnet", Max: 1, RequestOnly: true},
	}
	got := shapeRoutedTiers(tiers)
	if len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Fatalf("shapeRoutedTiers = %v, want [0 1] — a request_only tier must not be shape-routed", got)
	}
}

func TestPoolTierCapped(t *testing.T) {
	// An explicitly configured tier with max 0 is CLOSED, matching max_local: 0.
	if poolTierCapped(config.PoolTier{Agent: "a", Max: 0}, false) != true {
		t.Error("an explicit tier with max 0 must be capped (closed), not uncapped")
	}
	// A synthesized overflow tier with max 0 is UNCAPPED, matching
	// OverflowCapped()'s existing meaning.
	if poolTierCapped(config.PoolTier{Agent: "a", Max: 0}, true) != false {
		t.Error("a synthesized overflow tier with max 0 must be uncapped")
	}
	if poolTierCapped(config.PoolTier{Agent: "a", Max: 2}, true) != true {
		t.Error("a tier with a positive max is always capped")
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/cmd/ -run 'TestPoolTierIndex|TestPoolSeatCountsByTier|TestShapeRoutedTiers|TestPoolTierCapped' -v`
Expected: FAIL to compile — `undefined: poolTierIndex`.

- [ ] **Step 3: Write the implementation**

Create `internal/cmd/pool_tiers.go`:

```go
package cmd

import (
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// poolTierIndex returns the index of the tier that admits agent, or -1 when no
// tier does. An agent in no tier is one the pool does not own: it is counted
// for reporting but never capped and never refused (gt-4lbz, gt-gcrk).
func poolTierIndex(tiers []config.PoolTier, agent string) int {
	if agent == "" {
		return -1
	}
	for i, t := range tiers {
		if t.Agent == agent {
			return i
		}
	}
	return -1
}

// poolTierCapped reports whether a tier bounds its agent. An explicitly
// configured tier with max 0 is CLOSED — the same meaning max_local: 0 carries,
// which a town uses to turn local dispatch off (gt-4lbz). A tier synthesized
// from a legacy overflow_agent with no max_overflow is UNCAPPED, preserving
// OverflowCapped(). The asymmetry is deliberate; do not unify it.
func poolTierCapped(t config.PoolTier, synthesizedOverflow bool) bool {
	if synthesizedOverflow {
		return t.Max > 0
	}
	return true
}

// poolSeatCountsByTier counts live polecat sessions against each tier, counts
// the sessions no tier owns, and reports the newest spawn in the first tier —
// the timestamp the stagger guard reads.
//
// Counting and capping are separate concerns. Every live polecat is counted,
// including one on an agent the pool does not own, because an uncounted
// session still spends money and host load; only tiered sessions are capped.
func poolSeatCountsByTier(tiers []config.PoolTier, sessions []poolSession) (perTier []int, untiered int, newestFirstTier time.Time) {
	perTier = make([]int, len(tiers))
	for _, s := range sessions {
		idx := poolTierIndex(tiers, s.agent)
		if idx < 0 {
			untiered++
			continue
		}
		perTier[idx]++
		if idx == 0 && s.created.After(newestFirstTier) {
			newestFirstTier = s.created
		}
	}
	return perTier, untiered, newestFirstTier
}

// shapeRoutedTiers returns the indices of the tiers shape routing and the
// fall-through cascade may use, in order. A request_only tier is excluded: it
// is reachable solely by a caller naming its agent, so that an overflow-shaped
// bead cannot cascade onto the most expensive seat configured.
func shapeRoutedTiers(tiers []config.PoolTier) []int {
	idx := make([]int, 0, len(tiers))
	for i, t := range tiers {
		if !t.RequestOnly {
			idx = append(idx, i)
		}
	}
	return idx
}
```

- [ ] **Step 4: Run the test and confirm it passes**

Run: `go test ./internal/cmd/ -run 'TestPoolTierIndex|TestPoolSeatCountsByTier|TestShapeRoutedTiers|TestPoolTierCapped' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cmd/pool_tiers.go internal/cmd/pool_tiers_test.go
git commit -m "feat(pool): tier lookup and per-tier seat counting (gt-xmsqb)"
bd comments add gt-xmsqb "commit: $(git rev-parse --short HEAD) — pool_tiers.go: poolTierIndex, poolTierCapped, poolSeatCountsByTier, shapeRoutedTiers"
```

---

### Task 3: Route from tiers, with legacy parity proven

The load-bearing task. `choosePoolAgent` and `claimFor` stop reading `LocalAgent`/`OverflowAgent` directly and read the normalized tier list. Behaviour for every existing config must be unchanged.

**Files:**
- Modify: `internal/cmd/sling_pool.go` — `poolSeatCounts` (line ~144), `choosePoolAgent` (line ~183), `claimFor` (line ~644)
- Test: `internal/cmd/sling_pool_test.go`

**Interfaces:**
- Consumes: everything Task 2 produced, plus `config.PolecatPool.EffectiveTiers()` from Task 1.
- Produces: no new exported names. `choosePoolAgent` keeps its exact signature: `func choosePoolAgent(pool *config.PolecatPool, bead poolBead, requested string, sessions []poolSession, now time.Time) (agent, reason string, refused bool)`.

- [ ] **Step 1: Write the failing legacy-parity test**

This test is the reason the task exists. It names each prior bug so a future reader knows which case is load-bearing.

In `internal/cmd/sling_pool_test.go`:

```go
// TestChoosePoolAgentLegacyParity pins the routing decisions a four-field
// legacy config produced before tiers existed. Each case names the bug that
// established it; a failure here means the tier normalization reordered a case
// that someone already paid for.
func TestChoosePoolAgentLegacyParity(t *testing.T) {
	legacy := &config.PolecatPool{
		LocalAgent:    "local-coder-polecat",
		MaxLocal:      1,
		OverflowAgent: "deepseek-flash",
		MaxOverflow:   2,
	}
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)

	tests := []struct {
		name        string
		bead        poolBead
		requested   string
		sessions    []poolSession
		wantAgent   string
		wantRefused bool
	}{
		{
			name:      "gt-x40u: explicit local request with the local seat free is served by local",
			bead:      poolBead{Type: "bug"},
			requested: "local-coder-polecat",
			sessions:  nil,
			wantAgent: "local-coder-polecat",
		},
		{
			name:        "gt-x40u: explicit local request with the local seat full is REFUSED, never swapped to the paid seat",
			bead:        poolBead{Type: "bug"},
			requested:   "local-coder-polecat",
			sessions:    []poolSession{{agent: "local-coder-polecat", created: old}},
			wantAgent:   "local-coder-polecat",
			wantRefused: true,
		},
		{
			name:      "gt-4lbz: an agent the pool does not own is left untouched",
			bead:      poolBead{Type: "bug"},
			requested: "claude-sonnet",
			sessions:  nil,
			wantAgent: "",
		},
		{
			name:      "gt-nn7n: a chore takes the free local seat",
			bead:      poolBead{Type: "chore"},
			sessions:  nil,
			wantAgent: "local-coder-polecat",
		},
		{
			name:      "local full sends a chore to the overflow seat",
			bead:      poolBead{Type: "chore"},
			sessions:  []poolSession{{agent: "local-coder-polecat", created: old}},
			wantAgent: "deepseek-flash",
		},
		{
			name: "overflow full refuses",
			bead: poolBead{Type: "bug"},
			sessions: []poolSession{
				{agent: "local-coder-polecat", created: old},
				{agent: "deepseek-flash", created: old},
				{agent: "deepseek-flash", created: old},
			},
			wantAgent:   "deepseek-flash",
			wantRefused: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent, reason, refused := choosePoolAgent(legacy, tt.bead, tt.requested, tt.sessions, now)
			if agent != tt.wantAgent {
				t.Errorf("agent = %q, want %q (reason: %s)", agent, tt.wantAgent, reason)
			}
			if refused != tt.wantRefused {
				t.Errorf("refused = %v, want %v (reason: %s)", refused, tt.wantRefused, reason)
			}
			if reason == "" && agent != "" {
				t.Error("every decision that names an agent must carry a reason")
			}
		})
	}
}

// TestChoosePoolAgentTieredMatchesLegacy proves the two config forms are the
// same seat model: the tier form of today's live config must produce identical
// decisions to the legacy form, case for case.
func TestChoosePoolAgentTieredMatchesLegacy(t *testing.T) {
	legacy := &config.PolecatPool{
		LocalAgent:    "local-coder-polecat",
		MaxLocal:      1,
		OverflowAgent: "deepseek-flash",
		MaxOverflow:   2,
	}
	tiered := &config.PolecatPool{
		Tiers: []config.PoolTier{
			{Agent: "local-coder-polecat", Max: 1},
			{Agent: "deepseek-flash", Max: 2},
		},
	}
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)

	cases := []struct {
		bead      poolBead
		requested string
		sessions  []poolSession
	}{
		{bead: poolBead{Type: "chore"}},
		{bead: poolBead{Type: "bug"}},
		{bead: poolBead{Type: "chore"}, sessions: []poolSession{{agent: "local-coder-polecat", created: old}}},
		{bead: poolBead{Type: "bug"}, requested: "local-coder-polecat"},
		{bead: poolBead{Type: "bug"}, requested: "claude-sonnet"},
	}

	for i, c := range cases {
		lAgent, _, lRefused := choosePoolAgent(legacy, c.bead, c.requested, c.sessions, now)
		tAgent, _, tRefused := choosePoolAgent(tiered, c.bead, c.requested, c.sessions, now)
		if lAgent != tAgent || lRefused != tRefused {
			t.Errorf("case %d: legacy (%q,%v) != tiered (%q,%v)", i, lAgent, lRefused, tAgent, tRefused)
		}
	}
}
```

- [ ] **Step 2: Run the tests and confirm the parity test fails**

Run: `go test ./internal/cmd/ -run 'TestChoosePoolAgentLegacyParity|TestChoosePoolAgentTieredMatchesLegacy' -v`
Expected: `TestChoosePoolAgentLegacyParity` PASSES against current code (it describes today's behaviour), and `TestChoosePoolAgentTieredMatchesLegacy` FAILS — the tiered pool has no `LocalAgent`, so current code returns `""` with "no local_agent" for every case.

This is the correct RED: the parity test is a characterization test that must be green before and after, and the tiered test is the one that drives the change.

- [ ] **Step 3: Rewire the three call sites to read tiers**

In `internal/cmd/sling_pool.go`:

1. Replace the body of `poolSeatCounts` so it delegates, keeping its current signature for existing callers:

```go
func poolSeatCounts(pool *config.PolecatPool, sessions []poolSession) (local, overflow int, newestLocal time.Time) {
	tiers := pool.EffectiveTiers()
	if len(tiers) == 0 {
		return 0, 0, time.Time{}
	}
	perTier, _, newest := poolSeatCountsByTier(tiers, sessions)
	local = perTier[0]
	if len(perTier) > 1 {
		overflow = perTier[1]
	}
	return local, overflow, newest
}
```

2. In `choosePoolAgent`, replace every direct read of `pool.LocalAgent` and `pool.OverflowAgent` with the tier list. Derive them once at the top so the cascade body below is untouched:

```go
	tiers := pool.EffectiveTiers()
	if len(tiers) == 0 {
		return "", "pool: polecat_pool has no tiers configured; using the role default", false
	}
	routed := shapeRoutedTiers(tiers)
	if len(routed) == 0 {
		return "", "pool: polecat_pool has no shape-routed tiers; using the role default", false
	}
	firstTier := tiers[routed[0]]
	var lastTier config.PoolTier
	if len(routed) > 1 {
		lastTier = tiers[routed[len(routed)-1]]
	}
```

Then substitute `firstTier.Agent` for `pool.LocalAgent`, `firstTier.Max` for `pool.MaxLocal`, `lastTier.Agent` for `pool.OverflowAgent`, and `lastTier.Max` for `pool.MaxOverflow` throughout the existing cascade. **Do not reorder any case.** Keep every reason string's wording; only the values interpolated into it change.

3. In `claimFor`, replace the two-tier check:

```go
	tiers := pool.EffectiveTiers()
	idx := poolTierIndex(tiers, agent)
	if idx < 0 {
		return // an agent the pool does not own holds no seat claim
	}
	synthesized := len(pool.Tiers) == 0 && idx > 0
	if !poolTierCapped(tiers[idx], synthesized) {
		return
	}
```

- [ ] **Step 4: Run both tests and confirm they pass**

Run: `go test ./internal/cmd/ -run 'TestChoosePoolAgentLegacyParity|TestChoosePoolAgentTieredMatchesLegacy' -v`
Expected: both PASS.

Then run the whole package to catch any caller you moved out from under:
Run: `go test ./internal/cmd/ -run 'Pool|Sling' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cmd/sling_pool.go internal/cmd/sling_pool_test.go
git commit -m "refactor(pool): route from the normalized tier list (gt-xmsqb)"
bd comments add gt-xmsqb "commit: $(git rev-parse --short HEAD) — choosePoolAgent/poolSeatCounts/claimFor read EffectiveTiers; legacy parity pinned by test naming gt-x40u, gt-4lbz, gt-nn7n"
```

---

### Task 4: `request_only` tiers

With routing reading tiers, a third tier is now expressible. This task makes it safe.

**Files:**
- Modify: `internal/cmd/sling_pool.go` — the explicit-request branch of `choosePoolAgent`
- Test: `internal/cmd/sling_pool_test.go`

**Interfaces:**
- Consumes: `shapeRoutedTiers`, `poolTierIndex` from Task 2; the tier-reading `choosePoolAgent` from Task 3.
- Produces: no new names.

- [ ] **Step 1: Write the failing test**

```go
func TestChoosePoolAgentRequestOnlyTier(t *testing.T) {
	pool := &config.PolecatPool{
		Tiers: []config.PoolTier{
			{Agent: "local-coder-polecat", Max: 1},
			{Agent: "deepseek-flash", Max: 1},
			{Agent: "claude-sonnet", Max: 1, RequestOnly: true},
		},
	}
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	full := []poolSession{
		{agent: "local-coder-polecat", created: old},
		{agent: "deepseek-flash", created: old},
	}

	t.Run("a chore with every shape-routed tier full is refused, never cascaded to the request_only tier", func(t *testing.T) {
		agent, reason, refused := choosePoolAgent(pool, poolBead{Type: "chore"}, "", full, now)
		if !refused {
			t.Fatalf("want refused, got agent %q (reason: %s)", agent, reason)
		}
		if agent == "claude-sonnet" {
			t.Fatal("a shape-routed cascade reached the request_only tier — this is the case that makes a spend cap INCREASE spend")
		}
	})

	t.Run("an explicit request for the request_only tier is served when it has room", func(t *testing.T) {
		agent, reason, refused := choosePoolAgent(pool, poolBead{Type: "bug"}, "claude-sonnet", full, now)
		if refused || agent != "claude-sonnet" {
			t.Errorf("agent = %q refused = %v, want claude-sonnet served (reason: %s)", agent, refused, reason)
		}
	})

	t.Run("an explicit request for a full request_only tier is refused naming that tier", func(t *testing.T) {
		busy := append(append([]poolSession{}, full...), poolSession{agent: "claude-sonnet", created: old})
		agent, reason, refused := choosePoolAgent(pool, poolBead{Type: "bug"}, "claude-sonnet", busy, now)
		if !refused {
			t.Fatalf("want refused, got agent %q (reason: %s)", agent, reason)
		}
		if agent != "claude-sonnet" {
			t.Errorf("a refused request must name the seat the caller asked for, got %q (gt-x40u)", agent)
		}
		if !strings.Contains(reason, "claude-sonnet") {
			t.Errorf("reason %q must name the tier that was full", reason)
		}
	})
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/cmd/ -run TestChoosePoolAgentRequestOnlyTier -v`
Expected: FAIL on the first subtest — the cascade currently walks every tier, including the `request_only` one.

- [ ] **Step 3: Implement**

In `choosePoolAgent`'s explicit-request branch, admit a requested tier by its own rules regardless of `RequestOnly`:

```go
	if requested != "" {
		if idx := poolTierIndex(tiers, requested); idx >= 0 {
			synthesized := len(pool.Tiers) == 0 && idx > 0
			if poolTierCapped(tiers[idx], synthesized) && perTier[idx] >= tiers[idx].Max {
				return requested, fmt.Sprintf("pool: tier %s full (%d/%d) -> no seat (requested %s)",
					tiers[idx].Agent, perTier[idx], tiers[idx].Max, requested), true
			}
			return requested, fmt.Sprintf("pool: tier %s %d/%d -> %s",
				tiers[idx].Agent, perTier[idx]+1, tiers[idx].Max, requested), false
		}
		// An agent the pool does not own: leave the request untouched, ahead
		// of the spent-local-attempt rule (gt-4lbz, gt-gcrk).
		return "", "", false
	}
```

The shape-routing and cascade code below must iterate `routed` (the `shapeRoutedTiers` indices) rather than all tiers, which Task 3 already set up.

- [ ] **Step 4: Run the test and confirm it passes**

Run: `go test ./internal/cmd/ -run TestChoosePoolAgentRequestOnlyTier -v`
Expected: PASS, all three subtests.

Re-run parity to prove nothing regressed:
Run: `go test ./internal/cmd/ -run 'TestChoosePoolAgentLegacyParity|TestChoosePoolAgentTieredMatchesLegacy' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cmd/sling_pool.go internal/cmd/sling_pool_test.go
git commit -m "feat(pool): request_only tiers are reachable only by an explicit --agent (gt-xmsqb)"
bd comments add gt-xmsqb "commit: $(git rev-parse --short HEAD) — request_only keeps shape routing and the cascade off the most expensive seat"
```

---

### Task 5: Count untiered sessions without capping on them

**Files:**
- Modify: `internal/cmd/daemon_dispatch.go` — `dispatchSeats` (line ~56), `poolSeatPicture` (line ~300)
- Test: `internal/cmd/daemon_dispatch_test.go`

**Interfaces:**
- Consumes: `poolSeatCountsByTier` from Task 2; `EffectiveTiers()` from Task 1.
- Produces: `dispatchSeats.Untiered int` with JSON tag `untiered,omitempty`.

- [ ] **Step 1: Write the failing test**

```go
func TestPoolSeatPictureCountsUntiered(t *testing.T) {
	pool := &config.PolecatPool{
		Tiers: []config.PoolTier{
			{Agent: "local-coder-polecat", Max: 1},
			{Agent: "deepseek-flash", Max: 1},
		},
	}
	sessions := []poolSession{
		{agent: "local-coder-polecat"},
		{agent: "claude-opus"}, // in no tier
	}

	seats := poolSeatPicture(pool, sessions)

	if seats.Capacity != 2 {
		t.Errorf("Capacity = %d, want 2 (the sum of tier maxima)", seats.Capacity)
	}
	if seats.Occupied != 1 {
		t.Errorf("Occupied = %d, want 1 — only tiered sessions occupy tiered seats", seats.Occupied)
	}
	if seats.Untiered != 1 {
		t.Errorf("Untiered = %d, want 1 — this is the number today's picture omits", seats.Untiered)
	}
	// The load-bearing assertion: an untiered session must NOT consume a free
	// seat. dispatchDecision gates on Free < 1, so folding untiered into
	// Occupied would let one stray --agent session refuse every tiered sling.
	if seats.Free != 1 {
		t.Errorf("Free = %d, want 1 — an untiered session must never reduce free seats", seats.Free)
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/cmd/ -run TestPoolSeatPictureCountsUntiered -v`
Expected: FAIL to compile — `seats.Untiered undefined`.

- [ ] **Step 3: Implement**

Add to `dispatchSeats`, after `Uncapped`:

```go
	// Untiered counts live polecats on an agent no tier owns. They are
	// reported but never capped: they spend real money and host load, so
	// omitting them understates concurrency, while letting them reduce Free
	// would refuse every tiered sling (dispatchDecision gates on Free < 1).
	Untiered int `json:"untiered,omitempty"`
```

Replace the capacity block in `poolSeatPicture`:

```go
	tiers := pool.EffectiveTiers()
	if len(tiers) == 0 {
		return dispatchSeats{}
	}
	perTier, untiered, _ := poolSeatCountsByTier(tiers, sessions)

	capacity, occupied, uncapped := 0, 0, false
	for i, t := range tiers {
		synthesized := len(pool.Tiers) == 0 && i > 0
		if !poolTierCapped(t, synthesized) {
			uncapped = true
			occupied += perTier[i]
			continue
		}
		capacity += t.Max
		occupied += perTier[i]
	}

	seats := dispatchSeats{
		Source:   "polecat_pool",
		Capacity: capacity,
		Occupied: occupied,
		Untiered: untiered,
		Uncapped: uncapped,
	}
	seats.Free = capacity - occupied
	if seats.Free < 0 {
		seats.Free = 0
	}
```

- [ ] **Step 4: Run the test and confirm it passes**

Run: `go test ./internal/cmd/ -run TestPoolSeatPicture -v`
Expected: PASS, including any pre-existing `poolSeatPicture` tests.

- [ ] **Step 5: Commit**

```bash
git add internal/cmd/daemon_dispatch.go internal/cmd/daemon_dispatch_test.go
git commit -m "fix(dispatch): count untiered polecats without letting them cap dispatch (gt-xmsqb)"
bd comments add gt-xmsqb "commit: $(git rev-parse --short HEAD) — dispatchSeats.Untiered; capacity is the sum of tier maxima and untiered never reduces Free"
```

---

### Task 6: Fail open to the last shape-routed tier, and escalate when it repeats

The pool deliberately fails open when sessions cannot be listed — refusing there would stop every sling on a tmux hiccup. This task preserves that and makes the lapse loud.

**Files:**
- Modify: `internal/cmd/sling_pool.go` — `poolRoute` (line ~906)
- Test: `internal/cmd/sling_pool_test.go`

**Interfaces:**
- Consumes: `shapeRoutedTiers` from Task 2.
- Produces: `func poolUncountedFallback(pool *config.PolecatPool) string` keeps its name and signature; its body now returns the last shape-routed tier's agent.

- [ ] **Step 1: Write the failing test**

```go
func TestPoolUncountedFallbackUsesLastShapeRoutedTier(t *testing.T) {
	pool := &config.PolecatPool{
		Tiers: []config.PoolTier{
			{Agent: "local-coder-polecat", Max: 1},
			{Agent: "deepseek-flash", Max: 1},
			{Agent: "claude-sonnet", Max: 1, RequestOnly: true},
		},
	}
	got := poolUncountedFallback(pool)
	if got != "deepseek-flash" {
		t.Errorf("poolUncountedFallback = %q, want deepseek-flash — a fallback must never land on a request_only tier", got)
	}

	noTiers := &config.PolecatPool{}
	if got := poolUncountedFallback(noTiers); got != "the role default" {
		t.Errorf("poolUncountedFallback with no tiers = %q, want %q", got, "the role default")
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/cmd/ -run TestPoolUncountedFallbackUsesLastShapeRoutedTier -v`
Expected: FAIL — the current body returns `pool.OverflowAgent`, which is empty for a tiered pool, so it returns "the role default".

- [ ] **Step 3: Implement**

```go
// poolUncountedFallback names the agent a decision falls back to when it cannot
// count something it decides from: the last shape-routed tier, or the role
// default when the pool has none, which the caller resolves. A request_only
// tier is never a fallback — falling back onto the most expensive seat is the
// opposite of what a cap is for. The reason line names whichever it was, so a
// fallback never reads as a deliberate route.
func poolUncountedFallback(pool *config.PolecatPool) string {
	tiers := pool.EffectiveTiers()
	routed := shapeRoutedTiers(tiers)
	if len(routed) > 0 {
		return tiers[routed[len(routed)-1]].Agent
	}
	return "the role default"
}
```

- [ ] **Step 4: Run the test and confirm it passes**

Run: `go test ./internal/cmd/ -run TestPoolUncountedFallback -v`
Expected: PASS.

- [ ] **Step 5: Add the repeated-fallback counter**

In `poolRoute`, at the existing `cannot list sessions` branch, record the event before returning. Reuse the daemon's alert path rather than inventing one:

```go
		noteUncountedPoolFallback(townRoot)
		return poolUncountedFallback(ts.PolecatPool),
			"pool: cannot list sessions (" + err.Error() + "), using " + poolUncountedFallback(ts.PolecatPool), nil
```

Add to `internal/cmd/pool_tiers.go`:

```go
// uncountedFallbackThreshold is how many consecutive session-count failures
// pass before the lapse is escalated. The pool fails open here on purpose —
// refusing would stop every sling on a tmux hiccup — but a cap that lapses
// silently is the failure mode this whole change exists to remove, so the
// third consecutive lapse is reported.
const uncountedFallbackThreshold = 3

// noteUncountedPoolFallback records one session-count failure and escalates on
// the Nth consecutive one. A successful count clears the counter via
// clearUncountedPoolFallback.
func noteUncountedPoolFallback(townRoot string) {
	n := bumpUncountedFallbackCount(townRoot)
	if n < uncountedFallbackThreshold {
		return
	}
	escalatePoolFallback(townRoot, n)
}
```

Implement `bumpUncountedFallbackCount` as a small counter in
`<townRoot>/.runtime/pool-fallback.json` using the same `atomicfile` helper
`internal/daemon/deacon_self_probe.go` uses for its baseline, and
`escalatePoolFallback` as a `gt escalate -s HIGH --fingerprint pool-uncounted-fallback` exec, matching the argument shape in `internal/daemon/jsonl_git_backup.go:885`. Clear the counter on any successful `listPolecatSessions` in `poolRoute`.

- [ ] **Step 6: Run the full suite**

Run: `GOFLAGS=-p=8 make test`
Expected: PASS.
Run: `make lint`
Expected: clean.
Run: `make build`
Expected: builds.

- [ ] **Step 7: Commit and push**

```bash
git add internal/cmd/sling_pool.go internal/cmd/pool_tiers.go internal/cmd/sling_pool_test.go
git commit -m "feat(pool): fail open to the last shape-routed tier and escalate repeated lapses (gt-xmsqb)"
bd comments add gt-xmsqb "commit: $(git rev-parse --short HEAD) — uncounted-session fallback never lands on a request_only tier; third consecutive lapse escalates"
git push origin <your-branch>
```

---

## Rollout

The config change is the operator's, not this plan's. Once merged, the target
config in the design doc can be written to `~/gt/settings/config.json`:

```json
"polecat_pool": {
  "tiers": [
    { "agent": "local-coder-polecat", "max": 1 },
    { "agent": "deepseek-flash",      "max": 1 },
    { "agent": "claude-sonnet",       "max": 1, "request_only": true }
  ],
  "idle_fill": true,
  "min_spawn_gap": "4m"
}
```

**Do not commit that file.** It carries API keys. Edit it in place and leave it
untracked.

Until this lands, the interim cap stands: `max_overflow: 2 → 1` plus the mayor
holding itself to one live `claude-sonnet` polecat.

## Self-Review

**Spec coverage.** Every design section maps to a task: the `PoolTier` type and
`EffectiveTiers` (Task 1), `pool_tiers.go` with counting and lookup (Task 2),
the routing rewire with legacy parity (Task 3), `request_only` (Task 4),
`Untiered` reporting (Task 5), fail-open plus escalation (Task 6). The design's
`max: 0` asymmetry is covered by `TestPoolTierCapped` in Task 2. `idle_fill` and
`min_spawn_gap` staying pool-level require no code change, which is why no task
touches them — they continue to read the first tier via `newestFirstTier`.

**Placeholder scan.** No TBDs. The one step that describes rather than shows is
Task 6 Step 5's counter file, which names the exact helper (`atomicfile`), the
exact model to copy (`deacon_self_probe.go`'s baseline), and the exact
escalation argument shape (`jsonl_git_backup.go:885`) rather than leaving it to
taste.

**Type consistency.** `poolTierIndex`, `poolTierCapped`, `poolSeatCountsByTier`
and `shapeRoutedTiers` are defined in Task 2 and used under those exact names in
Tasks 3-6. `dispatchSeats.Untiered` is defined in Task 5 and referenced nowhere
earlier. `choosePoolAgent` keeps its original signature throughout.
