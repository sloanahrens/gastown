> Status: historical (2026-10). Research for Mo4/Mo5 in gt-p85pw. Not maintained; the port it sizes replaces it.

# R1: can gt cook formulas without `bd cook`, and how big is the cook/bond/wisp port

**Bead:** gt-p85pw.1 (epic gt-p85pw, track R, research). **Completed by:** polecat organic, 2026-10-25.
**Read at:** gastown `main` 2088024f; beads `main` 7570bb92 (the `bd` on this host reports 1.2.2, dev 39a04940).
**Method:** read-only. No file changed outside this report; no fetch in a mayor/rig. The beads tree was read through a bare partial clone of the beads remote, so every beads line number below is `origin/main`'s.

## Answer

**gt does not cook formulas. It shells out to `bd cook` and renders what comes back.** `internal/formula` is 490 non-test lines that embed the 21 formula files, sync them to the town, and hold the town overlay's file format; it contains no formula TOML decode, no `extends`, no expansion and no condition evaluation. The package README states it directly: "Nothing in gastown parses a formula" (`internal/formula/README.md:5`).

The engine a port would need is beads' `internal/formula` package (5,783 non-test lines, 12,473 with its tests; the cook path gt actually uses is about 2.5k of them) plus ~380 lines of assembly in `cmd/bd`. Copying that package with its tests is 20 beads (one bead = 20 minutes) for parity with the formulas gt ships, and ~37 beads for parity with `bd cook` as a whole.

`bd cook` never opens a database: bd exempts it from store initialization unless `--persist` (`cmd/bd/main.go:1125-1130`), and a cook from an empty directory with no beads database and no config returns a full tree given a `--search-path` (verified on this host). So keeping `bd cook` costs nothing on a rig whose Dolt is already gone; the port's deadline is set by retiring the `bd` binary, not by the Dolt cutover.

## 1. Where gt calls the engine today

Every call runs through one seam: `beads.Beads.Cook` runs `bd cook <formula> --var …` (`internal/beads/formula.go:19`), and every bd call in gt carries `BD_MACHINE=1` (`internal/beads/database.go:247-250`), so bd answers with the machine-mode step tree (`cmd/bd/cook_tree.go:196 runCookTree`).

| Call site | What it does with the tree |
|---|---|
| `internal/cmd/prime.go:352` (via `prime_checklist.go:47`) | the checklist `gt prime` prints for the hooked molecule; reads steps only (`formula_cook.go:70 checklist`) |
| `internal/cmd/formula.go:252` | `gt formula show` renders the cooked tree |
| `internal/cmd/formula.go:354` | `gt formula run` cooks with `--set` vars before slinging |
| `internal/cmd/sling_helpers.go:855` | sling cooks once to read declared vars and defaults, then bonds |
| `internal/cmd/sling_helpers.go:1010` (`CookFormula`, from `sling_run.go:257,270`, `sling_batch.go:85`) | pre-cooks a formula so a bad formula fails a batch before any bead changes |
| `internal/sling/run.go:333` | same pre-cook for a single sling |
| `internal/doctor/overlay_health_check.go:51` | cooks without the overlay to validate overlay step ids |

The renderer's view of the tree is `cookedFormula` / `cookedVar` / `cookedStep` (`internal/cmd/formula_cook.go:20,35,47`) and the engine interface is `formulaEngine` (`internal/cmd/formula_cook.go:91`) — four methods: `Cook`, `Bond`, `FormulaShow`, `Wisp`. That interface is the natural port seam: a Go engine implements it and every call site above is unchanged.

gt also points bd at the town's overlay dir (`BD_FORMULA_OVERLAY_DIR`, `internal/cmd/formula_cook.go:180`), so the overlay is applied by bd, not gt.

## 2. What `internal/formula` in gastown actually is

| File | Lines | What it contains |
|---|---|---|
| `internal/formula/doc.go` | 16 | the D6 statement that bd is the one engine |
| `internal/formula/embed.go` | 375 | `//go:embed formulas/*.formula.toml` (:17), sync by content hash (`ProvisionFormulas` :122, `PlanFormulaSync` :226, `SyncFormulas` :235) |
| `internal/formula/overlay.go` | 99 | the overlay **file format** (`FormulaOverlay` :31), its one directory (`OverlayDir` :58) and loader (`LoadFormulaOverlay` :69) |

