#!/usr/bin/env bash
#
# A wizard walks a human through a manual procedure, step by step.
# The helpers below are the /wizard template's, minus the ones this probe does
# not use; the per-step stages start at the STAGES marker.

set -euo pipefail

# ──────────────────────────────────────────────────────────────────────────
# Wizard library: delightful, consistent UX.
# ──────────────────────────────────────────────────────────────────────────

if [[ -t 1 ]] && command -v tput >/dev/null 2>&1 && [[ "$(tput colors 2>/dev/null || echo 0)" -ge 8 ]]; then
  BOLD=$(tput bold); DIM=$(tput dim); RESET=$(tput sgr0)
  BLUE=$(tput setaf 4); GREEN=$(tput setaf 2); YELLOW=$(tput setaf 3)
else
  BOLD=""; DIM=""; RESET=""; BLUE=""; GREEN=""; YELLOW=""
fi

# Author sets this at the top of the stages section.
TOTAL_STAGES=0

_STAGE_INDEX=0
SKIPPED=()        # things the human still has to do by hand

# _clear wipes the terminal so only the current step is on screen. No-op when
# output isn't a terminal, so piped logs stay readable.
_clear() {
  [[ -t 1 ]] || return 0
  if command -v tput >/dev/null 2>&1 && tput clear 2>/dev/null; then return 0; fi
  printf '\033[2J\033[3J\033[H'
}

# banner "Title" shows the opening frame: what this wizard does.
banner() {
  _clear
  printf '\n%s%s  %s%s\n' "$BOLD" "$BLUE" "$1" "$RESET"
  printf '%s  %s stages%s\n\n' "$DIM" "$TOTAL_STAGES" "$RESET"
  printf '%s  This wizard runs the commands and asks before every billable step.\n' "$DIM"
  printf '  It deletes the cluster at the end, and deletes one it already created\n'
  printf '  if the run fails. Anything left is listed when the wizard stops.%s\n' "$RESET"
  pause "Ready to start?"
}

# stage "Name" clears the screen, then announces a stage and shows progress.
# Clearing keeps only the current step on screen.
stage() {
  _clear
  _STAGE_INDEX=$((_STAGE_INDEX + 1))
  printf '\n%s%s▸ Stage %s/%s · %s%s\n' \
    "$BOLD" "$BLUE" "$_STAGE_INDEX" "$TOTAL_STAGES" "$1" "$RESET"
}

# say "..." prints a plain instruction line.
say()  { printf '  %s\n' "$1"; }
# step "..." is a numbered-feeling action the human takes in another window.
step() { printf '  %s•%s %s\n' "$BLUE" "$RESET" "$1"; }
note() { printf '  %s%s%s\n' "$DIM" "$1" "$RESET"; }
warn() { printf '  %s⚠ %s%s\n' "$YELLOW" "$1" "$RESET"; }

# pause "msg" waits for the human to confirm they've done the manual part.
pause() {
  printf '  %s%s%s ' "$DIM" "${1:-Press Enter to continue}" "$RESET"
  read -r _ || true
}

# confirm "question" is a y/N gate; returns success on yes.
confirm() {
  local reply=""
  printf '  %s? %s [y/N] ' "$YELLOW" "$1"
  read -r reply || true
  [[ "$reply" =~ ^[Yy] ]]
}

