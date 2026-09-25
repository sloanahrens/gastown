#!/usr/bin/env bash
# seat-refill/run.sh — nudge the mayor when a polecat seat sits empty with
# slingable work ready.
#
# Why this exists: the mayor is event-driven. Declining to dispatch opens no
# slot, so the SLOT_OPEN that would wake it never arrives, and a seat that
# empties while it sleeps stays empty — gt-59o9 measured 5.5 h idle with 362
# ready beads. The daemon's mayor_dispatch patrol covers that hole at a 30m
# cadence with one seat number for the whole pool; this plugin is the per-seat,
# three-minute half. It tracks each seat's own empty episode, so a seat that
# empties between patrols is named within one, with the beads that could fill
# it attached.
#
# It decides nothing about dispatch. It never slings, and it names a seat only
# when that seat is empty AND work exists that a sling could take. Which bead,
# on which model and priority, stays with the mayor (town directive, dispatch
# routing, 2026-09-19).
#
# The daemon runs this in-process (execution type script) and records the run
# itself, so this script must not call `gt plugin record-run` — that would
# write a second receipt for one run. Exit 0 prints the skip marker when the
# run found nothing to say.
set -euo pipefail

SKIP_MARKER='[plugin-result skipped]'

log() { printf '[seat-refill] %s\n' "$*"; }

# A failure here is not silence-worthy: the daemon records it and hands the
# output to a dog, which is the only moment an agent has something to decide.
fail() {
  printf '[seat-refill] FAIL: %s\n' "$*" >&2
  exit 1
}

# Nothing to say this run. The marker makes the receipt read "skipped" rather
# than "success", so a plugin that quietly stopped firing is visible in
# `gt plugin history` (gt-chqi).
skip() {
  log "$*"
  printf '%s\n' "$SKIP_MARKER"
  exit 0
}

int_or_default() {
  case "${1:-}" in
    ''|*[!0-9]*) printf '%s' "$2" ;;
    *) printf '%s' "$1" ;;
  esac
}

TOWN_ROOT="${GT_TOWN_ROOT:-${GT_ROOT:-}}"
[ -n "$TOWN_ROOT" ] || fail "GT_TOWN_ROOT is unset; cannot read the pool config or reach the mayor"
[ -d "$TOWN_ROOT" ] || fail "town root $TOWN_ROOT is not a directory"

# Every input below is overridable so run_test.sh can drive the episode logic
# without a town, a tmux server, or Dolt. The defaults are the real ones.
CONFIG_FILE="${GT_SEAT_REFILL_CONFIG:-$TOWN_ROOT/settings/config.json}"
STATE_FILE="${GT_SEAT_REFILL_STATE:-$TOWN_ROOT/.runtime/seat-refill.json}"
HOLD_FILE="${GT_SEAT_REFILL_HOLD:-$TOWN_ROOT/seat-refill.hold}"
MAYOR_TARGET="${GT_SEAT_REFILL_MAYOR:-mayor}"

NOW="${GT_SEAT_REFILL_NOW:-$(date +%s)}"
# How long a seat must sit empty before it is worth a nudge, and how often one
# empty episode may be nudged about. 5m is the bead's threshold; the 15m repeat
# keeps a long-empty seat from producing a nudge every heartbeat.
EMPTY_SECONDS=$(int_or_default "${GT_SEAT_REFILL_EMPTY_SECONDS:-}" 300)
NUDGE_SECONDS=$(int_or_default "${GT_SEAT_REFILL_NUDGE_SECONDS:-}" 900)
MAX_PRIORITY=$(int_or_default "${GT_SEAT_REFILL_MAX_PRIORITY:-}" 2)
TOP_CANDIDATES=$(int_or_default "${GT_SEAT_REFILL_TOP_CANDIDATES:-}" 3)
SONNET_MAX=$(int_or_default "${GT_SEAT_REFILL_SONNET_MAX:-}" 1)
SONNET_AGENT="${GT_SEAT_REFILL_SONNET_AGENT:-claude-sonnet}"
SONNET_LABEL="${GT_SEAT_REFILL_SONNET_LABEL:-needs-sonnet}"
CLAIM_TTL=$(int_or_default "${GT_SEAT_REFILL_CLAIM_TTL:-}" 1800)