That is the whole package: no parser, no step tree, no var substitution. `gt` reads formulas only as bytes it embeds and copies.

## 3. Feature by feature

The features named in the acceptance, in the order the cook pipeline applies them (`cmd/bd/cook.go:772-894 cookPipeline`). "gt implements" means gt computes it from a formula file; in every row the answer is no, and the gastown column names where gt consumes the cooked result.

| Feature | gt implements it? | beads implementation | gt consumes at | Shipped formulas using it |
|---|---|---|---|---|
| **needs** | No | `Step.Needs`/`DependsOn` (`internal/formula/types.go:195`), merged into one list of blockers at `cmd/bd/cook_tree.go:152-186` (`cookTreeSteps`) | `cookedStep.Needs` (`internal/cmd/formula_cook.go:52`) | 15 of 21 |
| **vars with defaults** | No | `VarDef.Default` (`types.go:127`), default resolution `parser.go:597 ApplyDefaults`, exposed per var at `cook_tree.go:121-143` | `cookedVar.Default` (`formula_cook.go:35`), backfilled into bond vars (`sling_helpers.go:971`) | 9 of 21 declare defaults; 15 of 21 declare a required var |
| **extends** | No | `Parser.Resolve` (`internal/formula/parser.go:227`, 86 lines) with `mergeSteps` (:364) and `mergeComposeRules` (:390) | visible only as the already-merged step list | 5 of 21 (`mol-deep-review`, `mol-doc-audit`, `mol-polecat-work-monorepo-tdd`, `shiny-enterprise`, `shiny-secure`) |
| **compose.expand** | No | `ApplyExpansions` (`internal/formula/expand.go:34`, 134 lines), `expandStep` (:196), `MaterializeExpansion` (:420), inline expansions (:459) | visible only in the resulting steps | 4 of 21 (the three above plus `mol-polecat-work-monorepo-tdd`) |
| **condition** | No | `FilterStepsByCondition` (`internal/formula/stepcondition.go:118`) and `EvaluateStepCondition` (:40) over the expression language in `internal/formula/condition.go` (`ParseCondition` :123, `evaluateAggregate` :279, `compare` :443); applied with formula defaults merged in at `cmd/bd/cook.go:847-866` | nowhere: gt never sees a filtered step as filtered, only absent | 0 of 21 (the `condition` hit in `mol-polecat-code-review.formula.toml` is the word in prose) |

Four more features ride the same pipeline and matter for a port:

- **Town overlay.** gt owns the file format but not the application: bd loads and applies it (`internal/formula/overlay.go:38 LoadOverlay`, `:88 ApplyOverlay`, `:125 replaceEdge`), after every expansion so it targets final step ids (`cmd/bd/cook.go:839-845`). A port must keep that ordering or overlays silently miss expanded steps.
- **Aspects and advice.** `advice.go` (292 lines) and the aspect branch of `cookPipeline` (`cook.go:823-835`); `shiny-secure.formula.toml` uses `compose.aspects`, `security-audit.formula.toml` is two `[[advice]]` rules with no steps. The design spec puts aspects out of scope, so a narrow port cannot cook `shiny-secure`.
- **Strict decode.** Machine-mode cook parses with `parser.Strict = true` (`cmd/bd/cook_tree.go:82`), backed by `strict.go` (251) and the key-position reader `keyline.go` (260). Without it a port silently drops unknown keys instead of failing the way `bd cook` fails.
- **Loops, branches, gates.** `controlflow.go` (620 lines) and `range.go` (381). No shipped formula uses them; the design spec puts them out of scope.

Step counts per formula as cooked (all 21, with only `--var issue` supplied): `beads-release` 17, `gastown-release` 13, `mol-polecat-work` 8, `mol-deep-review` 8 (17 declared vars, 16 with defaults), `mol-orphan-scan` 8 (18 vars), `mol-session-gc` 6 (12 vars, none required), the rest 1-14. This is the fixture set Mo4's unit tests should pin.

## 4. What `bd mol bond` does with formula vars, and what `backfillFormulaDefaultVars` compensates for