# finish clears, then shows a closing summary of everything configured.
finish() {
  _clear
  printf '\n%s%s  ✓ Wizard complete%s\n' "$BOLD" "$GREEN" "$RESET"
  if (( ${#SKIPPED[@]} )); then
    printf '\n'; warn "still to do by hand:"
    for s in "${SKIPPED[@]}"; do note "  - $s"; done
  fi
  printf '\n'
}

# ──────────────────────────────────────────────────────────────────────────
# STAGES: probe whether OpenShell's sandbox boundary can run on a GKE Standard
# Ubuntu node (gt-ycdmg). The human runs this; agents never do. It creates ONE
# billable Spot node in GCP project $PROJECT and always offers to delete it.
#
#   bash probe.sh              the probe (asks before every billable step)
#   DRY_RUN=1 bash probe.sh    print every gcloud/kubectl command, run none
#   bash probe.sh teardown     delete leftover probe clusters (label gt-purpose)
# ──────────────────────────────────────────────────────────────────────────

PROJECT="${GT_PROBE_PROJECT:-next-fractals-redux}"
ZONE="${GT_PROBE_ZONE:-us-central1-a}"
MACHINE="${GT_PROBE_MACHINE:-e2-standard-4}"
PURPOSE="openshell-probe"
STAMP="$(date -u +%m%d%H%M)"
NAME="${GT_PROBE_NAME:-gt-probe-$STAMP}"
TTL="$(( $(date -u +%s) + 4 * 3600 ))" # epoch seconds, four hours out
DRY_RUN="${DRY_RUN:-0}"
OUT_DIR="${GT_PROBE_OUT:-$HOME/gt/.runtime/gke-probe}"
RESULTS="$OUT_DIR/results-$(date -u +%Y%m%d-%H%M).txt"
CLUSTER_CREATED=0
CLUSTER_DELETED=0

# run prints the command and, unless DRY_RUN=1, runs it.
run() {
  note "+ $*"
  [[ "$DRY_RUN" == 1 ]] && return 0
  "$@"
}

# capture VAR cmd...: VAR gets the command's stdout and capture returns the
# command's status, so a gcloud that failed is never mistaken for empty output.
# VAR is "<dry-run>" under DRY_RUN=1.
capture() {
  local var="$1" out; shift
  note "+ $*"
  if [[ "$DRY_RUN" == 1 ]]; then printf -v "$var" '%s' "<dry-run>"; return 0; fi
  out="$("$@")" || return $?
  printf -v "$var" '%s' "$out"
}

record() { [[ "$DRY_RUN" == 1 ]] && return 0; printf '%s\n' "$1" >>"$RESULTS"; }

# wait_pod POD TIMEOUT_S polls until the pod reaches a terminal phase and
# returns 1 on Failed or timeout; `kubectl wait` on phase=Succeeded would sit
# out its whole timeout on a pod that failed instead.
wait_pod() {
  local pod="$1" timeout="$2" phase deadline
  if [[ "$DRY_RUN" == 1 ]]; then note "(dry-run) no wait for $pod"; return 0; fi
  deadline=$(( SECONDS + timeout ))
  while (( SECONDS < deadline )); do
    capture phase kubectl get pod "$pod" -o jsonpath='{.status.phase}' || return 1
    case "$phase" in
      Succeeded) return 0 ;;
      Failed) return 1 ;;
    esac
    sleep 3
  done
  return 1
}

# zone_flag LOCATION: --zone for a zonal location (us-central1-a), --region for
# a regional one (us-central1), so a leftover cluster from either is deletable.
zone_flag() { [[ "$1" =~ -[a-z]$ ]] && printf -- '--zone' || printf -- '--region'; }

# delete_probe_clusters deletes every cluster labelled gt-purpose=$PURPOSE in
# $PROJECT, one confirmation each.
delete_probe_clusters() {
  local rows name zone
  if ! capture rows gcloud container clusters list --project "$PROJECT" \
    --filter="resourceLabels.gt-purpose=$PURPOSE" --format='value(name,location)'; then
    warn "could not list clusters in $PROJECT (gcloud failed); nothing deleted."
    return 1
  fi
  if [[ "$DRY_RUN" == 1 ]]; then
    run gcloud container clusters delete "$NAME" --project "$PROJECT" --zone "$ZONE" --quiet
    return 0
  fi
  if [[ -z "$rows" ]]; then say "no probe clusters found."; return 0; fi
  # fd 3 carries the list so confirm reads the human's stdin, not the list.
  while read -r name zone <&3; do
    if [[ -z "$name" ]]; then continue; fi
    if confirm "Delete cluster $name ($zone) in $PROJECT?"; then
      if run gcloud container clusters delete "$name" --project "$PROJECT" \
        "$(zone_flag "$zone")" "$zone" --quiet; then
        if [[ "$name" == "$NAME" ]]; then CLUSTER_DELETED=1; fi
      else
        warn "delete of $name failed."
        SKIPPED+=("delete cluster $name ($zone) by hand")
      fi
    else
      SKIPPED+=("delete cluster $name ($zone) by hand")
    fi
  done 3<<<"$rows"
}

# cleanup_on_exit runs on every exit. A failed run that had already created the
# cluster deletes it, since otherwise a stack trace is all that stands between
# the human and a billing cluster they may not notice (gt-ycdmg).
cleanup_on_exit() {
  local rc=$?
  trap - EXIT
  if [[ "$CLUSTER_CREATED" == 1 && "$CLUSTER_DELETED" == 0 && "$DRY_RUN" != 1 ]]; then
    if (( rc != 0 )) && [[ "${GT_PROBE_KEEP:-0}" != 1 ]]; then
      printf '\n'
      warn "the run failed; deleting cluster $NAME so it stops billing (GT_PROBE_KEEP=1 keeps it)."
      if gcloud container clusters delete "$NAME" --project "$PROJECT" --zone "$ZONE" --quiet; then
        CLUSTER_DELETED=1
      else
        warn "the delete failed too."
      fi
    fi
  fi
  if [[ "$CLUSTER_CREATED" == 1 && "$CLUSTER_DELETED" == 0 && "$DRY_RUN" != 1 ]]; then
    printf '\n'
    warn "cluster $NAME may still be running and billing."
    note "delete it with:  bash $0 teardown"
    note "or list by hand:  gcloud container clusters list --project $PROJECT --filter=resourceLabels.gt-purpose=$PURPOSE"
  fi
  exit "$rc"
}
trap cleanup_on_exit EXIT

# node_facts_print reads the two host files that answer "which LSMs are active
# and what was the boot line". No pod can read securityfs unprivileged, and a
# hostPath pod would be a root mount on the node, so this goes over ssh.
node_facts_cmd() {
  printf '%s' 'echo LSM=$(cat /sys/kernel/security/lsm); echo CMDLINE=$(cat /proc/cmdline)'
}

run_wizard() {

if [[ "${1:-}" == teardown ]]; then
  TOTAL_STAGES=2
  banner "GKE probe: teardown"
  stage "Check the project"
  say "Project: $PROJECT   (override with GT_PROBE_PROJECT=<project-id>)"
  if ! capture PROJ_STATE gcloud projects describe "$PROJECT" --format='value(lifecycleState)'; then
    warn "cannot read project $PROJECT with the active account; nothing deleted."
    exit 1
  fi
  say "project state: $PROJ_STATE"
  stage "Delete probe clusters"
  say "Only clusters labelled gt-purpose=$PURPOSE in $PROJECT are listed."
  confirm "Delete probe clusters in $PROJECT?" || { say "Nothing deleted."; finish; return 0; }
  delete_probe_clusters || true
  finish
  return 0
fi

TOTAL_STAGES=7
banner "GKE probe: can OpenShell's boundary run on a Ubuntu node?"

stage "Preflight: account, project, billing"
say "Project: $PROJECT   zone: $ZONE   machine: $MACHINE (Spot, 1 node)"
command -v gcloud >/dev/null 2>&1 || [[ "$DRY_RUN" == 1 ]] || { warn "gcloud is not installed"; exit 1; }
command -v kubectl >/dev/null 2>&1 || [[ "$DRY_RUN" == 1 ]] || { warn "kubectl is not installed (gcloud components install kubectl)"; exit 1; }
if ! capture ACCT gcloud auth list --filter=status:ACTIVE --format='value(account)'; then
  warn "gcloud auth list failed; run gcloud auth login and try again."; exit 1
fi
if [[ -z "$ACCT" ]]; then
  warn "no active gcloud account."
  step "In another terminal run:  gcloud auth login   then come back."
  pause "Press Enter once you are logged in"
  if ! capture ACCT gcloud auth list --filter=status:ACTIVE --format='value(account)' || [[ -z "$ACCT" ]]; then
    warn "still no active gcloud account; nothing created."; exit 1
  fi
fi
say "Active account: $ACCT"
if ! capture PROJ_STATE gcloud projects describe "$PROJECT" --format='value(lifecycleState)'; then
  warn "cannot read project $PROJECT with account $ACCT; nothing created."; exit 1
fi
say "Project state: $PROJ_STATE"
if ! capture BILLING gcloud billing projects describe "$PROJECT" --format='value(billingEnabled)'; then
  warn "could not read the billing status of $PROJECT; nothing created."; exit 1
fi
say "Billing enabled: $BILLING"
[[ "$BILLING" == "True" || "$DRY_RUN" == 1 ]] || { warn "billing is not enabled on $PROJECT"; exit 1; }
# A failed list is not an empty list: refuse to create a second cluster blind.
if ! capture EXISTING gcloud container clusters list --project "$PROJECT" \
  --filter="resourceLabels.gt-purpose=$PURPOSE" --format='value(name)'; then
  warn "could not list probe clusters in $PROJECT; nothing created."; exit 1
fi
[[ -z "$EXISTING" || "$DRY_RUN" == 1 ]] || { warn "a probe cluster already exists: $EXISTING"; note "run: bash $0 teardown"; exit 1; }

stage "Enable the Kubernetes Engine API"
say "This is free; it only switches the API on for the project."
run gcloud services enable container.googleapis.com --project "$PROJECT"

stage "Cost check: one Spot node, deleted at the end"
say "Creating cluster $NAME: GKE Standard, Ubuntu containerd image, Spot, Dataplane V2."
say "Cost is a few cents an hour for the node plus GKE's cluster fee (a free-tier credit"
say "may cover one zonal cluster). Check the current numbers at cloud.google.com/kubernetes-engine/pricing."
say "Labels: gt-purpose=$PURPOSE, gt-ttl=$TTL (epoch; four hours from now)."
confirm "Create the cluster now?" || { say "Nothing created."; exit 0; }

stage "Create the cluster (takes a few minutes)"
CLUSTER_CREATED=1 # set first: a create that fails midway can still leave a billing cluster
run gcloud container clusters create "$NAME" \
  --project="$PROJECT" --zone="$ZONE" --num-nodes=1 --machine-type="$MACHINE" \
  --image-type=UBUNTU_CONTAINERD --spot --enable-dataplane-v2 \
  --labels="gt-purpose=$PURPOSE,gt-ttl=$TTL"
[[ "$DRY_RUN" == 1 ]] || mkdir -p "$OUT_DIR"
export KUBECONFIG="$OUT_DIR/kubeconfig"
run gcloud container clusters get-credentials "$NAME" --project "$PROJECT" --zone "$ZONE"
record "probe $NAME project=$PROJECT zone=$ZONE started=$(date -u +%FT%TZ)"

stage "Node facts: OS image, kernel, active security modules"
if ! capture NODE kubectl get nodes -o jsonpath='{.items[0].metadata.name}'; then
  warn "could not read the node name from the cluster."
fi
capture OSIMG kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.osImage}' || warn "could not read the OS image."
capture KERNEL kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.kernelVersion}' || warn "could not read the kernel."
say "node: $NODE"; say "os image: $OSIMG"; say "kernel: $KERNEL"
record "node=$NODE"; record "os_image=$OSIMG"; record "kernel=$KERNEL"
say "Reading /sys/kernel/security/lsm and /proc/cmdline over ssh; no pod mounts the node."
if capture FACTS gcloud compute ssh "$NODE" --project "$PROJECT" --zone "$ZONE" \
  --tunnel-through-iap --quiet --command "$(node_facts_cmd)"; then
  say "$FACTS"
  record "$FACTS"
