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
  "auth list") echo "tester@example.com" ;;
  "billing projects") echo "True" ;;
  "container clusters")
    case "$3" in
      list)
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
      *metadata.name*) echo "node-1" ;;
      *osImage*) echo "Ubuntu 24.04.3 LTS" ;;
      *kernelVersion*) echo "6.8.0-1066-gke" ;;
    esac
    ;;
  logs)
    case "$2" in
      node-facts) printf 'LSM=landlock,lockdown,yama,integrity,apparmor\nCMDLINE=BOOT_IMAGE=/boot/vmlinuz\n' ;;
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

echo "full run"
scenario full
probe '\ny\ny\ny\ny\n' --
check "full run exits 0" test "$RC" -eq 0
FULL_OUT="$OUT"
create=$(grep 'container clusters create' "$S/calls.log" || true)
check "create carries the Ubuntu image" contains "--image-type=UBUNTU_CONTAINERD" "$create"
check "create carries Spot" contains "--spot" "$create"
check "create carries Dataplane V2" contains "--enable-dataplane-v2" "$create"
check "create carries the purpose label" contains "gt-purpose=openshell-probe" "$create"
check "create carries a ttl label" contains "gt-ttl=" "$create"
check "create is in the default project" contains "--project=next-fractals-redux" "$create"
node_doc="$(cat "$S/state/apply-1.yaml")"
probe_doc="$(cat "$S/state/apply-2.yaml")"
check "the probe pod runs as non-root" contains "runAsNonRoot: true" "$probe_doc"
check "the probe pod drops every capability" contains 'capabilities: {drop: ["ALL"]}' "$probe_doc"
check "the probe pod uses the default seccomp profile" contains "type: RuntimeDefault" "$probe_doc"
check "the probe pod refuses privilege escalation" contains "allowPrivilegeEscalation: false" "$probe_doc"
check "the probe pod has no host mount" lacks "hostPath" "$probe_doc"
check "the node-facts pod is the one that reads the host (splitter sanity)" contains "hostPath" "$node_doc"
results="$(cat "$S"/home/gt/.runtime/gke-probe/results-*.txt)"
check "results record the kernel" contains "kernel=6.8.0-1066-gke" "$results"
check "results record the active security modules" contains "LSM=landlock" "$results"
check "ABI 4 is a PASS verdict" contains "VERDICT PASS landlock_abi=4" "$results"
check "the cluster is deleted at the end" contains "container clusters delete gt-probe-" "$CALLS_LOG"
check "no cluster is left in the stub state" test ! -s "$S/state/clusters"

echo "failing abi"
scenario fail
probe '\ny\ny\ny\ny\n' STUB_ABI=-38 --
results="$(cat "$S"/home/gt/.runtime/gke-probe/results-*.txt)"
check "ABI error -38 is a FAIL verdict" contains "VERDICT FAIL landlock_abi=-38" "$results"
check "a failing probe still deletes the cluster" contains "container clusters delete gt-probe-" "$CALLS_LOG"

echo "create fails midway"
scenario cfail
probe '\ny\n' STUB_CREATE_FAIL=1 --
check "a failed create exits non-zero" test "$RC" -ne 0
check "a failed create warns the cluster may still be billing" contains "may still be running and billing" "$OUT"
check "the warning names the teardown command" contains "teardown" "$OUT"
check "a clean run does not warn about a leftover cluster" lacks "may still be running" "$FULL_OUT"

echo "declined cost confirmation"
scenario decline
probe '\nn\n' --
check "declining creates nothing" lacks "clusters create" "$CALLS_LOG"
check "declining exits 0" test "$RC" -eq 0

echo "leftover cluster"
scenario leftover
echo "gt-probe-old us-central1-a" >"$S/state/clusters"
probe '\ny\ny\n' --
check "a leftover probe cluster stops the run" test "$RC" -ne 0
check "a leftover probe cluster blocks a second create" lacks "clusters create" "$CALLS_LOG"

echo "teardown"
scenario td
printf 'gt-probe-a us-central1-a\ngt-probe-b us-east1-b\n' >"$S/state/clusters"
probe '\ny\ny\n' -- teardown
check "teardown lists only labelled clusters" contains "--filter=resourceLabels.gt-purpose=openshell-probe" "$CALLS_LOG"
check "teardown deletes the first labelled cluster" contains "container clusters delete gt-probe-a" "$CALLS_LOG"
check "teardown deletes the second labelled cluster" contains "container clusters delete gt-probe-b" "$CALLS_LOG"
check "teardown never names the unlabelled cluster" lacks "prod" "$CALLS_LOG"
scenario td_no
printf 'gt-probe-a us-central1-a\n' >"$S/state/clusters"
probe '\nn\n' -- teardown
check "declining a teardown deletes nothing" lacks "clusters delete" "$CALLS_LOG"

if [ "$FAIL" -eq 0 ]; then
  echo "probe_test.sh: all $PASS cases passed"
else
  echo "probe_test.sh: $FAIL of $((PASS + FAIL)) cases failed"
  exit 1
fi