**bd mol bond's rule.** The bond operand is cooked inline to a subgraph (`cmd/bd/mol_bond.go:613 resolveOrCookToSubgraph` → `cmd/bd/cook.go:739 resolveAndCookFormulaWithVars`), which attaches each declared var's `VarDef` to the subgraph (`cmd/bd/cook.go:931-950`). Then `buildAttachCloneOpts` (`cmd/bd/mol_bond.go:416-459`) builds the required list from `extractAllVariables` (`cmd/bd/template.go:344`), which scans the cooked title, description, design, acceptance criteria and notes of every issue for `{{placeholder}}` — **it never looks at `VarDef.Default`**. One missing key aborts with bd's own words: `missing required variables: a, b (use --var)`.

**Neither does the clone apply defaults.** `cloneSubgraphInto` (`cmd/bd/template.go:668`) substitutes only the vars in `CloneOptions.Vars` (`substituteVariables`, `template.go:407`); an unprovided placeholder is written into the spawned bead literally. Unlike bond, `mol pour` (`cmd/bd/pour.go:174`) and `mol wisp` (`cmd/bd/wisp.go:264`) call `applyVariableDefaults` (`template.go:386`) and then check only `extractRequiredVariables` (`template.go:356`), which does filter by `VarDef`. Bond has no such call.

So a bare `bd mol bond mol-polecat-work <bead> --ephemeral` fails with `missing required variables: base_branch, build_command, issue, lint_command, setup_command, test_command, typecheck_command` — `issue` plus the six vars that formula declares with defaults, because bond's own cook applies none of them (a plain `bd cook` of the same formula with no `--var` leaves only `issue` unresolved).

**What gt compensates for.** `varsForBead` (`internal/cmd/sling_helpers.go:849`) cooks the formula once with the sling's vars, reads each declared var's `Default` from the returned tree (`sling_helpers.go:855`), and `backfillFormulaDefaultVars` (`sling_helpers.go:971`) appends `name=default` for every declared default, `name=` for an optional var with no default, and fails with a message naming the formula when a required var has no default and is still unresolved (bd's own error does not say which formula wanted it). The bond then receives every value explicitly, and `cloneSubgraphInto` substitutes them. gt therefore cooks twice per sling: once to learn the defaults, once inside the bond.

Two consequences for the port:

1. **The var rule must be reproduced, or the backfill disappears too.** Once `POST /molecules` (Mo1) replaces bond, gt is the caller that holds the tree, so the correct behaviour is wisp's: apply the formula's defaults, then require only vars without defaults (`extractRequiredVariables` semantics). The backfill exists only because bd's bond reads text instead of `VarDef`; it should not survive the move.
2. **bd's bond keyword list is not the tree's.** `extractAllVariables` skips Handlebars keywords `else, this, root, index, key, first, last` (`cmd/bd/template.go:334`), while the cook tree's `unresolved_vars` does not — `mol-session-gc` cooks with `else` in `unresolved_vars`. A port that derives bond requirements from `unresolved_vars` alone would demand a var named `else`.

## 5. Port size

Line counts are non-test lines in beads `origin/main`. Bead sizes assume copy-and-adapt at roughly 250-350 lines per 20-minute bead including the ported tests; treat them as ±30%.

### Cook (the read path Mo4 needs)

| Piece | Source | Lines | Tests (lines) | Beads |
|---|---|---|---|---|
| formula structs and validation | `internal/formula/types.go` | 875 | 0 (covered via parser) | 2 |
| parser, `extends` merge, var validation, `ApplyDefaults` | `internal/formula/parser.go` | 640 | `parser_test.go` 2,209 | 5 |
| inline + compose expansions | `internal/formula/expand.go` | 520 | `expand_test.go` 1,199 | 4 |
| overlay application | `internal/formula/overlay.go:38,88,125` | 117 (gt already has 99 lines of format) | `overlay_test.go` 95 | 1 |
| pipeline and tree assembly: `cookPipeline` 122, `cookTreeFor` 75, `cookTreeSteps` 44, `stepTypeToIssueType` 73, substitution 27, `loadFormulaByNameOrPath` 22, overlay dir 18 | `cmd/bd/cook.go:31,772,912,1220,235`, `cmd/bd/cook_tree.go:77,152` | 363 | `cook_tree_test.go` 199 + goldens | 3 |
| parity harness over the 21 shipped formulas + bd's `testdata/cook` fixtures | new | — | `cook_test.go` 1,039 to mine | 3 |
| seam wiring (`formulaEngine` swap; call sites unchanged) | `internal/cmd/formula_cook.go:91-178` | — | — | 2 |

**Subtotal: 2,515 production + ~3,700 test lines ≈ 20 beads (≈ 6.5 hours).**

Full `bd cook` parity adds: conditions 731 lines / 889 test lines (3-5 beads), strict decode 511 / 382 (3), loops and gates 620 / 980 (4), advice/aspects 292 / 375 (3), loop ranges 381 / 271 (2). **≈ 37 beads total (≈ 12 hours).** `schema_gen.go` (834 lines of generated JSON-schema code) and `lint.go` (134) are not needed by cook.

### Bond (needed only if the service does not replace the write)

| Function | Source | Lines |
|---|---|---|
| `buildAttachCloneOpts` (the var rule above) | `cmd/bd/mol_bond.go:416` | 44 |
| `resolveOrCookToSubgraph` / `resolveOrDescribe` | `mol_bond.go:613,577` | 76 |
| `bondProtoMolWithSubgraph` | `mol_bond.go:391` | 25 |
| `cloneSubgraphInto` / `cloneSubgraph` | `cmd/bd/template.go:668,648` | 130 |
| var extraction and substitution: `extractAllVariables` 12, `extractRequiredVariables` 30, `applyVariableDefaults` 21, `substituteVariables` 33, `substituteMetadataRepo` 44, `generateBondedID` 41, `getRelativeID` 23, keyword list 8 | `template.go:314-560` | 212 |
| `wouldCreateCycle` | `mol_bond.go:469` | 43 |
| `flattenUnregisteredIssueTypes` | `template.go:583` | 65 |
| loading a persisted proto (`loadTemplateSubgraph`, `loadDescendants`) — only for bonding to a proto bead, not a formula | `template.go:83,133` | 145 |

≈ 595 lines for formula→molecule bonding, ≈ 740 with persisted protos; tests ≈ 770 (`template_test.go` 531, bond tests 242). **4-6 beads** for the client side once Mo1's `POST /molecules` exists; without Mo1, add the store write path (that is Mo1's own work, not this port).

