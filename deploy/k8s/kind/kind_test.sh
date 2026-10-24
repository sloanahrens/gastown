#!/usr/bin/env bash
# Tests for deploy/k8s/kind/up.sh and down.sh (gt-s9gqm).
#
# What matters: the town must not be disturbed. The scripts must never leave
# docker's global context pointing at the kind VM (colima start flips it), never
# write ~/.kube or the town's openshell config, never print a key, refuse a
# gateway port someone else is listening on, and be safe to rerun. Stub tools
# on PATH keep the cluster state as files and record every call with the
# environment that matters (DOCKER_HOST, KUBECONFIG, XDG_CONFIG_HOME). Nothing
# here starts a VM or reaches a network.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/k8s-kind-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); if [ $# -gt 1 ]; then printf '%s\n' "$2" | sed 's/^/    | /'; fi; }
check() { local desc=$1; shift; if "$@"; then pass "$desc"; else fail "$desc"; fi; }
contains() { case "$2" in *"$1"*) return 0 ;; *) return 1 ;; esac; }
lacks() { ! contains "$1" "$2"; }
count() { grep -c -- "$1" <<<"$2" || true; }
# GNU stat has -f for the filesystem, BSD stat has -f for the format; the BSD
# probe must send GNU's filesystem dump to /dev/null, not just its error, or
# the fallback output is appended to that dump.
mode() { if stat -f %Lp "$1" >/dev/null 2>&1; then stat -f %Lp "$1"; else stat -c %a "$1"; fi; }

# The scripts run from a copy of the tree: they find values/kind.yaml relative to themselves.
mkdir -p "$TMP/root/deploy/k8s"
cp -R "$SCRIPT_DIR" "$TMP/root/deploy/k8s/kind"
cp -R "$SCRIPT_DIR/../values" "$TMP/root/deploy/k8s/values"
UP="$TMP/root/deploy/k8s/kind/up.sh"
DOWN="$TMP/root/deploy/k8s/kind/down.sh"

mkdir -p "$TMP/bin"
cat >"$TMP/bin/_log" <<'STUB'
#!/usr/bin/env bash
# _log TOOL ARGS...: one line per call with the env the assertions read.
printf '%s %s [DOCKER_HOST=%s KUBECONFIG=%s XDG=%s]\n' "$1" "${*:2}" "${DOCKER_HOST:-}" "${KUBECONFIG:-}" "${XDG_CONFIG_HOME:-}" >>"$CALLS"
STUB
cat >"$TMP/bin/docker" <<'STUB'
#!/usr/bin/env bash
case "$1 $2" in
  "context show") cat "$STATE/ctx"; exit 0 ;;
  "context use") "$(dirname "$0")/_log" docker "$@"; echo "$3" >"$STATE/ctx"; exit 0 ;;
esac
"$(dirname "$0")/_log" docker "$@"
case "$1" in
  inspect) [ -f "$STATE/has-registry" ] ;;
  run) touch "$STATE/has-registry" ;;
  rm) rm -f "$STATE/has-registry" ;;
  exec) if [ "$2" = "-i" ]; then cat >>"$STATE/hosts.toml"; fi ;;
esac
STUB
cat >"$TMP/bin/colima" <<'STUB'
#!/usr/bin/env bash
"$(dirname "$0")/_log" colima "$@"
case "$1" in
  status) [ -f "$STATE/colima-running" ] ;;
  start) touch "$STATE/colima-running"; [ -n "${STUB_CTX_FLIP:-}" ] && echo "$STUB_CTX_FLIP" >"$STATE/ctx"; exit 0 ;;
  stop) rm -f "$STATE/colima-running"; [ -n "${STUB_CTX_FLIP:-}" ] && echo "$STUB_CTX_FLIP" >"$STATE/ctx"; exit 0 ;;
