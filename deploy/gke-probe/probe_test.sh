#!/usr/bin/env bash
# Tests for deploy/gke-probe/probe.sh (gt-ycdmg).
#
# The wizard creates one billable Spot node and must also remove it, so the
# assertions are about which gcloud and kubectl calls happen and what manifests
# reach kubectl, never about output formatting. Stub gcloud and kubectl on PATH
# keep the cluster list as state, record every call and save each manifest the
# script pipes to `kubectl apply -f -`. HOME is the temp dir, so results and the
# kubeconfig land there and nothing can reach a real project. Every prompt is fed
# from a finite stdin, so a prompt the script did not expect ends at EOF
# instead of waiting.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROBE="$SCRIPT_DIR/probe.sh"

TMP="$(mktemp -d "${TMPDIR:-/tmp}/gke-probe-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); if [ $# -gt 1 ]; then printf '%s\n' "$2" | sed 's/^/    | /'; fi; }
check() { # check DESCRIPTION COMMAND...
  local desc=$1
  shift
  if "$@"; then pass "$desc"; else fail "$desc"; fi
}
contains() { case "$2" in *"$1"*) return 0 ;; *) return 1 ;; esac; }
lacks() { ! contains "$1" "$2"; }

mkdir -p "$TMP/bin"
cat >"$TMP/bin/gcloud" <<'STUB'
#!/usr/bin/env bash
echo "gcloud $*" >>"$CALLS"
case "$1 $2" in
  "auth list") [ -n "${STUB_NO_ACCT:-}" ] || echo "tester@example.com" ;;
  "projects describe") echo "ACTIVE" ;;
  "billing projects")
    [ -n "${STUB_BILLING_FAIL:-}" ] && exit 1
    [ -n "${STUB_NO_BILLING:-}" ] && echo "False" || echo "True"
    ;;
  "compute ssh")
    [ -n "${STUB_SSH_FAIL:-}" ] && exit 1
    printf 'LSM=landlock,lockdown,yama,integrity,apparmor\nCMDLINE=BOOT_IMAGE=/boot/vmlinuz lsm=landlock,lockdown,yama,integrity,apparmor\n'
    ;;
  "services enable") ;;
  "container clusters")
    case "$3" in
      list)
        [ -n "${STUB_LIST_FAIL:-}" ] && exit 1
        if [[ "$*" == *"gt-purpose=openshell-probe"* ]]; then
          [ -f "$STATE/clusters" ] && cat "$STATE/clusters"
        else
          echo "prod us-central1-a"
        fi
        ;;
      create)
        echo "$4 us-central1-a" >>"$STATE/clusters"
        [ -n "${STUB_CREATE_FAIL:-}" ] && exit 1
        ;;
      delete) grep -v "^$4 " "$STATE/clusters" >"$STATE/clusters.new" || true; mv "$STATE/clusters.new" "$STATE/clusters" ;;
    esac
    ;;
esac
exit 0
STUB
cat >"$TMP/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
echo "kubectl $*" >>"$CALLS"
case "$1" in
  apply)
    n=$(( $(ls "$STATE" | grep -c '^apply-') + 1 ))
    cat >"$STATE/apply-$n.yaml"
    ;;
  get)
    case "$*" in
      *"pod landlock-probe"*status.phase*) echo "${STUB_PHASE:-Succeeded}" ;;
      *metadata.name*) echo "node-1" ;;
      *osImage*) echo "Ubuntu 24.04.3 LTS" ;;
      *kernelVersion*) echo "6.8.0-1066-gke" ;;
    esac
    ;;
  logs)
    case "$2" in
      landlock-probe) echo "LANDLOCK_ABI ${STUB_ABI:-4}" ;;
    esac
    ;;
esac
exit 0
STUB
chmod +x "$TMP/bin/gcloud" "$TMP/bin/kubectl"