iso_age() {
  local iso="$1" epoch
  [ -n "$iso" ] || return 1
  epoch=$(jq -rn --arg ts "$iso" '$ts | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601? // empty' 2>/dev/null) || return 1
  [ -n "$epoch" ] || return 1
  printf '%s' "$(( NOW - epoch ))"
}

# An operator hold parks the plugin without touching its gate. A file is the
# guardrail shape this town already uses for a hand brake (ESTOP at the town
# root), and `touch` is a thing an operator can do from a shell prompt.
if [ -e "$HOLD_FILE" ]; then
  skip "sling hold flag present ($HOLD_FILE); not nudging"
fi
if [ -e "$TOWN_ROOT/ESTOP" ]; then
  skip "town estop active ($TOWN_ROOT/ESTOP); not nudging"
fi

[ -f "$CONFIG_FILE" ] || fail "town settings not found at $CONFIG_FILE"

# --- Seats ---------------------------------------------------------------
# A seat is one capped agent class, held as name|agent|cap|selector. The pool
# config is the source: local_agent admits max_local sessions, overflow_agent
# admits max_overflow. A third class — claude-sonnet, which reaches a seat only
# through an explicit `gt sling --agent claude-sonnet` — is not expressible in
# polecat_pool today (gt-xmsqb), so the mayor's policy of holding itself to one
# live sonnet is modeled here as a seat of its own. Set
# GT_SEAT_REFILL_SONNET_MAX=0 to drop it when that policy changes, or when
# gt-xmsqb gives the pool N tiers for this to read instead.

SEATS=""
add_seat() { SEATS+="$1|$2|$3|$4"$'\n'; }

read_config() {
  jq -r ".polecat_pool.${1} // ${2}" "$CONFIG_FILE" 2>/dev/null ||
    fail "could not read polecat_pool.$1 from $CONFIG_FILE"
}

LOCAL_AGENT=$(read_config local_agent '""')
MAX_LOCAL=$(read_config max_local 0)
OVERFLOW_AGENT=$(read_config overflow_agent '""')
MAX_OVERFLOW=$(read_config max_overflow 0)

[ "$LOCAL_AGENT" != "null" ] || LOCAL_AGENT=""
[ "$OVERFLOW_AGENT" != "null" ] || OVERFLOW_AGENT=""

if [ -z "$LOCAL_AGENT" ] && [ -z "$OVERFLOW_AGENT" ]; then
  skip "no polecat_pool in $CONFIG_FILE; this town has no seats to refill"
fi

# max_local: 0 means the local tier is CLOSED (no polecat runs there), which is
# a decision, not an empty seat. An absent or zero max_overflow is the opposite
# — the overflow seat is UNCAPPED, so it is never empty. That asymmetry is the
# pool's own reading of its config and is kept here (gt-xmsqb).
if [ "$MAX_LOCAL" -gt 0 ] && [ -n "$LOCAL_AGENT" ]; then
  add_seat local "$LOCAL_AGENT" "$MAX_LOCAL" any
else
  log "seat local is closed (max_local 0 or no local_agent); not watching it"
fi

if [ -n "$OVERFLOW_AGENT" ]; then
  if [ "$MAX_OVERFLOW" -gt 0 ]; then
    add_seat overflow "$OVERFLOW_AGENT" "$MAX_OVERFLOW" any
  else
    log "seat overflow is uncapped (max_overflow unset); always room, not watching it"
  fi
fi

# The sonnet seat fills only from work that asks for it: a needs-sonnet bead is
# the mayor's own signal that this class of work exists (gt-tq6l). Without one,
# an empty sonnet seat is the resting state, and a nudge about it would be
# noise the mayor learns to ignore.
if [ "$SONNET_MAX" -gt 0 ]; then
  add_seat sonnet "$SONNET_AGENT" "$SONNET_MAX" "label:$SONNET_LABEL"
