#!/usr/bin/env bash
# Tests for deploy/k8s/kind/up.sh and down.sh (gt-s9gqm).
#
# What matters: the town must not be disturbed. The scripts must never leave
# docker's global context pointing at the kind VM (colima start flips it, and a
# failed start must still restore it), never write ~/.kube or the town's
# openshell config, never print a key, refuse a gateway port someone else is
# listening on or cannot be probed, and refuse a changed remote manifest. Stub
# tools on PATH keep the cluster state as files and record every call with the
# environment that matters (DOCKER_HOST, KUBECONFIG, XDG_CONFIG_HOME, umask).
# Nothing here starts a VM or reaches a network.
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
occurrences() { { grep -o -- "$1" <<<"$2" || true; } | wc -l | tr -d ' '; }
# GNU stat has -f for the filesystem, BSD stat has -f for the format; the BSD
# probe must send GNU's filesystem dump to /dev/null, not just its error, or
# the fallback output is appended to that dump.
mode() { if stat -f %Lp "$1" >/dev/null 2>&1; then stat -f %Lp "$1"; else stat -c %a "$1"; fi; }
digest() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
line_of() { grep -n -- "$1" "$S/calls.log" | head -1 | cut -d: -f1; }
before() { # before A B: A's first call precedes B's first call
  local a b
  a="$(line_of "$1")"
  b="$(line_of "$2")"
  [ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ]
}
no_stage() { ! ls -A "$1" | grep -q '^\.stage\.'; }

# The scripts run from a copy of the tree: they find values/kind.yaml relative to themselves.
mkdir -p "$TMP/root/deploy/k8s"
cp -R "$SCRIPT_DIR" "$TMP/root/deploy/k8s/kind"
cp -R "$SCRIPT_DIR/../values" "$TMP/root/deploy/k8s/values"
cp -R "$SCRIPT_DIR/../services" "$TMP/root/deploy/k8s/services"
cp "$SCRIPT_DIR/../priority-classes.yaml" "$TMP/root/deploy/k8s/priority-classes.yaml"
UP="$TMP/root/deploy/k8s/kind/up.sh"
DOWN="$TMP/root/deploy/k8s/kind/down.sh"

mkdir -p "$TMP/bin"
cat >"$TMP/bin/_log" <<'STUB'
#!/usr/bin/env bash
# _log TOOL ARGS...: one line per call with the env the assertions read.
printf '%s %s [DOCKER_HOST=%s KUBECONFIG=%s XDG=%s UMASK=%s]\n' "$1" "${*:2}" "${DOCKER_HOST:-}" "${KUBECONFIG:-}" "${XDG_CONFIG_HOME:-}" "$(umask)" >>"$CALLS"
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
  start)
    [ -n "${STUB_CTX_FLIP:-}" ] && echo "$STUB_CTX_FLIP" >"$STATE/ctx"
    [ -n "${STUB_COLIMA_FAIL:-}" ] && exit 1
    touch "$STATE/colima-running"; exit 0 ;;
  stop)
    [ -n "${STUB_CTX_FLIP:-}" ] && echo "$STUB_CTX_FLIP" >"$STATE/ctx"
    rm -f "$STATE/colima-running"; exit 0 ;;
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
METRICS_DEFAULT_ARGS='--cert-dir=/tmp --secure-port=10250 --kubelet-preferred-address-types=InternalIP,ExternalIP,Hostname --kubelet-use-node-status-port --metric-resolution=15s'
# The args the deployment carries: the manifest's, plus each patch applied since.
# They persist across applies, so a rerun exercises the script's flag guard.
metrics_args() { cat "$STATE/metrics-args" 2>/dev/null || printf '%s' "$METRICS_DEFAULT_ARGS"; }
case "$*" in
  *"get nodes -l gt.dev/pool"*)
    if [ -f "$STATE/kubectl-down" ]; then echo "The connection to the server 127.0.0.1:1 was refused" >&2; exit 1; fi
    [ -f "$STATE/no-layout" ] || printf 'node/core-node\nnode/sandbox-node\n' ;;
  *"get secret openshell-client-tls"*)
    name=$(sed -n "s/.*\['\([^']*\)'\].*/\1/p" <<<"$*")
    # STUB_SECRET_ROTATES: every read of the CA sees a newer certificate, as
    # when the chart's certificate job rewrites the secret between two exports.
    # Every other read keeps the fixed value the other scenarios pin.
    bare=${name//\\/}
    if [ -n "${STUB_SECRET_ROTATES:-}" ] && [ "$bare" = ca.crt ]; then
      n=$(cat "$STATE/secret-gen" 2>/dev/null || echo 0)
      n=$((n + 1))
      echo "$n" >"$STATE/secret-gen"
      printf '%s' "FAKE-ca.crt-SECRETVALUE-gen$n" >"$STATE/last-ca"
      printf '%s' "FAKE-ca.crt-SECRETVALUE-gen$n" | base64
      exit 0
    fi
    printf 'FAKE-%s-SECRETVALUE' "$name" | base64 ;;
  *"get configmap gt-bootstrap"*) [ -f "$STATE/marker" ] && cat "$STATE/marker" ;;
  *"create configmap gt-bootstrap"*)
    for a in "$@"; do case "$a" in --from-literal=helm=*) echo "${a#--from-literal=}" ;; esac; done ;;
  *"apply -f -"*)
    while read -r line; do case "$line" in helm=*) printf '%s' "${line#helm=}" >"$STATE/marker" ;; esac; done ;;
  *"get deployment metrics-server"*)
    # STUB_METRICS_WARNING: a successful read that still warns on stderr, as
    # kubectl does; the warning follows the value so a read that merges the
    # streams no longer ends with the flag, and the check must see stdout only.
    metrics_args
    [ -n "${STUB_METRICS_WARNING:-}" ] && echo "Warning: unused parameter" >&2 ;;
  *"patch deployment metrics-server"*)
    case "$*" in
      *'"value":"--kubelet-insecure-tls"'*)
        printf '%s --kubelet-insecure-tls' "$(metrics_args)" >"$STATE/metrics-args" ;;
    esac ;;
esac
exit 0
STUB
cat >"$TMP/bin/helm" <<'STUB'
#!/usr/bin/env bash
"$(dirname "$0")/_log" helm "$@"
case "$1" in
  upgrade) touch "$STATE/helm-release" ;;
  # STUB_HELM_FAILED: a release that exists but is not deployed (a failed or
  # pending upgrade); `helm list --deployed` omits it, as the CLI does.
  list) [ -f "$STATE/helm-release" ] && [ -z "${STUB_HELM_FAILED:-}" ] && echo openshell ;;