# scenario NAME: fresh state and call log; HOME is inside the temp dir.
scenario() {
  S="$TMP/$1"
  rm -rf "$S"
  mkdir -p "$S/state" "$S/home"
  : >"$S/calls.log"
}
# probe INPUT [ENV=VAL...] -- ARGS...: run the wizard on the scenario.
probe() {
  local input=$1
  shift
  local envs=()
  while [ $# -gt 0 ] && [ "$1" != "--" ]; do envs+=("$1"); shift; done
  [ $# -gt 0 ] && shift
  set +e
  OUT="$(printf '%b' "$input" | env ${envs[@]+"${envs[@]}"} HOME="$S/home" CALLS="$S/calls.log" STATE="$S/state" \
    PATH="$TMP/bin:$PATH" bash "$PROBE" "$@" 2>&1)"
  RC=$?
  set -e
  CALLS_LOG="$(cat "$S/calls.log")"
  RESULTS_FILE="$(ls "$S"/home/gt/.runtime/gke-probe/results-*.txt 2>/dev/null | head -1 || true)"
  RESULTS_CONTENT=""
  if [ -n "$RESULTS_FILE" ]; then RESULTS_CONTENT="$(cat "$RESULTS_FILE")"; fi
}

echo "dry run"
scenario dry
probe '\ny\ny\ny\ny\n' DRY_RUN=1 --
check "dry run exits 0" test "$RC" -eq 0
check "dry run calls no gcloud or kubectl" test ! -s "$S/calls.log"
check "dry run shows the Ubuntu image flag" contains "--image-type=UBUNTU_CONTAINERD" "$OUT"
check "dry run shows the Spot flag" contains "--spot" "$OUT"
check "dry run shows Dataplane V2 (NetworkPolicy enforcement)" contains "--enable-dataplane-v2" "$OUT"
check "dry run shows the purpose and ttl labels" contains "gt-purpose=openshell-probe,gt-ttl=" "$OUT"
check "dry run shows the ssh node-facts command" contains "gcloud compute ssh" "$OUT"
check "dry run records no verdict" lacks "VERDICT" "$OUT"

echo "full run"
scenario full
probe '\ny\ny\ny\n' --
check "full run exits 0" test "$RC" -eq 0
FULL_OUT="$OUT"
create=$(grep 'container clusters create' "$S/calls.log" || true)
check "create carries the Ubuntu image" contains "--image-type=UBUNTU_CONTAINERD" "$create"
check "create carries Spot" contains "--spot" "$create"
check "create carries Dataplane V2" contains "--enable-dataplane-v2" "$create"
check "create carries the purpose label" contains "gt-purpose=openshell-probe" "$create"
check "create carries a ttl label" contains "gt-ttl=" "$create"
check "create is in the default project" contains "--project=next-fractals-redux" "$create"
probe_doc="$(cat "$S/state/apply-1.yaml")"
check "only one pod manifest is applied" test ! -f "$S/state/apply-2.yaml"
check "no applied manifest mounts the host filesystem" lacks "hostPath" "$probe_doc"
check "the probe pod runs as non-root" contains "runAsNonRoot: true" "$probe_doc"
check "the probe pod drops every capability" contains 'capabilities: {drop: ["ALL"]}' "$probe_doc"
check "the probe pod uses the default seccomp profile" contains "type: RuntimeDefault" "$probe_doc"
check "the probe pod refuses privilege escalation" contains "allowPrivilegeEscalation: false" "$probe_doc"
check "node facts come from ssh, not a host mount" contains "gcloud compute ssh node-1" "$CALLS_LOG"
check "results record the kernel" contains "kernel=6.8.0-1066-gke" "$RESULTS_CONTENT"
check "results record the active security modules" contains "LSM=landlock" "$RESULTS_CONTENT"
check "results record the boot line" contains "CMDLINE=" "$RESULTS_CONTENT"
check "results record the probe pod succeeded" contains "probe_pod=ok" "$RESULTS_CONTENT"
check "ABI 4 is a PASS verdict" contains "VERDICT PASS landlock_abi=4" "$RESULTS_CONTENT"
check "the cluster is deleted at the end" contains "container clusters delete gt-probe-" "$CALLS_LOG"
check "no cluster is left in the stub state" test ! -s "$S/state/clusters"
check "a clean run does not warn about a leftover cluster" lacks "may still be running" "$FULL_OUT"

echo "failing abi"
scenario fail
probe '\ny\ny\ny\n' STUB_ABI=-38 --
check "ABI error -38 is a FAIL verdict" contains "VERDICT FAIL landlock_abi=-38" "$RESULTS_CONTENT"
check "a failing probe still deletes the cluster" contains "container clusters delete gt-probe-" "$CALLS_LOG"
check "a failing probe leaves no cluster" test ! -s "$S/state/clusters"

echo "failed probe pod"
scenario podfail
probe '\ny\ny\ny\n' STUB_PHASE=Failed --
results="$(cat "$S"/home/gt/.runtime/gke-probe/results-*.txt)"
check "a failed pod is a FAIL verdict, not a missing one" contains "VERDICT FAIL" "$results"
check "the failed pod is recorded as failed" contains "probe_pod=failed" "$results"
check "a failed pod does not hang the run" test "$RC" -eq 0

echo "ssh node facts unavailable"
scenario noss
probe '\ny\ny\ny\n' STUB_SSH_FAIL=1 --
results="$(cat "$S"/home/gt/.runtime/gke-probe/results-*.txt)"
check "a failed ssh still records a PASS or FAIL verdict" contains "VERDICT PASS" "$results"
check "a failed ssh is recorded as unavailable" contains "node_facts=unavailable" "$results"
check "a failed ssh leaves a manual step" contains "read the LSM list on node-1" "$OUT"

echo "create fails midway"
scenario cfail
probe '\ny\n' STUB_CREATE_FAIL=1 --
check "a failed create exits non-zero" test "$RC" -ne 0
check "a failed create deletes the half-created cluster" contains "container clusters delete gt-probe-" "$CALLS_LOG"
check "the automatic delete leaves no cluster behind" test ! -s "$S/state/clusters"

echo "create fails, keep requested"
scenario ckeep
probe '\ny\n' STUB_CREATE_FAIL=1 GT_PROBE_KEEP=1 --
check "GT_PROBE_KEEP=1 leaves the cluster for the human" lacks "clusters delete" "$CALLS_LOG"
check "GT_PROBE_KEEP=1 warns it still bills" contains "may still be running and billing" "$OUT"
check "the warning names the teardown command" contains "teardown" "$OUT"
check "the cluster survives in the stub state" test -s "$S/state/clusters"

echo "declined cost confirmation"
scenario decline
probe '\nn\n' --
check "declining creates nothing" lacks "clusters create" "$CALLS_LOG"
check "declining exits 0" test "$RC" -eq 0

echo "preflight refusals"
scenario noacct
probe '\n\n' STUB_NO_ACCT=1 --
check "no active account stops the run" test "$RC" -ne 0
check "no active account creates nothing" lacks "clusters create" "$CALLS_LOG"
scenario nobilling
probe '\n' STUB_NO_BILLING=1 --
check "billing disabled stops the run" test "$RC" -ne 0
check "billing disabled creates nothing" lacks "clusters create" "$CALLS_LOG"
scenario listfail
probe '\n' STUB_LIST_FAIL=1 --
check "a failed cluster list stops the run" test "$RC" -ne 0
check "a failed cluster list creates nothing" lacks "clusters create" "$CALLS_LOG"

echo "leftover cluster"
scenario leftover
echo "gt-probe-old us-central1-a" >"$S/state/clusters"
probe '\ny\n' --
check "a leftover probe cluster stops the run" test "$RC" -ne 0
check "a leftover probe cluster blocks a second create" lacks "clusters create" "$CALLS_LOG"

echo "teardown"
scenario td
printf 'gt-probe-a us-central1-a\ngt-probe-b us-east1\n' >"$S/state/clusters"
probe '\ny\ny\ny\n' -- teardown
check "teardown reads the project before deleting" contains "gcloud projects describe next-fractals-redux" "$CALLS_LOG"
check "teardown lists only labelled clusters" contains "--filter=resourceLabels.gt-purpose=openshell-probe" "$CALLS_LOG"
check "teardown deletes the first labelled cluster" contains "container clusters delete gt-probe-a" "$CALLS_LOG"
check "teardown deletes the second labelled cluster" contains "container clusters delete gt-probe-b" "$CALLS_LOG"
check "a zonal cluster is deleted with --zone" contains "--zone us-central1-a" "$CALLS_LOG"
check "a regional cluster is deleted with --region" contains "--region us-east1" "$CALLS_LOG"
check "teardown never names the unlabelled cluster" lacks "prod" "$CALLS_LOG"
scenario td_no
printf 'gt-probe-a us-central1-a\n' >"$S/state/clusters"
probe '\ny\nn\n' -- teardown
check "declining one cluster deletes nothing" lacks "clusters delete" "$CALLS_LOG"
check "a declined cluster is left as a manual step" contains "delete cluster gt-probe-a" "$OUT"
scenario td_project
printf 'gt-probe-a us-central1-a\n' >"$S/state/clusters"
probe '\n' -- teardown
check "declining the project confirmation deletes nothing" lacks "clusters delete" "$CALLS_LOG"
scenario td_other
printf 'gt-probe-a us-central1-a\n' >"$S/state/clusters"
probe '\ny\ny\n' GT_PROBE_PROJECT=some-other-project -- teardown
check "GT_PROBE_PROJECT scopes the teardown" contains "--project some-other-project" "$CALLS_LOG"
check "teardown never touches the default project when overridden" lacks "next-fractals-redux" "$CALLS_LOG"

if [ "$FAIL" -eq 0 ]; then
  echo "probe_test.sh: all $PASS cases passed"
else
  echo "probe_test.sh: $FAIL of $((PASS + FAIL)) cases failed"
  exit 1
fi
