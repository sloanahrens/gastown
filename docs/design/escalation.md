# Gas Town Escalation Protocol

> Reference for the unified escalation system in Gas Town.

## Overview

Gas Town agents escalate issues when automated resolution is not possible.
Escalations are severity-routed, tracked as beads, and support stale detection
with automatic re-escalation.

## Severity Levels

| Level | Priority | Description | Default Route |
|-------|----------|-------------|---------------|
| **CRITICAL** | P0 (urgent) | System-threatening, immediate attention | bead + email + SMS |
| **HIGH** | P1 (high) | Important blocker, needs human soon | bead + email |
| **MEDIUM** | P2 (normal) | Standard escalation, human at convenience | bead |
| **LOW** | P3 (information) | Informational, can wait | bead |

## Tiered Escalation Flow

```
Agent -> gt escalate -s <SEVERITY> "description"
           |
           v
     [Escalation bead filed]
           |
           +-- routed --> bead and any configured outside channels
           +-- reviewed --> ack / close by whoever picks it up
```

Each tier can resolve OR forward. The chain is tracked via bead comments.

## Configuration

Config file: `~/gt/settings/escalation.json`

A repeat firing of an already-open escalation (same alert key) always bumps
its occurrence count, but only re-sends its routed notifications once
`renotify_window` has elapsed since the alert's last notification — otherwise
a condition that fires every few minutes would spam every channel on every
cycle, and a condition that persists for days would never say so twice.

### Default Configuration

```json
{
  "type": "escalation",
  "version": 1,
  "routes": {
    "low": ["bead"],
    "medium": ["bead"],
    "high": ["bead", "email:human"],
    "critical": ["bead", "email:human", "sms:human"]
  },
  "contacts": {
    "human_email": "",
    "human_sms": "",
    "slack_webhook": "",
    "smtp_host": "",
    "smtp_port": "587",
    "smtp_from": "",
    "smtp_user": "",
    "smtp_pass": "",
    "sms_webhook": ""
  },
  "stale_threshold": "4h",
  "max_reescalations": 2,
  "renotify_window": "1h",
  "unhandled_after": "30m"
}
```

### Action Types

| Action | Format | Behavior |
|--------|--------|----------|
| `bead` | `bead` | Create escalation bead (always first, implicit) |
| `mail:<target>` | `mail:gastown/witness` | Send gt mail to target |
| `email:human` | `email:human` | Send email to `contacts.human_email` |
| `sms:human` | `sms:human` | Send SMS to `contacts.human_sms` |
| `slack` | `slack` | Post to `contacts.slack_webhook` |
| `exec:<name>` | `exec:ntfy` | Run `notify.commands.<name>` with the message on stdin |
| `log` | `log` | Write to escalation log file |

### Push to a Phone: `exec:<name>`

`exec:<name>` pushes an escalation to the human through any command the
operator configures, so Gas Town carries no vendor code. Name the commands
under `notify.commands` in the same file as the routes; each is an argv array,
run without a shell:

```json
{
  "routes": {
    "high": ["bead", "exec:ntfy"],
    "critical": ["bead", "email:human", "exec:ntfy"]
  },
  "notify": {
    "commands": {
      "ntfy": ["curl", "-fsS", "-m", "8", "--data-binary", "@-",
               "https://ntfy.sh/REPLACE_WITH_PRIVATE_TOPIC"]
    }
  }
}
```

Config load rejects a route that names an `exec:` command that is not under
`notify.commands`, and the error names it. A town with no `notify` key behaves
as before.

How the command runs:

- The message is on **stdin**. The first line is a short title; the rest is the
  body. Nothing from the message goes in argv.
- The environment is `PATH` and `HOME` only. A command that needs a secret
  reads it from a file under `HOME`.
- It has 10 seconds. A command still running then is killed with its whole
  process group, `curl` and anything it spawned included.
- Stdout and stderr are discarded.
- A failure or timeout is logged and reported in the escalation's delivery
  result by the command's **name**, never its argv (a topic URL is a secret).
  It does not stop the route's other actions or the escalation bead.

Try a command with `gt notify test <name>`. It sends `gastown notify test`
and prints `NOTIFY OK` or `NOTIFY FAILED: <reason>`, exiting 1 on a failure.
Go code calls `notify.Push(name, title, body)` to push through the same
commands.

### Who a Push Goes To

A push is for what the human must act on, so the escalation's owner decides
whether it is sent at all (gt-71714). The owner is the human's when the human's
own session raised the escalation, and the overseer's when a polecat, a crew
session or the daemon raised it, because the overseer triages those first:
`exec:`, `email:`, `sms:` and `slack` run for the human's, while the bead, the
mail to agents and the log run either way.

