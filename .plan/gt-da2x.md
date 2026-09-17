# Fix: `gt dog done` leaves formula wisps hooked (gt-da2x)

## Context

When a dog runs `gt dog done`, the dog's state resets to idle but any formula wisp (molecule-type ephemeral bead with `attached_formula` metadata) attached to its hook bead remains `hooked`. This causes two problems:

1. **Idle dog invisibility**: The dog is invisible to Handler dispatch (skipped because it holds a hooked formula), but no plugin can run on it either — the wisp is stranded with no session to advance it.
2. **Wisp accumulation**: Over time, these stranded wisps pile up, wasting Dolt storage and polluting the beads database.

## Fix 1: `gt dog done` closes formula wisps

**File**: `internal/cmd/dog.go` — add `closeDogFormulaWisps` function, call it from `runDogDone`.

### New function: `closeDogFormulaWisps(name string, townRoot, beadsDir string) error`

Mirrors `dogHasHookedFormula`'s pattern — shell out to `bd query` directly (not `beads.Beads.List`) to avoid the daemon's `BD_DOLT_AUTO_COMMIT=off` env. The query finds all hooked wisps assigned to the dog:

```
ephemeral=true AND status="hooked" AND assignee="deacon/dogs/<name>"
```

For each returned bead, verify it has `attached_formula` (is a molecule wisp), then close it using the same logic as `closeFormulaWisp` in `sling_formula.go`:

1. `forceCloseDescendants(b, wispID)` — recursively close step wisps
2. `bd.ForceCloseWithReason("dog done", wispID)` — close the wisp root

Return a count of closed wisps for logging. If no wisps found, return nil (no-op).

### Call site in `runDogDone` (line ~681, after `closePluginMails`)

```go
if wispsClosed, err := closeDogFormulaWisps(name, townRoot, beadsDir); err != nil {
    return fmt.Errorf("closing formula wisps for dog %s: %w", name, err)
} else if wispsClosed > 0 {
    fmt.Printf("  Closed %d formula wisp(s) attached to hook\n", wispsClosed)
}
```

Needs `townRoot` and `beadsDir` — resolve them early in `runDogDone` using `workspace.FindFromCwd()` and `findLocalBeadsDir()` (same pattern as `runMoleculeBurn` and `runMoleculeSquash`).

## Fix 2: Handler stale wisp detection

**File**: `internal/daemon/handler.go` — modify `findDispatchableDog` to close stale wisps before skipping the dog.

### Approach

Currently `findDispatchableDog` calls `dogHasHookedFormulaFn` which returns `bool`. Replace with a new seam `dogCheckHookedFormulaFn` that returns both the hook status AND (if present) the wisp root ID. When the dog is idle with a hooked formula:

1. Query the wisp's steps via `bd show <wispID>` to count total vs closed steps
2. Check dog's `last_active` vs wisp's `created_at` (wisp must be older than dog's idle transition)
3. If zero step progress AND wisp is older → close with reason "abandoned: idle dog, no progress"
4. Use a "logged once" per-dog guard to avoid repeated WARN spam on every dispatch tick

### Implementation details

Add a new struct for the check result:

```go
type hookedFormulaResult struct {
    hasHooked bool
    wispID    string  // non-empty only when hasHooked is true
}
```

New function `dogCheckHookedFormula(townRoot, beadsDir, dogName string) (hookedFormulaResult, error)` — same `bd query` as `dogHasHookedFormula` but returns the full issue objects (not just `anyHasAttachedFormula` bool).

In `findDispatchableDog`, after detecting `hooked == true`:

```go
// Check if this is a stale wisp (no step progress, dog went idle after wisp created)
if result := dogCheckHookedFormulaFn(townRoot, beadsDir, d.Name); result.hasHooked {
    // Query wisp steps
    steps, _ := b.Children(result.wispID)
    allClosed := true
    for _, s := range steps {
        if s.Status != "closed" {
            allClosed = false
            break
        }
    }
    
    // Dog became idle after wisp was created AND no steps made progress
    if allClosed && d.LastActive.After(wispCreatedAt) {
        closeStaleWisp(result.wispID, beadsDir)
        // Log once with a per-dog tracked set
        if !staleLogged[d.Name] {
            logger.Printf("Handler: closed stale wisp for idle dog %s (abandoned, no progress)", d.Name)
            staleLogged[d.Name] = true
        }
    }
    continue
}
```

The `staleLogged` map is local to the dispatch cycle, cleared each time `DispatchPlugins` runs.

## Tests

### Test 1: `TestDogDone_ClosesFormulaWisps` (dog_test.go)

- Setup: dog state with work, create a fake hooked wisp bead in the beads DB
- Call: `closeDogFormulaWisps(name, townRoot, beadsDir)`
- Verify: wisp root is now closed, all descendants are closed, no hooked wisps remain

### Test 2: `TestFindDispatchableDog_ClosesStaleWisp` (handler_test.go)

- Use the `dogHasHookedFormulaFn` seam pattern — override to return a hooked wisp ID
- Setup: idle dog with `last_active` after wisp's `created_at`
- Setup: wisp with all steps closed (zero progress)
- Call: `findDispatchableDog`
- Verify: wisp is closed, dog is returned as dispatchable (stale wisp was removed)

### Test 3: `TestDogDone_NoWispsToClose` (dog_test.go)

- Edge case: dog has no hooked wisps → `closeDogFormulaWisps` returns 0, no error

## Quality Gates

- `go test ./internal/cmd/... -run TestDogDone`
- `go test ./internal/daemon/... -run "TestFindDispatchableDog|TestDogCheck"`
- `go vet ./internal/cmd/... ./internal/daemon/...`