else
  warn "ssh to $NODE failed, so the LSM list and boot line are missing from the results."
  step "Optional, in another terminal:  $(node_facts_cmd)"
  SKIPPED+=("read the LSM list on $NODE")
  record "node_facts=unavailable (ssh failed)"
fi

stage "Probe pod: Landlock ABI from an unprivileged, capability-free pod"
say "This pod has the same restrictions OpenShell's workload pod has: non-root,"
say "all capabilities dropped, the cluster's default seccomp profile, no host mounts."
PROBE_OK=0
if run kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata: {name: landlock-probe, labels: {gt-purpose: openshell-probe}}
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 10001
    seccompProfile: {type: RuntimeDefault}
  containers:
  - name: probe
    image: python:3.12-slim
    command: ["python3", "-c", "import ctypes; l=ctypes.CDLL(None,use_errno=True); r=l.syscall(444,None,0,1); print('LANDLOCK_ABI', r if r>=0 else -ctypes.get_errno())"]
    securityContext:
      allowPrivilegeEscalation: false
      capabilities: {drop: ["ALL"]}
EOF
then
  wait_pod landlock-probe 240 && PROBE_OK=1 || warn "the probe pod did not succeed."
else
  warn "the probe pod could not be created."
fi
ABI=""
if [[ "$PROBE_OK" == 1 ]]; then
  capture ABI kubectl logs landlock-probe || warn "could not read the probe pod's logs."
  say "$ABI"
  record "$ABI"