fi

[ -n "$SEATS" ] || skip "no capped seat configured; nothing can be empty"

# --- Live occupancy ------------------------------------------------------
# The pool's own count is the source of truth: a seat counts the sessions whose
# GT_AGENT is that seat's agent, exactly as sling_pool.go counts them. A nudge
# that named room the next sling would refuse is worse than no nudge (gt-59o9).

SESSIONS_JSON=$(gt polecat list --all --json 2>/dev/null) ||
  fail "gt polecat list --all --json failed; seat occupancy is unknown, not zero"
printf '%s' "$SESSIONS_JSON" | jq -e 'type == "array"' >/dev/null 2>&1 ||
  fail "gt polecat list --all --json did not return an array; seat occupancy is unknown"

# An in-flight sling holds its seat as a claim file before the session exists
# (gt-t8q5). Counting those keeps a seat mid-spawn from reading as empty; a
# claim whose process is gone, or that outlived the pool's own 30m TTL, has
# released its seat and is not counted.
claims_agents() {
  local dir="$TOWN_ROOT/.runtime/polecat-pool-claims" f pid created agent age
  [ -d "$dir" ] || return 0
  for f in "$dir"/*.json; do
    [ -e "$f" ] || continue
    IFS=$'\t' read -r pid created agent < <(
      jq -r '[(.pid // 0), (.created_at // ""), (.agent // "")] | @tsv' "$f" 2>/dev/null
    ) || continue
    [ -n "$agent" ] || continue
    case "$pid" in ''|*[!0-9]*) continue ;; esac
    [ "$pid" -gt 0 ] || continue
    kill -0 "$pid" 2>/dev/null || continue
    age=$(iso_age "$created") || continue
    [ "$age" -le "$CLAIM_TTL" ] || continue
    printf '%s\n' "$agent"
  done
}

LIVE_AGENTS=$(
  {
    printf '%s' "$SESSIONS_JSON" | jq -r '.[] | select(.session_running == true) | .agent // empty' 2>/dev/null
    claims_agents
  } | grep -v '^$' || true
)

agent_live() {
  local agent="$1" n=0 a
  [ -n "$LIVE_AGENTS" ] || { printf '0'; return 0; }
  while IFS= read -r a; do
    [ "$a" = "$agent" ] && n=$((n + 1))
  done <<< "$LIVE_AGENTS"
  printf '%s' "$n"
}

# --- Slingable work ------------------------------------------------------
# gt ready already drops the town's bookkeeping families (wisps, mail,
# handoffs, MRs, agent beads, convoys, formulas). What is left is narrowed to
# the bead's definition of fillable work: a P0-P2 task/bug/feature, unassigned,
# in a rig the pool may serve. Notification envelopes are titles *about* work
# (STATE_COLLAPSE, an escalation) rather than work, so they stay out —
# dispatch-check draws the same line for the same reason (gt-59o9).

operational_rigs() {
  local out rows
  out=$(gt rig list --json 2>/dev/null) || {
    log "SKIP: gt rig list --json failed; cannot tell parked from served rigs"
    return 0
  }
  rows=$(printf '%s' "$out" | jq -r '
    if type == "array" then .[] else empty end
    | select((.status // "" | ascii_downcase) == "operational")
    | select((.name // "") != "")
    | .name
  ' 2>/dev/null) || {
    log "SKIP: gt rig list --json not parseable; cannot tell parked from served rigs"
    return 0
  }
  printf '%s\n' "$rows" | awk 'NF >= 1 && $1 != ""'
}

# rig<TAB>id<TAB>priority<TAB>labels
CANDIDATES=""

while IFS= read -r RIG; do
  [ -n "$RIG" ] || continue
  if [ -e "$TOWN_ROOT/ESTOP.$RIG" ]; then
    log "SKIP $RIG: rig estop active"
    continue
  fi

  out=$(gt ready --rig "$RIG" --json 2>/dev/null) || {
    log "SKIP $RIG: gt ready failed"
    continue
  }
  rows=$(printf '%s' "$out" | jq -r --arg rig "$RIG" --argjson maxp "$MAX_PRIORITY" '
    [ .sources[]? | select(.name == $rig) | .issues[]? ]
    | .[]
    | select((.priority // 99) <= $maxp)
    | select((.issue_type // "") == "task" or (.issue_type // "") == "bug" or (.issue_type // "") == "feature")
    | select((.assignee // "") == "")
    | select((.title // "") | test("^(STATE_COLLAPSE|\\[HIGH\\]|\\[CRITICAL\\]|\\[MEDIUM\\]|main_branch_test:|HANDOFF|merge-slot)") | not)
    | [$rig, .id, (.priority | tostring), ((.labels // []) | join(","))] | @tsv
  ' 2>/dev/null) || {
    log "SKIP $RIG: ready output not parseable"
    continue
  }
  [ -n "$rows" ] && CANDIDATES+="$rows"$'\n'
done < <(operational_rigs)

# Best first: lowest priority number wins, and the id breaks a tie so the same
# board names the same beads twice running.
SORTED_CANDIDATES=$(printf '%s' "$CANDIDATES" | grep -v '^$' | sort -t$'\t' -k3,3n -k2,2 || true)

candidates_for() {
  local selector="$1"
  [ -n "$SORTED_CANDIDATES" ] || return 0
  case "$selector" in
    any)
      printf '%s\n' "$SORTED_CANDIDATES"
      ;;
    label:*)
      local want="${selector#label:}" rig id prio labels
      while IFS=$'\t' read -r rig id prio labels; do
        case ",${labels}," in
          *",${want},"*) printf '%s\t%s\t%s\t%s\n' "$rig" "$id" "$prio" "$labels" ;;
        esac
      done <<< "$SORTED_CANDIDATES"
      ;;
  esac
}

# --- Empty episodes ------------------------------------------------------
# State is per seat: when the seat was first seen empty, and when it was last
# nudged about. A seat that fills is left out of the new state, which forgets
# both, so the next episode starts its own clock. A seat first seen empty
# starts its clock now rather than being treated as long-empty: the plugin can
# only measure what it observes.

state_field() {
  local seat="$1" field="$2"
  [ -f "$STATE_FILE" ] || { printf '0'; return 0; }
  jq -r --arg s "$seat" --arg f "$field" '.episodes[$s][$f] // 0' "$STATE_FILE" 2>/dev/null || printf '0'
}

# seat<TAB>empty_since<TAB>last_nudge, one row per seat still empty.
STATE_ROWS=""
NUDGE_LINES=""

while IFS='|' read -r seat agent cap selector; do
  [ -n "$seat" ] || continue

  live=$(agent_live "$agent")

  if [ "$live" -ge "$cap" ]; then
    log "seat $seat: $live/$cap live ($agent)"
    continue
  fi

  empty_since=$(state_field "$seat" empty_since)
  last_nudge=$(state_field "$seat" last_nudge)
  case "$empty_since" in ''|*[!0-9]*|0) empty_since="$NOW" ;; esac
  case "$last_nudge" in ''|*[!0-9]*) last_nudge=0 ;; esac

  empty_for=$((NOW - empty_since))
  [ "$empty_for" -ge 0 ] || empty_for=0

  seat_candidates=$(candidates_for "$selector")
  seat_count=0
  [ -n "$seat_candidates" ] && seat_count=$(printf '%s\n' "$seat_candidates" | grep -c . || true)

  log "seat $seat: $live/$cap live ($agent), empty ${empty_for}s, $seat_count candidate(s)"

  # Empty with nothing to take it is not a fault. The episode keeps running so
  # that work appearing later is nudged about promptly.
  if [ "$seat_count" -gt 0 ] && [ "$empty_for" -ge "$EMPTY_SECONDS" ]; then
    if [ "$last_nudge" -gt 0 ] && [ "$((NOW - last_nudge))" -lt "$NUDGE_SECONDS" ]; then
      log "seat $seat: nudged $((NOW - last_nudge))s ago; holding until ${NUDGE_SECONDS}s"
    else
      top=$(printf '%s\n' "$seat_candidates" | head -n "$TOP_CANDIDATES" |
        awk -F'\t' '{ printf "%s%s (P%s %s)", (NR > 1 ? ", " : ""), $2, $3, $1 }')
      NUDGE_LINES+="seat ${seat} (agent ${agent}) has been empty ${empty_for}s at ${live}/${cap} live — candidates: ${top}"$'\n'
      last_nudge="$NOW"
    fi
  fi

  STATE_ROWS+="$seat"$'\t'"$empty_since"$'\t'"$last_nudge"$'\n'
done <<< "$SEATS"

# Drop episodes for seats that filled or vanished: a seat absent from the new
# state starts a fresh episode next run.
write_state() {
  local json
  if [ -n "$STATE_ROWS" ]; then
    json=$(printf '%s' "$STATE_ROWS" | jq -Rn '
      [ inputs | select(length > 0) | split("\t")
        | { key: .[0], value: { empty_since: (.[1] | tonumber), last_nudge: (.[2] | tonumber) } } ]
      | from_entries' 2>/dev/null) || fail "could not render episode state"
  else
    json='{}'
  fi

  mkdir -p "$(dirname "$STATE_FILE")" || fail "could not create $(dirname "$STATE_FILE")"
  printf '{"version":1,"episodes":%s}\n' "$json" > "$STATE_FILE.tmp" ||
    fail "could not write $STATE_FILE.tmp"
  mv "$STATE_FILE.tmp" "$STATE_FILE" || fail "could not replace $STATE_FILE"
}

if [ -z "$NUDGE_LINES" ]; then
  write_state
  skip "no seat has been empty long enough with work ready"
fi

# --- Nudge ---------------------------------------------------------------
# One nudge per run, naming every seat that earned one. It states the seat, how
# long it has been empty, and the beads that could take it; the decision it
# asks for is the mayor's own (town directive, idle seats are a fault).

message="seat-refill: polecat seat(s) empty with work ready.
$(printf '%s' "$NUDGE_LINES" | sed 's/^/- /')
Sling now, or mail the overseer why not — idle seats are a fault."

seat_n=$(printf '%s\n' "$NUDGE_LINES" | grep -c . || true)
log "nudging $MAYOR_TARGET: $seat_n empty seat(s) with work"

# Default (wait-idle) delivery, the same path the daemon's own mayor_dispatch
# patrol uses: it hands the message to an idle mayor directly, queues if the
# mayor is mid-turn rather than interrupting it, and honors DND.
#
# The bound keeps a wedged nudge inside this plugin's own 3m timeout, and it
# must outlast gt nudge's own budget: wait-idle polls 15s for idle, queues, then
# watches up to 60s more (internal/cmd/nudge.go waitIdleTimeout,
# idleWatcherTimeout). At 60s it killed every nudge to a mayor busy for more
# than ~45s — a normal nudge, already queued — and reported it lost (gt-hen4o).
# 90s covers 15s+60s with slack; pre-nudge work plus 90s stays under 3m.
#
# State is written only after the nudge lands. Persisting the episode first
# would record a nudge that never arrived and buy the seat 15 minutes of
# silence — the one outcome this plugin exists to prevent.
NUDGE_BOUND=90
nudge_rc=0
timeout "$NUDGE_BOUND" gt nudge "$MAYOR_TARGET" "$message" 2>&1 || nudge_rc=$?
if [ "$nudge_rc" -eq 124 ]; then
  # Past its whole budget gt nudge is wedged. Wait-idle queues before it
  # watches, so the message may still be delivered on the mayor's next drain;
  # say that rather than claiming it was lost.
  fail "gt nudge $MAYOR_TARGET timed out after ${NUDGE_BOUND}s; delivery unconfirmed (it may be queued for the mayor's next turn)"
elif [ "$nudge_rc" -ne 0 ]; then
  fail "gt nudge $MAYOR_TARGET failed (exit $nudge_rc); the empty seat was not reported"
fi

write_state
log "nudged ${seat_n} seat(s)"