esac
STUB
cat >"$TMP/bin/kind" <<'STUB'
#!/usr/bin/env bash
"$(dirname "$0")/_log" kind "$@"
case "$1 $2" in
  "get clusters") [ -f "$STATE/cluster" ] && echo gt-sbx; exit 0 ;;
  "get nodes") echo gt-sbx-control-plane ;;
  "create cluster") cp "${4:-/dev/null}" "$STATE/kind-config.yaml"; touch "$STATE/cluster" ;;
  "delete cluster") rm -f "$STATE/cluster" ;;
esac
STUB
cat >"$TMP/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
"$(dirname "$0")/_log" kubectl "$@"
case "$*" in
  *"get secret openshell-client-tls"*)
    name=$(sed -n "s/.*\['\([^']*\)'\].*/\1/p" <<<"$*")
    printf 'FAKE-%s-SECRETVALUE' "$name" | base64 ;;
esac
exit 0
STUB
cat >"$TMP/bin/helm" <<'STUB'
#!/usr/bin/env bash
"$(dirname "$0")/_log" helm "$@"
STUB
cat >"$TMP/bin/openssl" <<'STUB'
#!/usr/bin/env bash
"$(dirname "$0")/_log" openssl "$@"
while [ $# -gt 0 ]; do case "$1" in -in) in=$2 ;; -out) out=$2 ;; esac; shift; done
( umask 022; cat "$in" >"$out" ) # real openssl writes with the default umask
STUB
cat >"$TMP/bin/openshell" <<'STUB'
#!/usr/bin/env bash
"$(dirname "$0")/_log" openshell "$@"
case "$*" in
  "gateway list") [ -f "$STATE/gw" ] && echo k8s; exit 0 ;;
  "gateway add"*) touch "$STATE/gw" ;;
  "gateway remove"*) rm -f "$STATE/gw" ;;
  *"gateway info"*)
    n=$(cat "$STATE/info-calls" 2>/dev/null || echo 0)
    echo $((n + 1)) >"$STATE/info-calls"
    [ "$n" -ge "${STUB_INFO_FAILS:-0}" ] && echo "Status: healthy" ;;