fi
record "probe_pod=$([[ "$PROBE_OK" == 1 ]] && echo ok || echo failed)"
run kubectl delete pod landlock-probe --wait=false || true
# Everything except a clear pass is a FAIL: a failed pod, an empty result and an
# error code all mean the boundary is not proven (gt-ycdmg).
ABI_NUM="${ABI##* }"
if [[ "$DRY_RUN" == 1 ]]; then
  note "(dry-run) no verdict recorded"
elif [[ "$PROBE_OK" == 1 && "$ABI_NUM" =~ ^[0-9]+$ && "$ABI_NUM" -ge 3 ]]; then
  say "PASS: Landlock ABI $ABI_NUM is at least 3 (OpenShell's floor)."
  record "VERDICT PASS landlock_abi=$ABI_NUM"
else
  warn "FAIL: Landlock ABI result '$ABI_NUM' is below 3, an error code (-38 = not built in, -95 = not enabled), or missing."
  record "VERDICT FAIL landlock_abi=${ABI_NUM:-missing} probe_pod=$([[ "$PROBE_OK" == 1 ]] && echo ok || echo failed)"
fi
note "Not covered here: seccomp ADDFD under this node's containerd/AppArmor, and the chart."
note "A pass means the next bead (agent-sandbox + OpenShell chart smoke sandbox) is worth running."

stage "Teardown: delete the cluster so nothing keeps billing"
say "Results are in $RESULTS"
delete_probe_clusters || true

finish
}

run_wizard "$@"; exit