### Wisp (create only; list/gc/purge are Mo3)

`runWispCreateCore` 112 (`cmd/bd/wisp.go:177`), `checkRequiredVars` 13 (:289), `parseVarFlags` 12 (`pour.go:74`), then the shared clone. ≈ 150 lines of glue over the cook tree and the create API. **2 beads** with `wisp_test.go` 81 and `wisp_integration_test.go` 98.

### Total

| Deliverable | Beads | Wall clock (one worker) |
|---|---|---|
| Cook, 21-formula parity (Mo4 as scoped) | 20 | ≈ 6.5 h |
| Cook, full `bd cook` parity | 37 | ≈ 12 h |
| Bond client side (with Mo1) | 4-6 | ≈ 1.5-2 h |
| Wisp create client side | 2 | ≈ 40 min |

The largest single risk is not line count: `internal/formula/parser.go` imports three beads internals for search paths (`internal/beads`, `internal/git`, `internal/utils`), so the parser's `DefaultSearchPaths` and workspace discovery must be re-rooted on gt's `formulaSite` (dir, beads dir, town root). Everything else in the package is stdlib plus `BurntSushi/toml`, which gt already requires (`internal/formula/overlay.go:8`).

## 6. Recommendation

**Keep calling `bd cook` through the migration period, and port it last — as Mo4's narrow set, not as a rewrite.** `bd cook` is store-free (verified: it cooks from a directory with no beads database and no config), so a rig flipped to the tracker keeps its checklist, `formula show`, and sling pre-cook working with `bd.real` while bond and pour move to the service; nothing about the Dolt cutover forces the port. Porting early would create a second engine while bond and wisp still run inside bd, so the tree gt renders and the molecules bd pours could drift with no test that compares them. When the port is due — when `bd.real` is retired — copy beads' `internal/formula` package and its tests rather than reimplementing: 20 beads gets the 21 shipped formulas (extends, compose.expand, vars, needs, overlay) at parity, 37 gets full `bd cook` parity, and the package is self-contained apart from its search-path imports. Whatever the schedule, the one piece that should move with Mo1 rather than wait is bond's variable rule: once `POST /molecules` replaces bond, gt should apply formula defaults itself (pour/wisp semantics) and delete `backfillFormulaDefaultVars`, because it exists only to compensate for a bond that reads `{{placeholders}}` out of text while ignoring the defaults the formula declares.