An escalation the overseer owns reaches the human only if nobody acted on it for
`unhandled_after` (default `"30m"`). The daemon pushes one line then — `Unhandled
for 30 min: <id> <title>` — through the command `operational.overseer.silent_push`
names, and records the push as the `unhandled-pushed` label so a restart does not
say it twice. `gt ask` pushes a question the same way when it is filed at P0, P1
or P2, as `Question <id> P<n>: <title>`; a P3 or P4 waits on the dashboard. Both
push the title alone: no escalation body and no question body leave the town.

## Escalation Beads

Escalation beads use `type: escalation` with structured labels for tracking.

### Label Schema

| Label | Values | Purpose |
|-------|--------|---------|
| `severity:<level>` | MEDIUM, HIGH, CRITICAL | Current severity |
| `source:<type>:<name>` | plugin:rebuild-gt, daemon:patrol-scan | What triggered it |
| `acknowledged:<bool>` | true, false | Has human acknowledged |
| `reescalated:<bool>` | true, false | Has been re-escalated |
| `reescalation_count:<n>` | 0, 1, 2, ... | Times re-escalated |
| `original_severity:<level>` | MEDIUM, HIGH | Initial severity |

## Category Routing (future)

Categories provide structured routing based on the nature of the escalation.
Not yet implemented; routing is by severity only.

| Category | Description | Default Route |
|----------|-------------|---------------|
| `decision` | Multiple valid paths, need choice | Overseer |
| `help` | Need guidance or expertise | Overseer |
| `blocked` | Waiting on unresolvable dependency | Overseer |
| `failed` | Unexpected error, can't proceed | Overseer |
| `emergency` | Security or data integrity issue | Overseer (direct) |
| `gate_timeout` | Gate didn't resolve in time | Overseer |

## Commands

### gt escalate

Create a new escalation.

```bash
gt escalate -s <MEDIUM|HIGH|CRITICAL> "Short description" \
  [-r "Detailed explanation"] [--source="plugin:rebuild-gt"]
```

Flags: `-s` severity, `-r` reason (body), `--source` origin identifier,
`--fingerprint` stable alert key, `--related` related bead, `--dry-run`, `--json`.

For Dolt outages or GT behavior mismatches that involve Dolt-backed state, add
the RCA capture checklist from `docs/dolt-health-guide.md` to the escalation
body or the follow-up bead before restarting services.

### gt escalate ack

Acknowledge an escalation (prevents re-escalation).

```bash
gt escalate ack <bead-id> [--note="Investigating"]
```

### gt escalate list

```bash
gt escalate list [--severity=...] [--stale] [--unacked] [--all] [--json]
```

### gt escalate stale

Re-escalate stale (unacked past `stale_threshold`) escalations. Bumps severity
(MEDIUM->HIGH->CRITICAL), re-executes route, respects `max_reescalations`.

```bash
gt escalate stale [--dry-run]
```

### gt escalate close

```bash
gt escalate close <bead-id> [--reason="Fixed in commit abc123"]
```

## Integration Points

### Plugin System

Plugins use escalation for failure notification:

```bash
gt escalate -s MEDIUM "Plugin FAILED: rebuild-gt" \
  -r "$ERROR" --source="plugin:rebuild-gt"
```

### Stale Escalations

`gt escalate stale` re-escalates unacknowledged escalations past
`stale_threshold`. Nothing runs it on a schedule since the deacon patrol was
deleted (ADR 0005); run it by hand or from a plugin.

## When to Escalate

### Agents SHOULD escalate when:

- **System errors**: Database corruption, disk full, network failures
- **Security issues**: Unauthorized access attempts, credential exposure
- **Unresolvable conflicts**: Merge conflicts that cannot be auto-resolved
- **Ambiguous requirements**: Spec is unclear, multiple valid interpretations
- **Design decisions**: Architectural choices that need human judgment
- **Stuck loops**: Agent is stuck and cannot make progress
- **Gate timeouts**: Async conditions did not resolve in expected time

### Agents should NOT escalate for:

- **Normal workflow**: Regular work that can proceed without human input
- **Recoverable errors**: Transient failures that will auto-retry
- **Information queries**: Questions that can be answered from context

## Operator Review

Open escalations surface in the town's attention queue and in `gt escalate list`.
Action: review the open records, then ack or close them with
`gt escalate close <id> --reason "..."`.


## Viewing Escalations

```bash
# List all open escalations
bd list --status=open --tag=escalation

# Filter by category
bd list --tag=escalation --tag=decision

# View specific escalation
bd show <escalation-id>

# Close resolved escalation
bd close <id> --reason "Resolved by fixing X"
```
