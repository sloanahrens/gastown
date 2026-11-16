> Status: historical (2026-11). Merged in: gt-2l1v6y (the census is P1.1's input to the agent-bead retirement design). Not maintained.

# Who reads and writes `notification_level`, `role_type`, `active_mr` and `last_source_issue`

Input to the agent-bead retirement design (`docs/plans/2026-11-15-agent-bead-retirement-design.md`, "Field map"), task 1.1. That design still had to establish, field by field, which readers use these four fields, and whether anything needs them. This document answers that from the code.

## Method

Every site below is a line the design's command returned on `origin/main` (`0f1df68e`, 2026-11-16):

```text
git grep -n -E 'notification_level|NotificationLevel|role_type|RoleType|active_mr|ActiveMR|last_source_issue|LastSourceIssue' origin/main -- internal cmd ':!*_test.go'
```

That returned 314 lines in 39 files. This document lists 322 rows, because a line that names two or more of the fields appears under each. No working tree was read and no site was inferred.

Each row has one verdict, from four:

| Verdict | Meaning |
|---|---|
| **feeds a decision** | A branch reads the value to decide something: liveness, restart, capacity, reuse, nuke, cleanup, delivery or protection. |
| **display only** | The value is printed, or copied into a report or JSON document for a human. No branch reads it. |
| **derivable** | The value this site needs can be rebuilt from the seat name or from the work or MR bead, so the intent record need not carry it for this site. |
| **storage** | The field's declaration, wire format, update plumbing, reset, fixture or lint baseline. It carries or writes the value rather than consuming it. |

The plan names three verdicts — display, decision, derivable — which cover readers. The Goal also asks for writers, and a writer has no reader verdict; it is marked `storage`, together with the declaration, codec, fixture and lint-baseline rows. Four mail rows name a different `RoleType` and are marked `unrelated`.

`R/W` is `R` for a reader, `W` for a writer, `R/W` for the codec that both encodes and parses the description, and `—` for a declaration, comment or fixture. Comments are given the verdict of the code they explain: a comment stating a decision rule is `feeds a decision`, and help text is `display only`.

### Answers to the design's open questions

- **What does the reaper need `active_mr` for?** It protects MR wisps. `liveAgentReferencedWispIDs` (`internal/reaper/reaper.go`) reads every live agent bead's description and keeps any wisp named as `active_mr` or `hook_bead` out of every reap and purge phase. The protection is what keeps a live MR's wisp from being aged out while the seat still points at it (gt-gyb6, gt-4okk, gt-u0zq0). It reads the raw description in SQL, not through the field helpers, so no helper-level change reaches it.
- **Does anything need `notification_level`?** Yes. Mail and nudge delivery suppress a muted recipient, and `gt up` resets a muted seat to normal on startup. It is an operator preference; nothing derives it from the seat name or the work bead.
- **Does anything need `role_type`?** No reader needs it from the record. The one query that filters by it (mail's by-role group resolution) needs the role, and the seat ID already carries it. The writers stamp it and go with the bead.
- **Is there a field the design does not name?** None in these hits. The only other key in the same lines is `rig`, which the design's field map already covers. The `RoleType` in `internal/mail` is an address group type (`crew`, `polecat`), not the bead field, and is marked unrelated below.

### What the grep cannot see

- It is line-level. A reader that calls `beads.ParseAgentFields` and then reads a field is caught at the field constant, not at the parse. Two whole-description readers, `agentDescriptionKeys` (`internal/cmd/pool_seats.go`) and `descriptionField` (`internal/mail/router.go`), read any key by name and are not in a table; they are storage-shape checks that disappear with the bead.
- Four `RoleType` rows in `internal/mail/router.go` are `ParsedGroup.RoleType`, which the router sets from the address (`constants.RoleCrew`, `constants.RolePolecat`). They are not the agent-bead field and are listed as unrelated.

## `notification_level`

| Site | Symbol | R/W | Verdict |
|---|---|---|---|
| `internal/beads/beads_agent.go:47` | `AgentFields.NotificationLevel` declaration | — | storage |
| `internal/beads/beads_agent.go:114` | `FormatAgentDescription` writes only a set level | R/W | storage |
| `internal/beads/beads_agent.go:115` | `FormatAgentDescription` `notification_level:` line | R/W | storage |
| `internal/beads/beads_agent.go:117` | `FormatAgentDescription` `notification_level: null` line | R/W | storage |
| `internal/beads/beads_agent.go:190` | `ParseAgentFields` case | R/W | storage |
| `internal/beads/beads_agent.go:191` | `ParseAgentFields` assigns the level | R/W | storage |
| `internal/beads/beads_agent.go:309` | `AgentFieldUpdates.NotificationLevel` pointer | — | storage |
| `internal/beads/beads_agent.go:332` | `UpdateAgentDescriptionFields` validates the level | W | storage |
| `internal/beads/beads_agent.go:333` | `UpdateAgentDescriptionFields` reads the update | W | storage |
| `internal/beads/beads_agent.go:360` | `UpdateAgentDescriptionFields` applies the level | W | storage |
| `internal/beads/beads_agent.go:361` | `UpdateAgentDescriptionFields` assigns the level | W | storage |
| `internal/beads/client_helpers.go:250` | comment on `UpdateAgentNotificationLevel` | — | storage |
| `internal/beads/client_helpers.go:252` | `UpdateAgentNotificationLevel` | W | storage |
| `internal/beads/client_helpers.go:253` | `UpdateAgentNotificationLevel` body | W | storage |
| `internal/beads/client_helpers.go:256` | comment on `GetAgentNotificationLevel` | R | storage |
| `internal/beads/client_helpers.go:258` | `GetAgentNotificationLevel` | R | storage |
| `internal/beads/client_helpers.go:263` | `GetAgentNotificationLevel` defaults to normal when unset | R | storage |
| `internal/beads/client_helpers.go:266` | `GetAgentNotificationLevel` returns the level | R | storage |
| `internal/cmd/polecat_identity.go:705` | identity rename carries the level to the new bead | W | storage |
| `internal/cmd/pool_seats.go:658` | `rosterAgentFields` requires the line | R | storage |
| `internal/cmd/pool_seats.go:691` | comment: the required lines prove the record reaches this key | — | storage |
| `internal/cmd/status.go:595` | `renderAgentDetails` prints the muted marker | R | display only |
| `internal/cmd/status.go:694` | `renderAgentCompact` prints the muted marker | R | display only |
| `internal/cmd/status.go:752` | `detectCurrentDNDStatus` builds the DND status section | R | display only |
| `internal/cmd/testdata/pool_seat_list.json:5` | recorded `bd list` fixture | — | storage |
| `internal/cmd/testdata/pool_seat_show.json:5` | recorded `bd show` fixture | — | storage |
| `internal/cmd/up.go:424` | `disableCurrentAgentDND` reads the level before unmuting | R | feeds a decision |
| `internal/cmd/up.go:433` | `disableCurrentAgentDND` writes normal | W | storage |
| `internal/mail/address_policy.go:43` | `AgentBeadIDForAddress` comment: this bead gates delivery | — | feeds a decision |
| `internal/mail/router.go:71` | `townBeads.GetAgentNotificationLevel` | R | feeds a decision |
| `internal/mail/router.go:84` | `channelTown.GetAgentNotificationLevel` | R | feeds a decision |
| `internal/mail/router.go:85` | `channelTown` delegates to the beads helper | R | feeds a decision |
| `internal/mail/router.go:2037` | `isSessionMuted` suppresses a nudge | R | feeds a decision |
| `internal/mail/router.go:2218` | `isRecipientMuted` suppresses a tmux nudge | R | feeds a decision |
| `internal/nudge/deliver/town.go:29` | comment on the `NotificationLevel` hook | — | feeds a decision |
| `internal/nudge/deliver/town.go:31` | `NotificationLevel` injection point | R | feeds a decision |
| `internal/nudge/deliver/town.go:67` | injected hook is used when set | R | feeds a decision |
| `internal/nudge/deliver/town.go:68` | injected hook call | R | feeds a decision |
| `internal/nudge/deliver/town.go:70` | falls back to `beads.GetAgentNotificationLevel` | R | feeds a decision |
| `internal/townstatus/agents.go:203` | copies the level into the agent model | R | display only |
| `internal/townstatus/model.go:128` | `AgentRuntime.NotificationLevel` JSON field | — | display only |

**What the intent record must carry.** A `notification_level` (or an equivalent `muted` flag). Three delivery paths read it to decide whether to deliver (`isSessionMuted`, `isRecipientMuted`, the nudge `NotificationLevel` hook), and `gt up` reads it to decide whether to unmute a restarted seat. None of those values is derivable: the seat name carries the role and rig, and the work bead carries work, but a mute is an operator preference written to the seat's own record. Today both delivery reads fail open on a lookup error (deliver anyway); the intent reader should keep that direction explicit rather than inherit it by accident, because the design's rule is that an unreadable intent record reads as held. The display-only copies (status, townstatus) follow the same value and need no separate home.

## `role_type`

| Site | Symbol | R/W | Verdict |
|---|---|---|---|
| `internal/beads/beads_agent.go:41` | `AgentFields.RoleType` declaration | — | storage |
| `internal/beads/beads_agent.go:84` | `FormatAgentDescription` `role_type:` line | R/W | storage |
| `internal/beads/beads_agent.go:178` | `ParseAgentFields` case | R/W | storage |
| `internal/beads/beads_agent.go:179` | `ParseAgentFields` assigns the role | R/W | storage |
| `internal/beads/beadsfake/contract.go:1562` | `contractAgentActiveMR` description fixture | — | storage |
| `internal/beads/beadsfake/contract.go:1568` | `contractAgentActiveMR` asserts the role survives an update | R | storage |
| `internal/beads/beadsfake/contract.go:1590` | `contractAgentActiveMR` asserts the role survives a clear | R | storage |
| `internal/beads/client_helpers.go:549` | comment: reset preserves `role_type` | — | storage |
| `internal/cmd/agents_resolve.go:263` | `agentBeadMatches` compares the parsed role, then falls back to `ParseAgentBeadID` | R | derivable |
| `internal/cmd/crew_add.go:25` | `gt crew add` writes `role_type: crew` | W | storage |
| `internal/cmd/polecat_identity.go:238` | identity create writes `role_type: polecat` | W | storage |
| `internal/cmd/polecat_identity.go:699` | identity rename writes `role_type: polecat` | W | storage |
| `internal/cmd/pool_seats.go:652` | `rosterAgentFields` requires the line | R | storage |
| `internal/cmd/pool_seats.go:682` | `listedAgentBeadComplete` rejects a record with no role | R | storage |
| `internal/cmd/testdata/pool_seat_list.json:5` | recorded `bd list` fixture | — | storage |
| `internal/cmd/testdata/pool_seat_show.json:5` | recorded `bd show` fixture | — | storage |
| `internal/doctor/agent_beads_check.go:334` | doctor recreates a missing crew bead with a role | W | storage |
| `internal/doctor/agent_beads_check.go:351` | doctor recreates a missing polecat bead with a role | W | storage |
| `internal/done/done.go:2221` | `ensureAgentBeadExists` writes `role_type: polecat` | W | storage |
| `internal/mail/router.go:335` | `ParsedGroup.RoleType` — an address group type | — | unrelated |
| `internal/mail/router.go:378` | `ParsedGroup{RoleType: crew}` for a rig group | — | unrelated |
| `internal/mail/router.go:380` | `ParsedGroup{RoleType: polecat}` for a rig group | — | unrelated |
| `internal/mail/router.go:487` | `parseRigAgentAddress` comment | R | derivable |
| `internal/mail/router.go:491` | `parseRigAgentAddress` finds `role_type:` | R | derivable |
| `internal/mail/router.go:492` | `parseRigAgentAddress` parses the role | R | derivable |
| `internal/mail/router.go:562` | `parseAgentAddressFromDescription` comment | R | derivable |
| `internal/mail/router.go:570` | `parseAgentAddressFromDescription` finds `role_type:` | R | derivable |
| `internal/mail/router.go:571` | `parseAgentAddressFromDescription` parses the role | R | derivable |
| `internal/mail/router.go:578` | comment: `role_type` + `rig` may be too little to address | — | derivable |
| `internal/mail/router.go:596` | comment: rig-level address is `rig/name` | — | derivable |
| `internal/mail/router.go:626` | `resolveByGroup` reads `group.RoleType` (the group type) | R | unrelated |
| `internal/mail/router.go:669` | `resolveAgentsByRole` comment | R | feeds a decision |
| `internal/mail/router.go:673` | `resolveAgentsByRole` queries the description for the role | R | feeds a decision |
| `internal/polecat/manager.go:1304` | manager creates an agent bead with a role | W | storage |
| `internal/polecat/manager.go:1546` | manager creates an agent bead with a role | W | storage |
| `internal/polecat/manager.go:2413` | manager creates an agent bead with a role | W | storage |
| `internal/polecat/manager.go:2648` | manager creates an agent bead with a role | W | storage |

**What the intent record must carry.** Nothing. Every reader needs the role only to address the seat or to match it, and the seat name already states it: the ID is `<prefix>-<rig>-<role>-<name>`, `ParseAgentBeadID` parses the role and rig, and `agentBeadMatches` already falls back to the ID when the field is missing. The one by-role reader, mail's `resolveAgentsByRole`, needs the set of seats of a role; it should list the intent records (or the seat names) rather than query a description. The writers that stamp the field are part of the bead and stop with it.

## `active_mr`

| Site | Symbol | R/W | Verdict |
|---|---|---|---|
| `internal/beads/agent_reconcile.go:62` | comment: merge clears dangling `active_mr` | — | feeds a decision |
| `internal/beads/agent_reconcile.go:128` | `MergeLegacyAgentBead` picks a non-dangling MR pointer | R/W | feeds a decision |
| `internal/beads/beads_agent.go:46` | `AgentFields.ActiveMR` declaration | — | storage |
| `internal/beads/beads_agent.go:108` | `FormatAgentDescription` writes a set MR | R/W | storage |
| `internal/beads/beads_agent.go:109` | `FormatAgentDescription` `active_mr:` line | R/W | storage |
| `internal/beads/beads_agent.go:111` | `FormatAgentDescription` `active_mr: null` line | R/W | storage |
| `internal/beads/beads_agent.go:188` | `ParseAgentFields` case | R/W | storage |
| `internal/beads/beads_agent.go:189` | `ParseAgentFields` assigns the MR | R/W | storage |
| `internal/beads/beads_agent.go:308` | `AgentFieldUpdates.ActiveMR` pointer | — | storage |
| `internal/beads/beads_agent.go:357` | `UpdateAgentDescriptionFields` reads the update | W | storage |
| `internal/beads/beads_agent.go:358` | `UpdateAgentDescriptionFields` applies the MR | W | storage |
| `internal/beads/beads_agent.go:403` | comment on `ClearAgentActiveMRIfMatches` | — | storage |
| `internal/beads/beads_agent.go:405` | `(*Beads).ClearAgentActiveMRIfMatches` | W | storage |
| `internal/beads/beads_agent.go:407` | routes to a canonical-client method | W | storage |
| `internal/beads/beads_agent.go:421` | falls through to the free function | W | storage |
| `internal/beads/beadsfake/contract.go:189` | contract-test table entry | — | storage |
| `internal/beads/beadsfake/contract.go:1558` | `contractAgentActiveMR` comment | — | storage |
| `internal/beads/beadsfake/contract.go:1559` | `contractAgentActiveMR` | — | storage |
| `internal/beads/beadsfake/contract.go:1562` | description fixture with an MR | — | storage |
| `internal/beads/beadsfake/contract.go:1568` | asserts the parsed MR and role | R | storage |
| `internal/beads/beadsfake/contract.go:1579` | clears with a non-matching MR | W | storage |
| `internal/beads/beadsfake/contract.go:1583` | asserts the non-matching clear changed nothing | R | storage |
| `internal/beads/beadsfake/contract.go:1584` | error message for a changed MR | R | storage |
| `internal/beads/beadsfake/contract.go:1586` | clears with a padded matching MR | W | storage |
| `internal/beads/beadsfake/contract.go:1590` | asserts the MR cleared and the role stayed | R | storage |
| `internal/beads/beadsfake/contract.go:1591` | error message after the clear | R | storage |
| `internal/beads/beadsfake/contract.go:1593` | clear of a missing bead | W | storage |
| `internal/beads/beadsfake/contract.go:1596` | clear of a non-agent bead refuses | W | storage |
| `internal/beads/client_helpers.go:102` | comment on `ClearAgentActiveMRIfMatches` | — | storage |
| `internal/beads/client_helpers.go:106` | `ClearAgentActiveMRIfMatches` | W | storage |
| `internal/beads/client_helpers.go:108` | routes to a canonical-client method | W | storage |
| `internal/beads/client_helpers.go:110` | trims both ids before the fallback | W | storage |
| `internal/beads/client_helpers.go:113` | comment on the read-compare-write | — | storage |
| `internal/beads/client_helpers.go:115` | `clearAgentActiveMRIfMatches` | W | storage |
| `internal/beads/client_helpers.go:130` | compares the current MR with the expected one | R/W | storage |
| `internal/beads/client_helpers.go:133` | clears the field | W | storage |
| `internal/beads/client_helpers.go:244` | comment on `UpdateAgentActiveMR` | — | storage |
| `internal/beads/client_helpers.go:246` | `UpdateAgentActiveMR` | W | storage |
| `internal/beads/client_helpers.go:247` | writes the MR through the update helper | W | storage |
| `internal/beads/client_helpers.go:552` | `ResetAgentBeadForReuse` clears the MR | W | storage |
| `internal/cmd/dashboard_polecats.go:564` | dashboard passes the MR reader into the inventory | R | feeds a decision |
| `internal/cmd/polecat.go:195` | `gt polecat list` help: an open MR blocks | — | display only |
| `internal/cmd/polecat.go:241` | `check-recovery` help: reads `active_mr` | — | display only |
| `internal/cmd/polecat.go:244` | `check-recovery` help: SAFE_TO_NUKE needs a terminal MR | — | display only |
| `internal/cmd/polecat.go:246` | `check-recovery` help: NEEDS_RECOVERY cases | — | display only |
| `internal/cmd/polecat.go:462` | comment: `MRID` is the agent bead's `active_mr` | — | feeds a decision |
| `internal/cmd/polecat.go:469` | recovery status struct `active_mr` JSON field | — | display only |
| `internal/cmd/polecat.go:585` | recovery verdict details mention the MR | R | display only |
| `internal/cmd/polecat.go:586` | appends `active_mr=` to the details | R | display only |
| `internal/cmd/polecat.go:596` | `AssessActiveMR` in the batch check | R | feeds a decision |
| `internal/cmd/polecat.go:601` | workstate facts take the MR | R | feeds a decision |
| `internal/cmd/polecat.go:603` | sets the MR blocker | R | feeds a decision |
| `internal/cmd/polecat.go:704` | comment: an empty MR index reads unknown | — | feeds a decision |
| `internal/cmd/polecat.go:743` | `hookIDs` gathers `active_mr` for the title lookup | R | display only |
| `internal/cmd/polecat.go:757` | passes the MR reader into the inventory | R | feeds a decision |
| `internal/cmd/polecat.go:1512` | inventory item `active_mr` JSON field | — | display only |
| `internal/cmd/polecat.go:1671` | `recoveryBatchIdle` refuses idle with a live MR | R | feeds a decision |
| `internal/cmd/polecat.go:1799` | `recoveryTargetRefs` takes the MR | R | feeds a decision |
| `internal/cmd/polecat.go:1808` | comment: the no-bead branch is all failure flags | — | feeds a decision |
| `internal/cmd/polecat.go:1839` | comment: the read branch takes the bead's fields | — | feeds a decision |
| `internal/cmd/polecat.go:1846` | recovery status takes the parsed MR | R | feeds a decision |
| `internal/cmd/polecat.go:1847` | workstate facts take the parsed MR | R | feeds a decision |
| `internal/cmd/polecat.go:1851` | `recoveryTargetRefs` with the source hint | R | feeds a decision |
| `internal/cmd/polecat.go:1865` | assessment variable | R | feeds a decision |
| `internal/cmd/polecat.go:1866` | assesses only a set MR | R | feeds a decision |
| `internal/cmd/polecat.go:1873` | comment on the dangling-MR fix | — | feeds a decision |
| `internal/cmd/polecat.go:1876` | comment: landed evidence for a gone MR | — | feeds a decision |
| `internal/cmd/polecat.go:1877` | `AssessActiveMRWithLandedEvidence` | R | feeds a decision |
| `internal/cmd/polecat.go:1878` | passes the MR into the assessment | R | feeds a decision |
| `internal/cmd/polecat.go:1892` | marks the source terminal | R | feeds a decision |
| `internal/cmd/polecat.go:1895` | sets the MR blocker | R | feeds a decision |
| `internal/cmd/polecat.go:1995` | prints the active MR line | R | display only |
| `internal/cmd/polecat.go:1996` | `Active MR:` output line | R | display only |
| `internal/cmd/polecat.go:2529` | `activeMRBlocker` classifies the MR | R | feeds a decision |
| `internal/cmd/polecat.go:2530` | passes the MR into the assessment | R | feeds a decision |
| `internal/cmd/polecat.go:3250` | `nuke` refuses while an MR is pending | R | feeds a decision |
| `internal/cmd/polecat.go:3622` | `activeMRRemovalChecker` interface | R | feeds a decision |
| `internal/cmd/polecat.go:3625` | `checkNukeActiveMRSafety` | R | feeds a decision |
| `internal/cmd/polecat.go:3629` | refuses a nuke on a pending MR | R | feeds a decision |
| `internal/cmd/polecat_helpers.go:97` | recovery result `ActiveMR` field | R | feeds a decision |
| `internal/cmd/polecat_helpers.go:165` | assessment variable | R | feeds a decision |
| `internal/cmd/polecat_helpers.go:166` | assesses only a set MR | R | feeds a decision |
| `internal/cmd/polecat_helpers.go:174` | `AssessActiveMR` with git safety | R | feeds a decision |
| `internal/cmd/polecat_helpers.go:212` | marks the source terminal | R | feeds a decision |
| `internal/cmd/polecat_helpers.go:216` | sets the MR blocker | R | feeds a decision |
| `internal/cmd/polecat_helpers.go:217` | fills a blocker when the policy gives none | R | feeds a decision |
| `internal/cmd/polecat_helpers.go:218` | `active_mr=%s pending` blocker | R | feeds a decision |
| `internal/cmd/polecat_helpers.go:244` | copies the MR into the result | R | feeds a decision |
| `internal/cmd/polecat_helpers.go:245` | result takes the MR | R | feeds a decision |
| `internal/cmd/polecat_helpers.go:366` | checks a set MR | R | feeds a decision |
| `internal/cmd/polecat_helpers.go:372` | `activeMRBlocker` decides whether the MR reads terminal | R | feeds a decision |
| `internal/cmd/polecat_helpers.go:375` | prints a terminal MR | R | display only |
| `internal/cmd/polecat_identity.go:704` | identity rename carries the MR | W | storage |
| `internal/cmd/polecat_identity_reconcile.go:37` | reconcile help: an unreadable target | — | display only |
| `internal/cmd/polecat_inventory.go:50` | `polecatSeat.ActiveMR` | R | feeds a decision |
| `internal/cmd/polecat_inventory.go:115` | comment: the policy's two lookups | — | feeds a decision |
| `internal/cmd/polecat_inventory.go:129` | `polecatActiveMRReader` comment | R | feeds a decision |
| `internal/cmd/polecat_inventory.go:138` | `polecatActiveMRReader` type | R | feeds a decision |
| `internal/cmd/polecat_inventory.go:143` | `Show` resolves the policy's lookup | R | feeds a decision |
| `internal/cmd/polecat_inventory.go:157` | comment: a lookup error is not `ErrNotFound` | — | feeds a decision |
| `internal/cmd/polecat_inventory.go:162` | `landedProbe` supplies git evidence | — | feeds a decision |
| `internal/cmd/polecat_inventory.go:289` | comment: the reuse gate uses the same policy | — | feeds a decision |
| `internal/cmd/polecat_inventory.go:398` | `statusFor(fields.ActiveMR)` | R | feeds a decision |
| `internal/cmd/polecat_inventory.go:412` | item takes the parsed MR | R | feeds a decision |
| `internal/cmd/polecat_inventory.go:422` | facts take the MR | R | feeds a decision |
| `internal/cmd/polecat_inventory.go:520` | checks a set MR | R | feeds a decision |
| `internal/cmd/polecat_inventory.go:521` | comment: the list uses the shared policy | — | feeds a decision |
| `internal/cmd/polecat_inventory.go:535` | `AssessActiveMRWithLandedEvidence` | R | feeds a decision |
| `internal/cmd/polecat_inventory.go:536` | passes the MR into the assessment | R | feeds a decision |
| `internal/cmd/polecat_inventory.go:540` | sets the MR blocker | R | feeds a decision |
| `internal/cmd/polecat_inventory.go:588` | comment: the policy resolves MR ids | — | feeds a decision |
| `internal/cmd/polecat_inventory.go:681` | report item carries the MR | R | display only |
| `internal/cmd/polecat_inventory.go:789` | comment: a pointer whose bead is gone | — | feeds a decision |
| `internal/cmd/polecat_inventory.go:815` | comment: the MR index | — | feeds a decision |
| `internal/cmd/polecat_inventory.go:883` | `statusFor` reads the bead's MR | R | feeds a decision |
| `internal/cmd/pool_seats.go:657` | `rosterAgentFields` requires the line | R | storage |
| `internal/cmd/reaper.go:796` | `reaper purge` help: live references are kept | — | feeds a decision |
| `internal/cmd/sling_pool.go:899` | pool seat reads the MR | R | feeds a decision |
| `internal/cmd/sling_pool.go:900` | facts take the MR | R | feeds a decision |
| `internal/cmd/sling_pool.go:901` | sets the MR blocker | R | feeds a decision |
| `internal/cmd/testdata/pool_seat_list.json:5` | recorded `bd list` fixture | — | storage |
| `internal/cmd/testdata/pool_seat_show.json:5` | recorded `bd show` fixture | — | storage |
| `internal/daemon/patrol_scan_reap.go:236` | `recoveryStatusJSON.ActiveMR` decoded field | R | display only |
| `internal/daemon/patrol_scan_reap.go:264` | copies the decoded MR into `Recovery` | R | display only |
| `internal/doctor/agent_beads_shadow_check.go:15` | comment: two slots leaked to a ghost MR | — | feeds a decision |
| `internal/doctor/agent_beads_shadow_check.go:105` | shadow check compares bead and intent MR | R | feeds a decision |
| `internal/done/done.go:489` | comment: one write includes the MR | — | storage |
| `internal/done/done.go:2206` | comment: recreate so MR writes do not fail | — | storage |
| `internal/guardlint/baseline.txt:12` | lint baseline note for the clear helper | — | storage |
| `internal/guardlint/baseline.txt:13` | lint baseline entry for the clear helper | — | storage |
| `internal/patrolscan/reap.go:68` | `Recovery.ActiveMR` field | — | display only |
| `internal/polecat/active_mr.go:11` | `IssueReader` comment | — | feeds a decision |
| `internal/polecat/active_mr.go:16` | `ActiveMRInput` comment | — | feeds a decision |
| `internal/polecat/active_mr.go:17` | `ActiveMRInput` type | — | feeds a decision |
| `internal/polecat/active_mr.go:18` | `ActiveMRInput.ActiveMR` | — | feeds a decision |
| `internal/polecat/active_mr.go:25` | comment: the dangling-MR gate | — | feeds a decision |
| `internal/polecat/active_mr.go:32` | `ActiveMRAssessment` comment | — | feeds a decision |
| `internal/polecat/active_mr.go:36` | `ActiveMRAssessment` type | — | feeds a decision |
| `internal/polecat/active_mr.go:37` | `ActiveMRAssessment.ActiveMR` | — | feeds a decision |
| `internal/polecat/active_mr.go:51` | comment on `AssessActiveMR` | — | feeds a decision |
| `internal/polecat/active_mr.go:55` | `AssessActiveMR` | R | feeds a decision |
| `internal/polecat/active_mr.go:56` | trims the input MR | R | feeds a decision |
| `internal/polecat/active_mr.go:58` | empty input is not pending | R | feeds a decision |
| `internal/polecat/active_mr.go:60` | a set MR starts pending | R | feeds a decision |
| `internal/polecat/active_mr.go:62` | unverified status reason | R | feeds a decision |
| `internal/polecat/active_mr.go:69` | a missing MR goes to the stale path | R | feeds a decision |
| `internal/polecat/active_mr.go:71` | lookup error keeps it pending | R | feeds a decision |
| `internal/polecat/active_mr.go:75` | an empty lookup goes to the stale path | R | feeds a decision |
| `internal/polecat/active_mr.go:80` | status reason for a found MR | R | feeds a decision |
| `internal/polecat/active_mr.go:83` | classifies a found MR's status | R | feeds a decision |
| `internal/polecat/active_mr.go:86` | comment on `AssessActiveMRWithLandedEvidence` | — | feeds a decision |
| `internal/polecat/active_mr.go:94` | comment: no probe leaves the result unchanged | — | feeds a decision |
| `internal/polecat/active_mr.go:99` | `AssessActiveMRWithLandedEvidence` | R | feeds a decision |
| `internal/polecat/active_mr.go:100` | runs the base assessment | R | feeds a decision |
| `internal/polecat/active_mr.go:110` | returns the base assessment when unmeasured | R | feeds a decision |
| `internal/polecat/active_mr.go:113` | `assessStaleActiveMR` | R | feeds a decision |
| `internal/polecat/active_mr.go:118` | `sourceIssueForActiveMR` | R | feeds a decision |
| `internal/polecat/active_mr.go:134` | stale-MR reason | R | feeds a decision |
| `internal/polecat/active_mr.go:138` | unsafe-git reason | R | feeds a decision |
| `internal/polecat/active_mr.go:146` | `sourceIssueForActiveMR` prefers the hint | R | feeds a decision |
| `internal/polecat/landed.go:17` | comment: only for a gone or closed MR | — | feeds a decision |
| `internal/polecat/landed.go:18` | comment: see the landed-evidence assessment | — | feeds a decision |
| `internal/polecat/landed.go:21` | comment: probe of the carried work | — | feeds a decision |
| `internal/polecat/landed.go:23` | comment: the dangling-MR gate | — | feeds a decision |
| `internal/polecat/landed.go:73` | comment: the gate's landed evidence | — | feeds a decision |
| `internal/polecat/manager.go:1723` | `nuke` consults `ActiveMRRemovalBlocker` | R | feeds a decision |
| `internal/polecat/manager.go:1960` | comment on `ActiveMRRemovalBlocker` | — | feeds a decision |
| `internal/polecat/manager.go:1963` | `ActiveMRRemovalBlocker` | R | feeds a decision |
| `internal/polecat/manager.go:1972` | trims the bead's MR | R | feeds a decision |
| `internal/polecat/manager.go:1980` | no MR means no blocker | R | feeds a decision |
| `internal/polecat/manager.go:1987` | `AssessActiveMR` for the removal gate | R | feeds a decision |
| `internal/polecat/manager.go:1994` | pending-MR blocker | R | feeds a decision |
| `internal/polecat/manager.go:2643` | comment: reset clears the MR | — | storage |
| `internal/polecat/manager.go:3448` | comment: the MR is a verified fact | — | feeds a decision |
| `internal/polecat/manager.go:3462` | reuse facts take the MR | R | feeds a decision |
| `internal/polecat/manager.go:3463` | local `activeMR` takes it | R | feeds a decision |
| `internal/polecat/manager.go:3503` | comment: the snapshot feeds `AssessActiveMR` | — | feeds a decision |
| `internal/polecat/manager.go:3508` | comment: the probe runs only for a gone MR | — | feeds a decision |
| `internal/polecat/manager.go:3513` | `AssessActiveMRWithLandedEvidence` in the reuse gate | R | feeds a decision |
| `internal/polecat/manager.go:3521` | sets the MR blocker | R | feeds a decision |
| `internal/polecat/manager.go:3527` | marks the source terminal | R | feeds a decision |
| `internal/polecat/manager.go:3669` | `reuseTargetRefs` reads a set MR | R | feeds a decision |
| `internal/polecat/manager.go:3670` | looks up the MR bead for its target | R | feeds a decision |
| `internal/polecat/reclaim.go:54` | `reclaim` blocks on a set MR | R | feeds a decision |
| `internal/polecat/reclaim.go:55` | `active_mr=` blocker string | R | feeds a decision |
| `internal/polecat/reuse.go:36` | `WorkstateInput.ActiveMR` | — | feeds a decision |
| `internal/polecat/reuse.go:37` | `WorkstateInput.ActiveMRBlocker` | — | feeds a decision |
| `internal/polecat/reuse.go:38` | `WorkstateInput.ActiveMRSourceTerminal` | — | feeds a decision |
| `internal/polecat/reuse.go:81` | copies the MR into the facts | R | feeds a decision |
| `internal/polecat/reuse.go:82` | copies the blocker | R | feeds a decision |
| `internal/polecat/reuse.go:83` | copies the terminal marker | R | feeds a decision |
| `internal/polecat/workstate.go:87` | `WorkstateFacts.ActiveMR` | — | feeds a decision |
| `internal/polecat/workstate.go:88` | `WorkstateFacts.ActiveMRBlocker` | — | feeds a decision |
| `internal/polecat/workstate.go:192` | comment: a done seat's MR facts | — | feeds a decision |
| `internal/polecat/workstate.go:203` | comment: verified MR facts | — | feeds a decision |
| `internal/polecat/workstate.go:219` | a done seat with an MR blocker is not reusable | R | feeds a decision |
| `internal/polecat/workstate.go:224` | returns the MR blocker | R | feeds a decision |
| `internal/polecat/workstate.go:383` | an MR blocker is set | R | feeds a decision |
| `internal/polecat/workstate.go:385` | `active-mr-open` block | R | feeds a decision |
| `internal/polecat/workstate.go:615` | comment: the facts come from one read | — | feeds a decision |
| `internal/polecat/workstate.go:675` | comment: a successful read sets the facts | — | feeds a decision |
| `internal/polecat/workstate.go:692` | `reuseFacts.ActiveMR` | — | feeds a decision |
| `internal/polecat/workstate.go:693` | `reuseFacts.ActiveMRBlocker` | — | feeds a decision |
| `internal/polecat/workstate.go:694` | `reuseFacts.ActiveMRSourceTerminal` | — | feeds a decision |
| `internal/polecat/workstate.go:735` | `WorkTerminal` counts a terminal MR source | R | feeds a decision |
| `internal/polecat/workstate.go:744` | comment on `ActiveMRSafe` | — | feeds a decision |
| `internal/polecat/workstate.go:745` | `ActiveMRSafe` | R | feeds a decision |
| `internal/polecat/workstate.go:746` | safe means no blocker | R | feeds a decision |
| `internal/polecat/workstate.go:757` | stale cleanup status needs a safe MR | R | feeds a decision |
| `internal/polecat/workstate.go:767` | reuse reads `ActiveMRSafe` | R | feeds a decision |
| `internal/polecat/workstate.go:791` | copies the MR into the reuse facts | R | feeds a decision |
| `internal/polecat/workstate.go:792` | copies the blocker | R | feeds a decision |
| `internal/reaper/reaper.go:398` | `activeMRFieldPattern` matches the description | R | feeds a decision |
| `internal/reaper/reaper.go:415` | comment: the pointers the reaper protects | — | feeds a decision |
| `internal/reaper/reaper.go:437` | `collectAgentReferences` adds the MR's wisp | R | feeds a decision |
| `internal/reaper/reaper.go:581` | comment: the reference exclusion | — | feeds a decision |
| `internal/reaper/reaper.go:718` | comment: every phase carries the exclusion | — | feeds a decision |
| `internal/reaper/reaper.go:784` | comment: closed-molecule steps carry it too | — | feeds a decision |

**What the intent record must carry.** An MR pointer. This is the field's one irreplaceable consumer: the reaper protects any wisp a live agent bead names as `active_mr` or `hook_bead` from every reap and purge phase, so a live MR's wisp is not swept while the seat still points at it (gt-gyb6, gt-4okk, gt-u0zq0). `hook_bead` already maps to the intent record's `work_bead`; `active_mr` has no equivalent, and if the field is dropped the protection disarms and a live MR becomes age-eligible. The pointer is not derivable from the seat name, and the MR bead does not name the seat today, so the intent record must carry it (for example `mr_bead`, alongside `work_bead`). Every other reader here — the reuse gate, the nuke refusal, `gt polecat list` and `check-recovery` dispositions, the reclaim blocker — reads the same value through `polecat.AssessActiveMR`, so one carried field serves them all. The two `display only` daemon rows, `Recovery.ActiveMR` and `recoveryStatusJSON.ActiveMR`, are decoded but never read; they need no replacement.

## `last_source_issue`

| Site | Symbol | R/W | Verdict |
|---|---|---|---|
| `internal/beads/agent_reconcile.go:132` | `MergeLegacyAgentBead` picks the source issue | R/W | feeds a decision |
| `internal/beads/beads_agent.go:59` | `AgentFields.LastSourceIssue` declaration | — | storage |
| `internal/beads/beads_agent.go:140` | `FormatAgentDescription` writes a set source | R/W | storage |
| `internal/beads/beads_agent.go:141` | `FormatAgentDescription` `last_source_issue:` line | R/W | storage |
| `internal/beads/beads_agent.go:203` | `ParseAgentFields` case | R/W | storage |
| `internal/beads/beads_agent.go:204` | `ParseAgentFields` assigns the source | R/W | storage |
| `internal/beads/beads_agent.go:316` | `AgentFieldUpdates.LastSourceIssue` pointer | — | storage |
| `internal/beads/beads_agent.go:379` | `UpdateAgentDescriptionFields` reads the update | W | storage |
| `internal/beads/beads_agent.go:380` | `UpdateAgentDescriptionFields` applies the source | W | storage |
| `internal/beads/client_helpers.go:287` | `CompletionUpdates` records the hook bead as the source | W | storage |
| `internal/beads/client_helpers.go:303` | `ClearAgentCompletion` clears the source | W | storage |
| `internal/beads/client_helpers.go:559` | `ResetAgentBeadForReuse` clears the source | W | storage |
| `internal/cmd/polecat.go:743` | `hookIDs` gathers the source for the title lookup | R | display only |
| `internal/cmd/polecat.go:2476` | `agentSourceIssueHint` falls back to the source | R | feeds a decision |
| `internal/cmd/polecat.go:2477` | returns the source as the hint | R | feeds a decision |
| `internal/cmd/pool_seats.go:661` | `rosterAgentFields` key | R | storage |
| `internal/daemon/patrol_scan.go:673` | `AgentRecord` takes the source | R | feeds a decision |
| `internal/daemon/patrol_scan_reap.go:375` | `hookBeadRef` falls back to the source for the grace clock | R | feeds a decision |
| `internal/doctor/agent_beads_shadow_check.go:109` | shadow check compares bead and intent source | R | feeds a decision |
| `internal/patrolscan/scan.go:189` | comment on `AgentRecord.LastSourceIssue` | — | feeds a decision |
| `internal/patrolscan/scan.go:191` | `AgentRecord.LastSourceIssue` field | — | feeds a decision |
| `internal/patrolscan/scan.go:1476` | kept-seat attention rule reads the source | R | feeds a decision |
| `internal/polecat/manager.go:1983` | `ActiveMRRemovalBlocker` source hint | R | feeds a decision |
| `internal/polecat/manager.go:3466` | reuse-gate source hint | R | feeds a decision |
| `internal/polecat/manager.go:3687` | `reuseTargetRefs` reads a distinct source | R | feeds a decision |
| `internal/polecat/manager.go:3688` | looks up the source bead's targets | R | feeds a decision |

**What the intent record must carry.** The work bead of the last turn, as the design's `last_work_bead`. It feeds four decisions: the kept-seat attention rule (`finishedExitEvidence` uses it to date a closed escalation), the reap grace clock's close time (`hookBeadRef`), the reuse gate's target-ref lookup (`reuseTargetRefs`), and the source-issue hint the active-MR landed probe uses. It cannot be derived after the turn ends: `gt done` writes it as the outgoing hook bead at the same moment it clears the hook, and the intent record clears its own `work_bead` when the submission ends, so without a new field the value is gone. It is only partly recoverable from the work bead itself, through its assignee, and that fails once the bead is deleted or the seat has run several. The two `display only` rows use it only to look up a title.

## What this changes in the design

- `active_mr` no longer stays in the field map's "open" column: it must be carried, and the reaper is the reader that decides it.
- `notification_level` moves from "none: display or derivable" to carried: it gates delivery, and no seat name or work bead states a mute.
- `role_type` is confirmed as needing no home: the seat ID already carries it. The one by-role query in mail must read the intent records instead of a description.
- No fifth field surfaced. The remaining description keys in these lines (`rig`) are already in the field map.
