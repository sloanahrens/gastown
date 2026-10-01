# Heartbeats

Gas Town has **two heartbeat stores**. Only the session store feeds a
supervision decision.

## The two stores

### 1. Session heartbeat (per-session state store)

- **Written by:** `gt heartbeat [--state=working|idle|exiting|stuck]` →
  `polecat.TouchSessionHeartbeatWithState()`. Requires `GT_SESSION`.
- **Read by:** the daemon's `patrol_scan` tick, which reads the self-reported
  state instead of inferring liveness from timers (ZFC: gt-3vr5). A polecat
  whose heartbeat says `stuck` or a fresh `exiting` is never restarted
  ([ADR 0005](../adr/0005-patrol-scan-tick.md)). This is the store polecats
  refresh.

### 2. Agent-bead label — `heartbeat:<EPOCH>` on the agent bead

- **Written by:** `gt mol await-signal` and `gt mol await-event` on each wake
  (`updateAgentHeartbeatVia` in `internal/cmd/molecule_await_signal.go`), and
  by every `gt agents state` write. A label rewrite is used because
  `bd agent heartbeat` was never shipped (steveyegge/beads#2828).
- **Read by:** nothing that acts on it. The patrol agents that aged this label
  out were deleted with the witness and deacon roles.

## Rules of thumb

- **Polecats:** `gt heartbeat` (session store) is the one that matters.
- **Monitoring scripts:** never declare an agent stuck from a heartbeat alone.
  Liveness is measured by change, not by elapsed time
  ([ADR 0003](../adr/0003-one-supervisor-no-idle-llm.md)); cross-check tmux
  session activity (`tmux display-message -p '#{window_activity}'`) before
  escalating — a live session with a stale store is *heartbeat-write
  divergence*, not a stuck agent (hq-qxl9).