esac
STUB
cat >"$TMP/bin/lsof" <<'STUB'
#!/usr/bin/env bash
# Reports a listener only for the port named in $STATE/busy-port.
busy=$(cat "$STATE/busy-port" 2>/dev/null || true)
case "$*" in *":$busy"*) [ -n "$busy" ] && echo "kubectl 52678 someone 8u IPv4 TCP 127.0.0.1:$busy (LISTEN)" ;; esac
exit 0
STUB
cat >"$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
"$(dirname "$0")/_log" curl "$@"
n=$(cat "$STATE/curl-calls" 2>/dev/null || echo 0)
echo $((n + 1)) >"$STATE/curl-calls"
[ "$n" -ge "${STUB_CURL_FAILS:-0}" ]
STUB
chmod +x "$TMP"/bin/*

scenario() {
  S="$TMP/$1"
  rm -rf "$S"
  mkdir -p "$S/state" "$S/home"
  echo desktop-linux >"$S/state/ctx"
  : >"$S/calls.log"
}
run() { # run SCRIPT [ENV=VAL...] -- ARGS...
  local script=$1
  shift
  local envs=()
  while [ $# -gt 0 ] && [ "$1" != "--" ]; do envs+=("$1"); shift; done
  [ $# -gt 0 ] && shift
  set +e
  OUT_TEXT="$(env ${envs[@]+"${envs[@]}"} HOME="$S/home" CALLS="$S/calls.log" STATE="$S/state" \
    GT_K8S_POLL_INTERVAL=0 PATH="$TMP/bin:$PATH" bash "$script" "$@" 2>&1)"
  RC=$?
  set -e
  CALLS_LOG="$(cat "$S/calls.log")"
}

echo "fresh up"
scenario up
run "$UP" --
check "up exits 0" test "$RC" -eq 0
check "colima profile is started" contains "colima start gt-sbx" "$CALLS_LOG"
check "the registry is started on a loopback port" contains "run -d --restart=always -p 127.0.0.1:5001:5000 --name gt-registry registry:2" "$CALLS_LOG"
kind_cfg="$(cat "$S/state/kind-config.yaml")"
check "the kind config maps the gateway nodePort to the Mac loopback port" contains "hostPort: 28443" "$kind_cfg"
check "the kind config maps from nodePort 30443" contains "containerPort: 30443" "$kind_cfg"
check "the kind config listens on 127.0.0.1 only" contains "listenAddress: 127.0.0.1" "$kind_cfg"
check "the kind config enables the containerd registry config_path" contains "config_path" "$kind_cfg"
check "nodes get the registry mirror" contains 'server = "http://gt-registry:5000"' "$(cat "$S/state/hosts.toml")"
check "agent-sandbox is pinned to v1.0.7" contains "agent-sandbox/releases/download/v1.0.7/sandbox.yaml" "$CALLS_LOG"
helm_call="$(grep '^helm ' "$S/calls.log")"
check "the OpenShell chart is pinned to 0.1.4" contains "--version 0.1.4" "$helm_call"
check "the chart is not the broken pre-release" lacks "pre." "$helm_call"
check "the chart is given the kind values file" contains "values/kind.yaml" "$helm_call"
check "the NodePort service is applied" contains "gateway-nodeport.yaml" "$CALLS_LOG"
check "the client key is converted to PKCS8" contains "pkcs8 -topk8 -nocrypt" "$CALLS_LOG"
check "the jsonpath key escapes its dot" contains "['tls\\.key']" "$CALLS_LOG"
check "the gateway is registered as k8s on the fixed port" contains "gateway add https://127.0.0.1:28443 --local --name k8s" "$CALLS_LOG"
mtls="$S/home/gt/.runtime/sbx-k8s/xdg/openshell/gateways/k8s/mtls"
check "the client key is mode 600" test "$(mode "$mtls/tls.key")" = 600
check "no key material is printed" lacks "SECRETVALUE" "$OUT_TEXT"
check "every kind call runs against the kind VM docker socket" test "$(grep '^kind ' "$S/calls.log" | grep -vc "DOCKER_HOST=unix://$S/home/.colima/gt-sbx/docker.sock")" -eq 0
check "kind was called to list clusters" contains "kind get clusters" "$CALLS_LOG"
check "kubectl always uses the private kubeconfig" test "$(grep '^kubectl ' "$S/calls.log" | grep -vc "KUBECONFIG=$S/home/gt/.runtime/sbx-k8s/kubeconfig")" -eq 0
check "openshell always uses the isolated config dir" test "$(grep '^openshell ' "$S/calls.log" | grep -vc "XDG=$S/home/gt/.runtime/sbx-k8s/xdg")" -eq 0
check "nothing is written to ~/.kube" test ! -e "$S/home/.kube"
check "the town's openshell config is untouched" test ! -e "$S/home/.config/openshell"
check "no docker context change when colima left it alone" lacks "docker context use" "$CALLS_LOG"

echo "colima flips the docker context"
scenario flip
run "$UP" STUB_CTX_FLIP=colima-gt-sbx --
check "up exits 0 after a context flip" test "$RC" -eq 0
check "the original context is restored" test "$(cat "$S/state/ctx")" = desktop-linux
check "the only context change is the restore" test "$(count 'docker context use' "$CALLS_LOG")" -eq 1
check "the restore names the original context" contains "context use desktop-linux" "$CALLS_LOG"

echo "rerun is idempotent"
scenario rerun
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw"
run "$UP" --
check "rerun exits 0" test "$RC" -eq 0
check "rerun does not start colima" lacks "colima start" "$CALLS_LOG"
check "rerun does not create a second cluster" lacks "kind create" "$CALLS_LOG"
check "rerun does not start a second registry" lacks "docker run" "$CALLS_LOG"
check "rerun does not register the gateway twice" lacks "gateway add" "$CALLS_LOG"

echo "registry and cluster are repaired independently"
scenario noreg
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/gw"
run "$UP" --
check "a missing registry is started even though the cluster exists" test "$(count 'docker run' "$CALLS_LOG")" -eq 1
check "the existing cluster is still left alone" lacks "kind create" "$CALLS_LOG"
scenario nocluster
touch "$S/state/colima-running" "$S/state/has-registry"
run "$UP" --
check "an existing registry is not started again for a new cluster" lacks "docker run" "$CALLS_LOG"
check "the cluster is still created" contains "kind create cluster" "$CALLS_LOG"

echo "the gateway answers late"
scenario late
run "$UP" STUB_CURL_FAILS=2 --
check "up waits for the gateway and succeeds" test "$RC" -eq 0
check "the gateway was polled until it answered" test "$(count '^curl ' "$CALLS_LOG")" -eq 3
check "the gateway is registered only after it answers" test "$(grep -n '^curl ' "$S/calls.log" | tail -1 | cut -d: -f1)" -lt "$(grep -n 'gateway add' "$S/calls.log" | cut -d: -f1)"
scenario never
run "$UP" STUB_CURL_FAILS=999 GT_K8S_POLL_TRIES=3 --
check "a gateway that never answers fails the run" test "$RC" -ne 0
check "the wait is bounded by the try limit" test "$(count '^curl ' "$CALLS_LOG")" -eq 3
check "the failure names the port" contains "127.0.0.1:28443" "$OUT_TEXT"
check "no gateway registration happens after the failure" lacks "gateway add" "$CALLS_LOG"

echo "the gateway rewrites its certificates after the first export"
scenario certs
run "$UP" STUB_INFO_FAILS=2 --
check "up succeeds after re-exporting" test "$RC" -eq 0
check "the client certificates were exported again for each failed handshake" test "$(count 'get secret openshell-client-tls' "$CALLS_LOG")" -eq 9
scenario badcerts
run "$UP" STUB_INFO_FAILS=999 GT_K8S_CERT_TRIES=3 --
check "a gateway that never turns healthy fails the run" test "$RC" -ne 0
check "the retry is bounded by the try limit" test "$(count 'gateway info' "$CALLS_LOG")" -eq 3
check "the failure says the gateway is not healthy" contains "not healthy" "$OUT_TEXT"

echo "someone else owns the gateway port"
scenario busy
echo 28443 >"$S/state/busy-port"
touch "$S/state/colima-running"
run "$UP" --
check "a busy gateway port stops the run" test "$RC" -ne 0
check "the message names the port" contains "28443" "$OUT_TEXT"
check "nothing is created on a busy port" lacks "kind create" "$CALLS_LOG"
check "the other process is not signalled" lacks "kill" "$CALLS_LOG"
scenario busy_env
echo 28999 >"$S/state/busy-port"
touch "$S/state/colima-running"
run "$UP" GT_K8S_GATEWAY_PORT=29001 --
check "GT_K8S_GATEWAY_PORT moves the mapping to a free port" contains "hostPort: 29001" "$(cat "$S/state/kind-config.yaml")"

echo "down"
scenario down
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw"
run "$DOWN" --
check "down exits 0" test "$RC" -eq 0
check "down deletes the kind cluster" contains "kind delete cluster --name gt-sbx" "$CALLS_LOG"
check "down removes the registry" contains "rm -f gt-registry" "$CALLS_LOG"
check "down removes the gateway registration" contains "gateway remove k8s" "$CALLS_LOG"
check "down leaves the VM running by default" lacks "colima stop" "$CALLS_LOG"
scenario down_vm
touch "$S/state/colima-running" "$S/state/cluster"
run "$DOWN" STUB_CTX_FLIP=colima-gt-sbx -- --stop-vm
check "--stop-vm stops the colima profile" contains "colima stop -p gt-sbx" "$CALLS_LOG"
check "--stop-vm restores a flipped context" test "$(cat "$S/state/ctx")" = desktop-linux

if [ "$FAIL" -eq 0 ]; then
  echo "kind_test.sh: all $PASS cases passed"
else
  echo "kind_test.sh: $FAIL of $((PASS + FAIL)) cases failed"
  exit 1
fi