esac
STUB
cat >"$TMP/bin/openssl" <<'STUB'
#!/usr/bin/env bash
"$(dirname "$0")/_log" openssl "$@"
while [ $# -gt 0 ]; do case "$1" in -in) in=$2 ;; -out) out=$2 ;; esac; shift; done
( umask 022; cat "$in" >"$out" ) # a tool that ignores the caller's umask
STUB
cat >"$TMP/bin/openshell" <<'STUB'
#!/usr/bin/env bash
"$(dirname "$0")/_log" openshell "$@"
# The town config is wherever the daemon looks (${XDG_CONFIG_HOME:-$HOME/.config});
# the script's private config is any other XDG directory, kept in $STATE.
town=0
[ "${XDG_CONFIG_HOME:-$HOME/.config}" = "$HOME/.config" ] && town=1
town_active() { echo "$1" >"$HOME/.config/openshell/active_gateway"; echo "$1" >"$STATE/town-active"; }
# The flags are scanned, not matched in a fixed order: the test must not
# depend on how the script spells its arguments.
name=""
prev=""
for a in "$@"; do
  [ "$prev" = "--name" ] && name=$a
  prev=$a
done
case "$*" in
  "gateway list")
    if [ "$town" = 1 ]; then
      calls=$(cat "$STATE/town-list-calls" 2>/dev/null || echo 0)
      echo $((calls + 1)) >"$STATE/town-list-calls"
      # STUB_TOWN_LIST_FAILS=N fails the list from the Nth town call on, as a
      # flaky CLI would. The earlier calls still answer, so the registration
      # probe and the active-gateway probe can be failed separately.
      if [ -n "${STUB_TOWN_LIST_FAILS:-}" ] && [ "$((calls + 1))" -ge "$STUB_TOWN_LIST_FAILS" ]; then
        echo "Error: cannot read $HOME/.config/openshell" >&2
        exit 1
      fi
      # A header row and three columns, as the real list prints: the parser
      # must find the star and the name without relying on the row's width.
      echo "NAME URL STATUS"
      while read -r n; do
        [ -n "$n" ] || continue
        if [ "$n" = "$(cat "$STATE/town-active" 2>/dev/null)" ]; then
          echo "* $n https://127.0.0.1:28443 healthy"
        else
          echo "  $n https://127.0.0.1:28443 healthy"
        fi
      done <"$STATE/town-gw-list" 2>/dev/null
    else
      [ -f "$STATE/gw-list" ] && cat "$STATE/gw-list"
      [ -f "$STATE/gw" ] && echo k8s
    fi
    exit 0 ;;
  "gateway add"*)
    if [ "$town" = 1 ]; then
      n=$(cat "$STATE/town-add-calls" 2>/dev/null || echo 0)
      echo $((n + 1)) >"$STATE/town-add-calls"
      # STUB_TOWN_ADD_FAIL_ONCE fails the first add only, so the rollback's
      # retry can be told apart from the registration that failed.
      [ -n "${STUB_TOWN_ADD_FAIL_ONCE:-}" ] && [ "$n" -eq 0 ] && exit 1
      mkdir -p "$HOME/.config/openshell"
      grep -qx "$name" "$STATE/town-gw-list" 2>/dev/null || echo "$name" >>"$STATE/town-gw-list"
      town_active "$name" # add makes the new gateway active
    else
      touch "$STATE/gw"
    fi
    exit 0 ;;
  "gateway select"*)
    if [ "$town" = 1 ]; then
      [ -n "${STUB_SELECT_FAIL:-}" ] && exit 1
      town_active "$3"
    fi
    exit 0 ;;
  "gateway remove"*)
    if [ "$town" = 1 ]; then
      grep -vx "$3" "$STATE/town-gw-list" >"$STATE/town-gw-list.tmp" 2>/dev/null || true
      mv "$STATE/town-gw-list.tmp" "$STATE/town-gw-list" 2>/dev/null || true
      # STUB_TOWN_REMOVE_CLEARS_ACTIVE drops the selection whichever gateway was removed.
      [ -n "${STUB_TOWN_REMOVE_CLEARS_ACTIVE:-}" ] && rm -f "$STATE/town-active" "$HOME/.config/openshell/active_gateway"
      [ "$(cat "$STATE/town-active" 2>/dev/null)" = "$3" ] && rm -f "$STATE/town-active"
      [ "$(cat "$HOME/.config/openshell/active_gateway" 2>/dev/null)" = "$3" ] && rm -f "$HOME/.config/openshell/active_gateway"
      # STUB_TOWN_REMOVE_SELECTS names a gateway the CLI selects when it removes another.
      [ -z "${STUB_TOWN_REMOVE_SELECTS:-}" ] || town_active "$STUB_TOWN_REMOVE_SELECTS"
    else
      rm -f "$STATE/gw"
    fi
    exit 0 ;;
  *"gateway info"*)
    if [ "$town" = 1 ] && [ -n "${STUB_TOWN_INFO_FAILS:-}" ]; then
      tn=$(cat "$STATE/town-info-calls" 2>/dev/null || echo 0)
      echo $((tn + 1)) >"$STATE/town-info-calls"
      if [ "$tn" -lt "$STUB_TOWN_INFO_FAILS" ]; then echo "Error: invalid peer certificate: BadSignature"; exit 0; fi
      echo "Status: healthy"; exit 0
    fi
    n=$(cat "$STATE/info-calls" 2>/dev/null || echo 0)
    echo $((n + 1)) >"$STATE/info-calls"
    # STUB_INFO_DECOY: a line that says healthy while the status does not, as a
    # CLI message about one check can; only the status may decide.
    if [ -n "${STUB_INFO_UNHEALTHY:-}" ]; then
      echo "Status: unhealthy"
      [ -n "${STUB_INFO_DECOY:-}" ] && echo "Last check: the client certificate is healthy"
      exit 0
    fi
    [ "$n" -ge "${STUB_INFO_FAILS:-0}" ] && echo "Status: healthy"
    exit 0 ;;
esac
STUB
cat >"$TMP/bin/lsof" <<'STUB'
#!/usr/bin/env bash
# Reports a listener only for the port named in $STATE/busy-port; exits 2 when
# the probe itself is broken (STUB_LSOF_FAIL).
[ -n "${STUB_LSOF_FAIL:-}" ] && exit 2
busy=$(cat "$STATE/busy-port" 2>/dev/null || true)
case "$*" in *":$busy"*) [ -n "$busy" ] && echo "kubectl 52678 someone 8u IPv4 TCP 127.0.0.1:$busy (LISTEN)" ;; esac
exit 0
STUB
cat >"$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
"$(dirname "$0")/_log" curl "$@"
out=""
for ((i = 1; i <= $#; i++)); do
  if [ "${!i}" = "-o" ]; then j=$((i + 1)); out=${!j}; fi
done
if [ -n "$out" ] && [ "$out" != /dev/null ]; then
  printf 'kind: List\n' >"$out" # a fetched manifest
  exit 0
fi
# Sandbox and gateway polls answer only after STUB_CURL_FAILS calls.
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
check "colima profile is started with its resources" contains "colima start gt-sbx --activate=false --cpu 12 --memory 24 --disk 60 --vm-type vz --runtime docker" "$CALLS_LOG"
check "the registry is started on a loopback port" contains "run -d --restart=always -p 127.0.0.1:5001:5000 -v gt-registry-data:/var/lib/registry -e REGISTRY_STORAGE_DELETE_ENABLED=true --name gt-registry registry:2" "$CALLS_LOG"
check "the registry exists before the cluster" before "docker run -d" "kind create cluster"
kind_cfg="$(cat "$S/state/kind-config.yaml")"
check "the kind config maps the gateway nodePort to the Mac loopback port" contains "hostPort: 28443" "$kind_cfg"
check "the kind config maps from nodePort 30443" contains "containerPort: 30443" "$kind_cfg"
check "the kind config listens on 127.0.0.1 only" contains "listenAddress: 127.0.0.1" "$kind_cfg"
check "the kind config enables the containerd registry config_path" contains 'config_path = "/etc/containerd/certs.d"' "$kind_cfg"
check "nodes get the registry mirror" contains 'server = "http://gt-registry:5000"' "$(cat "$S/state/hosts.toml")"
check "the cluster exists before the chart is installed" before "kind create cluster" "helm upgrade --install"
check "the agent-sandbox manifest is downloaded from the pinned release" contains "agent-sandbox/releases/download/v1.0.7/sandbox.yaml" "$(grep '^curl ' "$S/calls.log")"
check "the agent-sandbox manifest is applied from the local verified copy" contains "kubectl apply -f $S/home/gt/.runtime/sbx-k8s/agent-sandbox-v1.0.7.yaml" "$CALLS_LOG"
manifest="$S/home/gt/.runtime/sbx-k8s/agent-sandbox-v1.0.7.yaml"
check "the first fetch records the manifest digest" test "$(cat "$manifest.sha256")" = "$(digest "$manifest")"
helm_call="$(grep '^helm upgrade' "$S/calls.log")"
check "the OpenShell chart is pinned to 0.1.4" contains "--version 0.1.4" "$helm_call"
check "the chart is not the broken pre-release" lacks "pre." "$helm_call"
check "the chart is given the kind values file" contains "values/kind.yaml" "$helm_call"
check "the NodePort service is applied" contains "gateway-nodeport.yaml" "$CALLS_LOG"
check "colima is started without activating its docker context" contains "colima start gt-sbx --activate=false" "$CALLS_LOG"
check "the NodePort service follows the chart install" before "helm upgrade --install" "gateway-nodeport.yaml"
check "the kind config has a control plane and two workers" test "$(grep -c '^- role:' "$S/state/kind-config.yaml")" -eq 3
check "the kind config has exactly two workers" test "$(grep -c '^- role: worker' "$S/state/kind-config.yaml")" -eq 2
check "the core worker is labelled gt.dev/pool core" test "$(grep -cx '    gt.dev/pool: core' "$S/state/kind-config.yaml")" -eq 1
check "the sandbox worker is labelled gt.dev/pool sandbox" test "$(grep -cx '    gt.dev/pool: sandbox' "$S/state/kind-config.yaml")" -eq 1
check "the sandbox worker is tainted NoSchedule at registration" contains "effect: NoSchedule" "$(cat "$S/state/kind-config.yaml")"
check "the taint is on the pool key with value sandbox" contains "      - key: gt.dev/pool
        value: sandbox" "$(cat "$S/state/kind-config.yaml")"
check "only one taint is declared" test "$(grep -c 'effect:' "$S/state/kind-config.yaml")" -eq 1
check "the gateway port mapping stays on the control-plane node" test "$(awk '/^- role: control-plane/{f=1;next} /^- role:/{f=0} f&&/hostPort/{c++} END{print c+0}' "$S/state/kind-config.yaml")" -eq 1
check "the priority classes are applied" contains "priority-classes.yaml" "$CALLS_LOG"
check "the priority classes are applied before the chart" before "priority-classes.yaml" "helm upgrade --install"
check "the metrics-server manifest is downloaded from the pinned release" contains "metrics-server/releases/download/v0.9.0/components.yaml" "$(grep '^curl ' "$S/calls.log")"
metrics_manifest="$S/home/gt/.runtime/sbx-k8s/metrics-server-v0.9.0.yaml"
check "the metrics-server manifest is applied from the local verified copy" contains "kubectl apply -f $metrics_manifest" "$CALLS_LOG"
check "the first fetch records the metrics-server digest" test "$(cat "$metrics_manifest.sha256")" = "$(digest "$metrics_manifest")"
check "metrics-server is installed after the cluster exists" before "kind create cluster" "kubectl apply -f $metrics_manifest"
check "the insecure kubelet flag is patched in once" test "$(count 'patch deployment metrics-server' "$CALLS_LOG")" -eq 1
check "the flag patch appends to the container args as a json op" contains '"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"' "$CALLS_LOG"
check "the flag is patched after the manifest is applied" before "apply -f $metrics_manifest" "patch deployment metrics-server"
check "the metrics-server rollout follows the flag patch" before "patch deployment metrics-server" "rollout status deployment/metrics-server"
patch_args='-n agent-sandbox-system patch deployment agent-sandbox-controller --type merge -p {"spec":{"template":{"spec":{"nodeSelector":{"gt.dev/pool":"core"}}}}}'
check "the agent-sandbox controller deployment is patched once with exactly the core nodeSelector" test "$(grep -cF -e "$patch_args" "$S/calls.log" || true)" -eq 1
check "the controller is pinned before its rollout is awaited" before "patch deployment agent-sandbox-controller" "rollout status deployment/agent-sandbox-controller"
check "the pool layout is checked with an exact label query" contains "get nodes -l gt.dev/pool" "$CALLS_LOG"
check "the control-plane Services are applied" contains "services/control-plane.yaml" "$CALLS_LOG"
check "the control-plane Services follow the NodePort service" before "gateway-nodeport.yaml" "services/control-plane.yaml"
check "the control-plane Services are applied before the gateway is registered" before "services/control-plane.yaml" "gateway add"
check "the client key is converted to PKCS8" contains "pkcs8 -topk8 -nocrypt" "$CALLS_LOG"
check "the jsonpath key escapes its dot" contains "['tls\\.key']" "$CALLS_LOG"
check "the certificates are exported before the gateway is registered" before "get secret openshell-client-tls" "gateway add"
check "the gateway is registered as k8s on the fixed port" contains "gateway add https://127.0.0.1:28443 --local --name k8s" "$CALLS_LOG"
mtls="$S/home/gt/.runtime/sbx-k8s/xdg/openshell/gateways/k8s/mtls"
check "the certificates are written under umask 077" contains "UMASK=0077" "$(grep '^openssl ' "$S/calls.log")"
check "the mtls directory is private" test "$(mode "$mtls")" = 700
check "the client key is mode 600" test "$(mode "$mtls/tls.key")" = 600
check "the CA certificate is mode 600" test "$(mode "$mtls/ca.crt")" = 600
check "the client certificate is mode 600" test "$(mode "$mtls/tls.crt")" = 600
check "the certificate staging directory is gone" no_stage "$mtls"
check "no key material is printed" lacks "SECRETVALUE" "$OUT_TEXT"
check "every kind call runs against the kind VM docker socket" test "$(grep '^kind ' "$S/calls.log" | grep -vc "DOCKER_HOST=unix://$S/home/.colima/gt-sbx/docker.sock")" -eq 0
check "kind was called to list clusters" contains "kind get clusters" "$CALLS_LOG"
check "kubectl always uses the private kubeconfig" test "$(grep '^kubectl ' "$S/calls.log" | grep -vc "KUBECONFIG=$S/home/gt/.runtime/sbx-k8s/kubeconfig")" -eq 0
check "openshell always uses the isolated config dir" test "$(grep '^openshell ' "$S/calls.log" | grep -vc "XDG=$S/home/gt/.runtime/sbx-k8s/xdg")" -eq 0
check "nothing is written to ~/.kube" test ! -e "$S/home/.kube"
check "the town's openshell config is untouched" test ! -e "$S/home/.config/openshell"
check "no openshell call reads or writes the town config" test "$(grep -c "XDG=$S/home/.config " "$S/calls.log")" -eq 0
check "no docker context change when colima left it alone" lacks "docker context use" "$CALLS_LOG"

echo "colima flips the docker context"
scenario flip
run "$UP" STUB_CTX_FLIP=colima-gt-sbx --
check "up exits 0 after a context flip" test "$RC" -eq 0
check "the original context is restored" test "$(cat "$S/state/ctx")" = desktop-linux
check "the only context change is the restore" test "$(count 'docker context use' "$CALLS_LOG")" -eq 1
check "the restore names the original context" contains "context use desktop-linux" "$CALLS_LOG"

echo "colima fails after flipping the docker context"
scenario colima_fail
run "$UP" STUB_CTX_FLIP=colima-gt-sbx STUB_COLIMA_FAIL=1 --
check "a failed colima start fails the run" test "$RC" -ne 0
check "the failure names the profile" contains "colima start gt-sbx failed" "$OUT_TEXT"
check "the flipped context is still restored" test "$(cat "$S/state/ctx")" = desktop-linux
check "the failed start creates nothing" lacks "kind create" "$CALLS_LOG"
check "the failed start creates no registry" lacks "docker run" "$CALLS_LOG"

echo "rerun is idempotent"
scenario rerun
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw"
run "$UP" --
check "rerun exits 0" test "$RC" -eq 0
check "rerun does not start colima" lacks "colima start" "$CALLS_LOG"
check "rerun does not create a second cluster" lacks "kind create" "$CALLS_LOG"
check "rerun does not start a second registry" lacks "docker run" "$CALLS_LOG"
check "rerun does not register the gateway twice" lacks "gateway add" "$CALLS_LOG"

echo "a similar gateway name does not count as registered"
scenario lookalike
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry"
printf 'NAME STATUS\nk8s-other healthy\n' >"$S/state/gw-list"
run "$UP" --
check "the lookalike name does not stop registration of k8s" contains "gateway add https://127.0.0.1:28443 --local --name k8s" "$CALLS_LOG"

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
check "the gateway was polled until it answered" test "$(count '^curl .* -o /dev/null' "$CALLS_LOG")" -eq 3
check "the gateway is registered only after it answers" test "$(grep -n '^curl .* -o /dev/null' "$S/calls.log" | tail -1 | cut -d: -f1)" -lt "$(grep -n 'gateway add' "$S/calls.log" | cut -d: -f1)"
scenario never
run "$UP" STUB_CURL_FAILS=999 GT_K8S_POLL_TRIES=3 --
check "a gateway that never answers fails the run" test "$RC" -ne 0
check "the wait is bounded by the try limit" test "$(count '^curl .* -o /dev/null' "$CALLS_LOG")" -eq 3
check "the failure names the port" contains "127.0.0.1:28443" "$OUT_TEXT"
check "no gateway registration happens after the failure" lacks "gateway add" "$CALLS_LOG"

echo "the gateway rewrites its certificates after the first export"
scenario certs
run "$UP" STUB_INFO_FAILS=2 --
check "up succeeds after re-exporting" test "$RC" -eq 0
check "the client certificates were exported again for each failed handshake" test "$(count 'get secret openshell-client-tls' "$CALLS_LOG")" -eq 9
check "the CA was read through the escaped jsonpath key" test "$(cat "$S/home/gt/.runtime/sbx-k8s/xdg/openshell/gateways/k8s/mtls/ca.crt")" = 'FAKE-ca\.crt-SECRETVALUE'
scenario badcerts
run "$UP" STUB_INFO_FAILS=999 GT_K8S_CERT_TRIES=3 --
check "a gateway that never turns healthy fails the run" test "$RC" -ne 0
check "the retry is bounded by the try limit" test "$(count 'gateway info' "$CALLS_LOG")" -eq 3
check "the failure says the gateway is not healthy" contains "not healthy" "$OUT_TEXT"

echo "unhealthy is not healthy"
scenario unhealthy
run "$UP" STUB_INFO_UNHEALTHY=1 GT_K8S_CERT_TRIES=3 --
check "a gateway reporting unhealthy fails the run" test "$RC" -ne 0
check "the run still says the gateway is not healthy" contains "not healthy" "$OUT_TEXT"
scenario decoyhealthy
run "$UP" STUB_INFO_UNHEALTHY=1 STUB_INFO_DECOY=1 GT_K8S_CERT_TRIES=2 --
check "a healthy word in a message line does not make the gateway healthy" test "$RC" -ne 0
check "the decoy gateway is retried only up to the try limit" test "$(count 'gateway info' "$CALLS_LOG")" -eq 2

echo "an existing cluster without the three-node layout"
scenario oldlayout
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw" "$S/state/no-layout"
run "$UP" --
check "a cluster without the pools is refused" test "$RC" -ne 0
check "the message says how to rebuild it" contains "down.sh" "$OUT_TEXT"
check "nothing is installed into the half-configured cluster" lacks "helm upgrade --install" "$CALLS_LOG"
check "the priority classes are not applied to it either" lacks "priority-classes.yaml" "$CALLS_LOG"
check "a missing layout is reported as missing, not as a kubectl error" lacks "cannot list the nodes" "$OUT_TEXT"
scenario kubectldown
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw" "$S/state/kubectl-down"
run "$UP" --
check "a failing node query fails the run" test "$RC" -ne 0
check "the failure names the kubectl error, not a missing layout" contains "cannot list the nodes" "$OUT_TEXT"
check "the kubectl error text is shown" contains "connection to the server" "$OUT_TEXT"
check "a failing node query does not claim the layout is missing" lacks "lacks the core and sandbox nodes" "$OUT_TEXT"
check "nothing is installed after a failing node query" lacks "helm upgrade --install" "$CALLS_LOG"

echo "helm only runs when the chart or its values changed (every helm upgrade regenerates the certificates)"
VALUES_SHA="$(shasum -a 256 "$TMP/root/deploy/k8s/values/kind.yaml" | cut -d' ' -f1)"
helm_calls() { grep -c '^helm upgrade' "$S/calls.log" || true; }
scenario helmfresh
run "$UP" --
check "a fresh cluster runs helm once" test "$(helm_calls)" -eq 1
check "the marker records the chart version and the values digest" test "$(cat "$S/state/marker")" = "0.1.4:$VALUES_SHA"
check "the marker is written through a configmap named gt-bootstrap" contains "create configmap gt-bootstrap" "$CALLS_LOG"
scenario helmskip
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw" "$S/state/helm-release"
printf '%s' "0.1.4:$VALUES_SHA" >"$S/state/marker"
run "$UP" --
check "an unchanged chart and values make no helm call" test "$(helm_calls)" -eq 0
check "the skip says why" contains "not running helm" "$OUT_TEXT"
check "the skipped run still succeeds and reaches a healthy gateway" test "$RC" -eq 0
check "the marker is left as it was" test "$(cat "$S/state/marker")" = "0.1.4:$VALUES_SHA"
check "the manifests are still applied on a skipped run" contains "gateway-nodeport.yaml" "$CALLS_LOG"
scenario helmvalues
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw" "$S/state/helm-release"
printf '%s' "0.1.4:0000000000000000000000000000000000000000000000000000000000000000" >"$S/state/marker"
run "$UP" --
check "a changed values file runs helm" test "$(helm_calls)" -eq 1
check "the marker is updated to the new digest" test "$(cat "$S/state/marker")" = "0.1.4:$VALUES_SHA"
scenario helmchart
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw" "$S/state/helm-release"
printf '%s' "0.1.3:$VALUES_SHA" >"$S/state/marker"
run "$UP" --
check "a changed chart version runs helm" test "$(helm_calls)" -eq 1
check "the marker is updated to the new chart version" test "$(cat "$S/state/marker")" = "0.1.4:$VALUES_SHA"
scenario helmforce
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw" "$S/state/helm-release"
printf '%s' "0.1.4:$VALUES_SHA" >"$S/state/marker"
run "$UP" GT_K8S_FORCE_HELM=1 --
check "GT_K8S_FORCE_HELM runs helm despite an unchanged marker" test "$(helm_calls)" -eq 1
scenario helmgone
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw"
printf '%s' "0.1.4:$VALUES_SHA" >"$S/state/marker"
run "$UP" --
check "a marker without a deployed release runs helm" test "$(helm_calls)" -eq 1
scenario helmfailed
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw" "$S/state/helm-release"
printf '%s' "0.1.4:$VALUES_SHA" >"$S/state/marker"
run "$UP" STUB_HELM_FAILED=1 --
check "a release that exists but is not deployed runs helm" test "$(helm_calls)" -eq 1
scenario helmtown
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw" "$S/state/helm-release"
printf '%s' "0.1.4:$VALUES_SHA" >"$S/state/marker"
mkdir -p "$S/home/.config/openshell"
printf 'docker\n' >"$S/state/town-gw-list"
printf 'docker\n' >"$S/state/town-active"
run "$UP" -- --register-town
check "register-town on an unchanged chart makes no helm call" test "$(helm_calls)" -eq 0
check "register-town on an unchanged chart succeeds" test "$RC" -eq 0

echo "the town copy of the certificates is verified under the town config"
scenario towncerts
mkdir -p "$S/home/.config/openshell"
printf 'docker\n' >"$S/state/town-gw-list"
printf 'docker\n' >"$S/state/town-active"
run "$UP" STUB_SECRET_ROTATES=1 STUB_TOWN_INFO_FAILS=2 -- --register-town
check "register-town succeeds once the town copy turns healthy" test "$RC" -eq 0
check "the town gateway info was asked three times (two failures, then healthy)" test "$(cat "$S/state/town-info-calls")" -eq 3
check "the town ca.crt equals the LAST certificate the cluster served" test "$(cat "$S/home/.config/openshell/gateways/k8s/mtls/ca.crt")" = "$(cat "$S/state/last-ca")"
check "the town copy was re-exported between the tries" contains "town copy not healthy yet" "$OUT_TEXT"
check "each failed try re-read all three client certificates (6 private+town reads, then 2 tries x 3)" test "$(count 'get secret openshell-client-tls' "$CALLS_LOG")" -eq 12
check "the active gateway is still the one from before" test "$(cat "$S/home/.config/openshell/active_gateway")" = docker
scenario townnever
mkdir -p "$S/home/.config/openshell"
printf 'docker\n' >"$S/state/town-gw-list"
printf 'docker\n' >"$S/state/town-active"
run "$UP" STUB_TOWN_INFO_FAILS=999 GT_K8S_CERT_TRIES=3 -- --register-town
check "a town copy that never turns healthy fails the run" test "$RC" -ne 0
check "the failure names the town config and the last error" contains "BadSignature" "$OUT_TEXT"
check "the retries are bounded by the try limit" test "$(cat "$S/state/town-info-calls")" -eq 3
check "the failed registration leaves the active gateway as it was" test "$(cat "$S/home/.config/openshell/active_gateway")" = docker
check "the failed registration leaves no k8s entry in the town list" test "$(grep -cx k8s "$S/state/town-gw-list" || true)" -eq 0

echo "someone else owns the gateway port"
scenario busy
echo 28443 >"$S/state/busy-port"
touch "$S/state/colima-running"
run "$UP" --
check "a busy gateway port stops the run" test "$RC" -ne 0
check "the message names the port" contains "28443" "$OUT_TEXT"
check "nothing is created on a busy port" lacks "kind create" "$CALLS_LOG"
check "no kind config is written for a busy port" test ! -e "$S/state/kind-config.yaml"
scenario busy_env
echo 28999 >"$S/state/busy-port"
touch "$S/state/colima-running"
run "$UP" GT_K8S_GATEWAY_PORT=29001 --
check "GT_K8S_GATEWAY_PORT moves the mapping to a free port" contains "hostPort: 29001" "$(cat "$S/state/kind-config.yaml")"

echo "the port probe cannot run"
scenario probe_error
touch "$S/state/colima-running"
run "$UP" STUB_LSOF_FAIL=1 --
check "an unusable port probe fails the run" test "$RC" -ne 0
check "the failure names the probe" contains "cannot probe" "$OUT_TEXT"
check "nothing is created when the probe fails" lacks "docker run" "$CALLS_LOG"
check "no cluster is created when the probe fails" lacks "kind create" "$CALLS_LOG"

echo "the agent-sandbox manifest does not match its pin"
scenario digest
mkdir -p "$S/home/gt/.runtime/sbx-k8s"
echo "0000000000000000000000000000000000000000000000000000000000000000" >"$S/home/gt/.runtime/sbx-k8s/agent-sandbox-v1.0.7.yaml.sha256"
run "$UP" --
check "a changed agent-sandbox manifest stops the run" test "$RC" -ne 0
check "the failure names both digests" contains "refusing to apply it" "$OUT_TEXT"
check "the unverified manifest is not applied" lacks "kubectl apply -f $S/home/gt/.runtime/sbx-k8s/agent-sandbox-v1.0.7.yaml" "$CALLS_LOG"

echo "the metrics-server manifest does not match its pin"
scenario metrics_digest
mkdir -p "$S/home/gt/.runtime/sbx-k8s"
echo "0000000000000000000000000000000000000000000000000000000000000000" >"$S/home/gt/.runtime/sbx-k8s/metrics-server-v0.9.0.yaml.sha256"
run "$UP" --
check "a changed metrics-server manifest stops the run" test "$RC" -ne 0
check "the failure names metrics-server and the version" contains "metrics-server v0.9.0 is" "$OUT_TEXT"
check "the failure refuses to apply it and names both digests" contains "refusing to apply it" "$OUT_TEXT"
check "the unverified metrics-server manifest is not applied" lacks "kubectl apply -f $S/home/gt/.runtime/sbx-k8s/metrics-server-v0.9.0.yaml" "$CALLS_LOG"
check "no flag is patched onto an unverified deployment" lacks "patch deployment metrics-server" "$CALLS_LOG"

echo "a configured pin is used in place of the recorded digest"
scenario metrics_pin
# The curl stub writes this text for every download, so its digest is the pin.
mkdir -p "$S/home/gt/.runtime/sbx-k8s"
printf 'kind: List\n' >"$S/home/gt/.runtime/sbx-k8s/pin-probe"
stub_digest="$(digest "$S/home/gt/.runtime/sbx-k8s/pin-probe")"
run "$UP" GT_K8S_METRICS_SERVER_SHA256="$stub_digest" --
check "a matching configured pin lets the run proceed" test "$RC" -eq 0
check "a configured pin records no digest file" test ! -e "$S/home/gt/.runtime/sbx-k8s/metrics-server-v0.9.0.yaml.sha256"
check "the verified manifest is applied from the runtime copy" contains "kubectl apply -f $S/home/gt/.runtime/sbx-k8s/metrics-server-v0.9.0.yaml" "$CALLS_LOG"
scenario metrics_pin_wins
mkdir -p "$S/home/gt/.runtime/sbx-k8s"
printf '%s\n' "$stub_digest" >"$S/home/gt/.runtime/sbx-k8s/metrics-server-v0.9.0.yaml.sha256"
printf 'the copy verified before this run\n' >"$S/home/gt/.runtime/sbx-k8s/metrics-server-v0.9.0.yaml"
run "$UP" GT_K8S_METRICS_SERVER_SHA256=1111111111111111111111111111111111111111111111111111111111111111 --
check "a configured pin wins over a matching recorded digest" test "$RC" -ne 0
check "the failure names the configured digest" contains "expected 1111111111111111111111111111111111111111111111111111111111111111" "$OUT_TEXT"
check "the unverified manifest is not applied" lacks "kubectl apply -f $S/home/gt/.runtime/sbx-k8s/metrics-server-v0.9.0.yaml" "$CALLS_LOG"
check "a rejected download leaves the verified copy in place" contains "the copy verified before this run" "$(cat "$S/home/gt/.runtime/sbx-k8s/metrics-server-v0.9.0.yaml")"

echo "a warning on stderr does not hide the flag (kubectl merges stderr if the read captures it)"
scenario metrics_warning
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw"
run "$UP" STUB_METRICS_WARNING=1 --
run "$UP" STUB_METRICS_WARNING=1 --
check "the warned reads still exit 0" test "$RC" -eq 0
check "the flag is patched once across both runs" test "$(count 'patch deployment metrics-server' "$(cat "$S/calls.log")")" -eq 1
check "the deployment carries the flag exactly once" test "$(occurrences '--kubelet-insecure-tls' "$(cat "$S/state/metrics-args")")" -eq 1

echo "the insecure kubelet flag survives a rerun exactly once"
scenario metrics_rerun
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw"
run "$UP" --
check "the first run patches the flag in" test "$(occurrences '--kubelet-insecure-tls' "$(cat "$S/state/metrics-args")")" -eq 1
run "$UP" --
check "the rerun exits 0" test "$RC" -eq 0
# calls.log accumulates both runs; the guard means the two runs patch once.
check "the flag is not patched again when the deployment already has it" test "$(count 'patch deployment metrics-server' "$(cat "$S/calls.log")")" -eq 1
check "the deployment still carries the flag exactly once" test "$(occurrences '--kubelet-insecure-tls' "$(cat "$S/state/metrics-args")")" -eq 1
check "the manifest's own container args survive the patch" contains "--metric-resolution=15s" "$(cat "$S/state/metrics-args")"

echo "register the gateway in the town config"
scenario town
mkdir -p "$S/home/.config/openshell"
printf 'docker\n' >"$S/state/town-gw-list"
printf 'docker\n' >"$S/state/town-active" # active only in the list's starred row
run "$UP" -- --register-town
check "--register-town exits 0" test "$RC" -eq 0
town_calls="$(grep "XDG=$S/home/.config " "$S/calls.log")"
check "the k8s gateway is added to the town config" contains "gateway add https://127.0.0.1:28443 --local --name k8s" "$town_calls"
check "the starred active gateway is selected back" contains "gateway select docker" "$town_calls"
check "the active gateway is the one from before" test "$(cat "$S/home/.config/openshell/active_gateway")" = docker
check "the town list keeps the other gateway" test "$(grep -cx docker "$S/state/town-gw-list")" -eq 1
check "the town list holds one k8s entry" test "$(grep -cx k8s "$S/state/town-gw-list")" -eq 1
check "the town gateway is still registered in the private config" contains "gateway add https://127.0.0.1:28443 --local --name k8s" "$(grep "XDG=$S/home/gt/.runtime/sbx-k8s/xdg " "$S/calls.log")"
check "the town client certificates are exported too" test "$(count 'get secret openshell-client-tls' "$CALLS_LOG")" -eq 6
town_mtls="$S/home/.config/openshell/gateways/k8s/mtls"
check "the town client key is mode 600" test "$(mode "$town_mtls/tls.key")" = 600
check "the town CA certificate is mode 600" test "$(mode "$town_mtls/ca.crt")" = 600
check "the town mtls directory is private" test "$(mode "$town_mtls")" = 700
check "the town staging directory is gone" no_stage "$town_mtls"
check "no key material is printed for the town config" lacks "SECRETVALUE" "$OUT_TEXT"

echo "register-town reads the active_gateway file"
scenario town_file
mkdir -p "$S/home/.config/openshell"
printf 'docker\n' >"$S/state/town-gw-list"
printf 'docker\n' >"$S/state/town-active"
printf 'docker\n' >"$S/home/.config/openshell/active_gateway"
run "$UP" -- --register-town
check "up exits 0" test "$RC" -eq 0
check "the file's active gateway is selected back" contains "gateway select docker" "$CALLS_LOG"
check "the active_gateway file keeps its value" test "$(cat "$S/home/.config/openshell/active_gateway")" = docker

echo "register-town fails loudly when the restore fails"
scenario town_select_fail
mkdir -p "$S/home/.config/openshell"
printf 'docker\n' >"$S/state/town-gw-list"
printf 'docker\n' >"$S/state/town-active"
run "$UP" STUB_SELECT_FAIL=1 -- --register-town
check "a failed restore exits non-zero" test "$RC" -ne 0
check "the failure says the gateway could not be restored" contains "could not be restored to docker" "$OUT_TEXT"
check "the failure names the command to run" contains "openshell gateway select docker" "$OUT_TEXT"
check "the k8s entry stays registered when only the restore fails" test "$(grep -cx k8s "$S/state/town-gw-list")" -eq 1

echo "register-town warns when no gateway is active"
scenario town_no_active
mkdir -p "$S/home/.config/openshell"
printf 'docker\n' >"$S/state/town-gw-list"
run "$UP" -- --register-town
check "up exits 0" test "$RC" -eq 0
check "the run warns that no gateway was active" contains "no active gateway is named" "$OUT_TEXT"
check "the warning says k8s becomes active" contains "k8s becomes the active gateway" "$OUT_TEXT"
check "the warning names the command to choose another" contains "openshell gateway select" "$OUT_TEXT"
check "k8s is the active gateway" test "$(cat "$S/home/.config/openshell/active_gateway")" = k8s
check "the town list still holds the other gateway" test "$(grep -cx docker "$S/state/town-gw-list")" -eq 1

echo "a failed town registration puts the previous entry back"
scenario town_add_fail
mkdir -p "$S/home/.config/openshell"
printf 'docker\nk8s\n' >"$S/state/town-gw-list"
printf 'docker\n' >"$S/state/town-active"
printf 'docker\n' >"$S/home/.config/openshell/active_gateway"
run "$UP" STUB_TOWN_ADD_FAIL_ONCE=1 -- --register-town
check "the failed registration exits non-zero" test "$RC" -ne 0
check "the failure says the gateway could not be registered" contains "cannot register k8s" "$OUT_TEXT"
check "the previous k8s entry is back" test "$(grep -cx k8s "$S/state/town-gw-list")" -eq 1
check "the rollback says the entry is back" contains "its previous entry is back" "$OUT_TEXT"
check "the rollback restores the active gateway" test "$(cat "$S/home/.config/openshell/active_gateway")" = docker
check "the active gateway is not the new one" test "$(cat "$S/state/town-active")" = docker
check "the other gateway was never removed" test "$(grep -cx docker "$S/state/town-gw-list")" -eq 1

echo "a run without the flag leaves the town config alone"
scenario town_untouched
mkdir -p "$S/home/.config/openshell"
printf 'docker\n' >"$S/state/town-gw-list"
printf 'docker\n' >"$S/state/town-active"
printf 'docker\n' >"$S/home/.config/openshell/active_gateway"
run "$UP" --
check "up exits 0" test "$RC" -eq 0
check "the town active gateway is untouched" test "$(cat "$S/home/.config/openshell/active_gateway")" = docker
check "no gateway command runs against the town config" test "$(grep -c "XDG=$S/home/.config " "$S/calls.log")" -eq 0
check "no town certificates are written" test ! -e "$S/home/.config/openshell/gateways"
check "the town gateway list is unchanged" test "$(cat "$S/state/town-gw-list")" = docker

echo "register-town rerun replaces only the k8s entry"
scenario town_rerun
mkdir -p "$S/home/.config/openshell"
printf 'docker\n' >"$S/state/town-gw-list"
printf 'docker\n' >"$S/state/town-active"
run "$UP" -- --register-town
run "$UP" -- --register-town
check "both runs exit 0" test "$RC" -eq 0
check "the existing k8s entry is removed before the add" contains "gateway remove k8s" "$CALLS_LOG"
remove_line="$(grep -n 'gateway remove k8s' "$S/calls.log" | head -1 | cut -d: -f1)"
export_line="$(grep -n 'get secret openshell-client-tls' "$S/calls.log" | tail -1 | cut -d: -f1)"
add_line="$(grep -n 'gateway add https://127.0.0.1:28443 --local --name k8s' "$S/calls.log" | tail -1 | cut -d: -f1)"
check "the stale entry is removed before the town certificates are written" test "$remove_line" -lt "$export_line"
check "the town certificates are written before the new entry is added" test "$export_line" -lt "$add_line"
check "no gateway other than k8s is removed" test "$(count 'gateway remove' "$CALLS_LOG")" -eq 1
check "the town list holds one k8s entry" test "$(grep -cx k8s "$S/state/town-gw-list")" -eq 1
check "the other gateway survives" test "$(grep -cx docker "$S/state/town-gw-list")" -eq 1
check "the active gateway is still docker" test "$(cat "$S/home/.config/openshell/active_gateway")" = docker

echo "down"
scenario down
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw"
run "$DOWN" --
check "down exits 0" test "$RC" -eq 0
check "down deletes the kind cluster" contains "kind delete cluster --name gt-sbx" "$CALLS_LOG"
check "down removes the registry" contains "rm -f gt-registry" "$CALLS_LOG"
check "down removes the gateway registration" contains "gateway remove k8s" "$CALLS_LOG"
check "down leaves the VM running by default" lacks "colima stop" "$CALLS_LOG"
check "down changes no docker context when none flipped" lacks "docker context use" "$CALLS_LOG"
scenario down_vm
touch "$S/state/colima-running" "$S/state/cluster"
run "$DOWN" STUB_CTX_FLIP=colima-gt-sbx -- --stop-vm
check "--stop-vm stops the colima profile" contains "colima stop -p gt-sbx" "$CALLS_LOG"
check "--stop-vm restores a flipped context" test "$(cat "$S/state/ctx")" = desktop-linux

echo "down removes the town's k8s entry and keeps the other gateway selected"
scenario down_town
mkdir -p "$S/home/.config/openshell"
printf 'docker\nk8s\n' >"$S/state/town-gw-list"
printf 'docker\n' >"$S/state/town-active"
printf 'docker\n' >"$S/home/.config/openshell/active_gateway"
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw"
run "$DOWN" --
check "down exits 0" test "$RC" -eq 0
town_calls="$(grep "XDG=$S/home/.config " "$S/calls.log" || true)"
check "the k8s entry is removed from the town config" contains "gateway remove k8s" "$town_calls"
check "the town config holds no k8s entry" test "$(grep -cx k8s "$S/state/town-gw-list")" -eq 0
check "the other town gateway is kept" test "$(grep -cx docker "$S/state/town-gw-list")" -eq 1
check "the other gateway stays selected" test "$(cat "$S/home/.config/openshell/active_gateway")" = docker
check "only the k8s entry is removed from the town config" test "$(count 'gateway remove' "$town_calls")" -eq 1
check "the private registration is still removed" contains "gateway remove k8s" "$(grep "XDG=$S/home/gt/.runtime/sbx-k8s/xdg " "$S/calls.log")"
check "the town entry is removed before the cluster" before "gateway remove k8s" "kind delete cluster"

echo "down leaves a town config without a k8s entry alone"
scenario down_town_absent
mkdir -p "$S/home/.config/openshell"
printf 'docker\n' >"$S/state/town-gw-list"
printf 'docker\n' >"$S/state/town-active"
printf 'docker\n' >"$S/home/.config/openshell/active_gateway"
touch "$S/state/colima-running" "$S/state/cluster"
run "$DOWN" --
check "down exits 0" test "$RC" -eq 0
town_calls="$(grep "XDG=$S/home/.config " "$S/calls.log" || true)"
check "no town gateway is removed" lacks "gateway remove" "$town_calls"
check "no town gateway is selected" lacks "gateway select" "$town_calls"
check "the town gateway list is unchanged" test "$(cat "$S/state/town-gw-list")" = docker
check "the town active gateway is unchanged" test "$(cat "$S/home/.config/openshell/active_gateway")" = docker
check "the teardown still runs" contains "kind delete cluster" "$CALLS_LOG"

echo "down refuses to remove the town's active k8s entry"
scenario down_town_active
mkdir -p "$S/home/.config/openshell"
printf 'docker\nk8s\n' >"$S/state/town-gw-list"
printf 'k8s\n' >"$S/state/town-active"
printf 'k8s\n' >"$S/home/.config/openshell/active_gateway"
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw"
run "$DOWN" --
check "down refuses" test "$RC" -ne 0
check "the refusal names the active gateway" contains "k8s is the active gateway" "$OUT_TEXT"
check "the refusal names the command to select another gateway" contains "openshell gateway select" "$OUT_TEXT"
check "the town k8s entry is kept" test "$(grep -cx k8s "$S/state/town-gw-list")" -eq 1
check "the cluster is kept" test -f "$S/state/cluster"
check "no teardown runs before the refusal" lacks "kind delete cluster" "$CALLS_LOG"

echo "down selects the previous gateway back when the town removal drops it"
scenario down_town_reselect
mkdir -p "$S/home/.config/openshell"
printf 'docker\nk8s\n' >"$S/state/town-gw-list"
printf 'docker\n' >"$S/state/town-active"
printf 'docker\n' >"$S/home/.config/openshell/active_gateway"
touch "$S/state/colima-running" "$S/state/cluster"
run "$DOWN" STUB_TOWN_REMOVE_CLEARS_ACTIVE=1 --
check "down exits 0" test "$RC" -eq 0
check "the previous active gateway is selected again" test "$(cat "$S/home/.config/openshell/active_gateway")" = docker
check "the reselect goes through the town config" contains "gateway select docker" "$(grep "XDG=$S/home/.config " "$S/calls.log")"

scenario down_town_no_reselect
mkdir -p "$S/home/.config/openshell"
printf 'docker\nk8s\n' >"$S/state/town-gw-list"
printf 'docker\n' >"$S/state/town-active"
printf 'docker\n' >"$S/home/.config/openshell/active_gateway"
touch "$S/state/colima-running" "$S/state/cluster"
run "$DOWN" STUB_TOWN_REMOVE_CLEARS_ACTIVE=1 STUB_SELECT_FAIL=1 --
check "a failed reselect fails the run" test "$RC" -ne 0
check "the failure names the gateway to select" contains "openshell gateway select docker" "$OUT_TEXT"
check "the half-done removal still tears the cluster down" contains "kind delete cluster" "$CALLS_LOG"
check "the k8s entry is gone" test "$(grep -cx k8s "$S/state/town-gw-list")" -eq 0

echo "down stops before removal when the active gateway cannot be probed"
scenario down_town_probe_fails
mkdir -p "$S/home/.config/openshell"
printf 'docker\nk8s\n' >"$S/state/town-gw-list"
printf 'docker\n' >"$S/state/town-active"
touch "$S/state/colima-running" "$S/state/cluster" "$S/state/has-registry" "$S/state/gw"
run "$DOWN" STUB_TOWN_LIST_FAILS=2 --
check "down refuses" test "$RC" -ne 0
check "the failure names the active gateway probe" contains "cannot read the active gateway" "$OUT_TEXT"
check "the town k8s entry is kept" test "$(grep -cx k8s "$S/state/town-gw-list")" -eq 1
check "the cluster is kept" test -f "$S/state/cluster"
check "no teardown runs before the refusal" lacks "kind delete cluster" "$CALLS_LOG"

echo "down keeps a config that names no active gateway without selecting one"
scenario down_town_no_active
mkdir -p "$S/home/.config/openshell"
printf 'docker\nk8s\n' >"$S/state/town-gw-list"
touch "$S/state/colima-running" "$S/state/cluster"
run "$DOWN" --
check "down exits 0" test "$RC" -eq 0
town_calls="$(grep "XDG=$S/home/.config " "$S/calls.log" || true)"
check "the k8s entry is removed" contains "gateway remove k8s" "$town_calls"
check "no gateway is selected" lacks "gateway select" "$town_calls"
check "the teardown runs" contains "kind delete cluster" "$CALLS_LOG"

echo "down reports a selection the removal invented where none was chosen"
scenario down_town_selected_by_remove
mkdir -p "$S/home/.config/openshell"
printf 'docker\nk8s\n' >"$S/state/town-gw-list"
touch "$S/state/colima-running" "$S/state/cluster"
run "$DOWN" STUB_TOWN_REMOVE_SELECTS=docker --
check "the run fails" test "$RC" -ne 0
check "the warning names the gateway the CLI selected" contains "selected docker" "$OUT_TEXT"
check "nothing is selected back" lacks "gateway select" "$(grep "XDG=$S/home/.config " "$S/calls.log" || true)"
check "the teardown still runs" contains "kind delete cluster" "$CALLS_LOG"

if [ "$FAIL" -eq 0 ]; then
  echo "kind_test.sh: all $PASS cases passed"
else
  echo "kind_test.sh: $FAIL of $((PASS + FAIL)) cases failed"
  exit 1
fi
