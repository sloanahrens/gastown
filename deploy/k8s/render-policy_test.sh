#!/usr/bin/env bash
# Tests for deploy/k8s/render-policy.sh and the checked-in Kubernetes policy and
# Services (gt-fju1j).
#
# What matters: each in-cluster name lands on the right port (Dolt on 3307,
# Forgejo on 3000, never swapped), the pinned address is present and cannot be
# injected, the Docker policy is only read, and the checked-in kind policy is
# exactly what the script renders from the Docker policy (so they cannot
# drift). No cluster, no network.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RENDER="$SCRIPT_DIR/render-policy.sh"
BASE="$SCRIPT_DIR/../polecat/policy.yaml"
KIND="$SCRIPT_DIR/policy/kind.yaml"
SERVICES="$SCRIPT_DIR/services/control-plane.yaml"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/render-policy-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); if [ $# -gt 1 ]; then printf '%s\n' "$2" | sed 's/^/    | /'; fi; }
check() { local desc=$1; shift; if "$@"; then pass "$desc"; else fail "$desc"; fi; }
contains() { case "$2" in *"$1"*) return 0 ;; *) return 1 ;; esac; }
lacks() { ! contains "$1" "$2"; }

# endpoint_of PORT TEXT: the three lines of the endpoint that listens on PORT
# (host, allowed_ips, port), or the host and port lines when there is no pin.
endpoint_of() {
  awk -v want="$1" '
    /- host:/ { h = $0; next }
    h != "" && /allowed_ips:/ { a = $0; next }
    h != "" && /port:/ { p = $0; sub(/^ *port: */, "", p); if (p == want) { print h; if (a != "") print a; print $0 } h = ""; a = "" }
  ' <<<"$2"
}

out="$("$RENDER")"
base_sum_before="$(shasum "$BASE")"

echo "default render"
check "no host.docker.internal is left" lacks "host.docker.internal" "$out"
check "the render differs from the Docker policy in exactly 6 lines" test "$(diff "$BASE" <(printf '%s\n' "$out") | grep -c '^[<>]')" -eq 6
check "the Docker policy file is only read" test "$(shasum "$BASE")" = "$base_sum_before"
dolt="$(endpoint_of 3307 "$out")"
forgejo="$(endpoint_of 3000 "$out")"
check "dolt's endpoint is the control-plane name" contains "host: control-plane.openshell.svc.cluster.local" "$dolt"
check "dolt's endpoint pins the Mac address" contains 'allowed_ips: ["192.168.5.2/32"]' "$dolt"
check "forgejo's endpoint is the control-plane name" contains "host: control-plane.openshell.svc.cluster.local" "$forgejo"
check "forgejo's endpoint pins the Mac address" contains 'allowed_ips: ["192.168.5.2/32"]' "$forgejo"

echo "separate names land on the right ports"
out2="$("$RENDER" --dolt-host dolt.example --forgejo-host forgejo.example --allowed-ip 10.0.0.9)"
dolt2="$(endpoint_of 3307 "$out2")"
forgejo2="$(endpoint_of 3000 "$out2")"
check "the dolt name is on port 3307" contains "host: dolt.example" "$dolt2"
check "the forgejo name is on port 3000" contains "host: forgejo.example" "$forgejo2"
check "the dolt name is not on port 3000" lacks "dolt.example" "$forgejo2"
check "the forgejo name is not on port 3307" lacks "forgejo.example" "$dolt2"
check "--allowed-ip is the pinned address on both" test "$(grep -c 'allowed_ips: \["10.0.0.9/32"\]' <<<"$out2")" -eq 2

echo "bad input"
set +e
"$RENDER" --allowed-ip '1.2.3.4/32"]; evil' >"$TMP/o" 2>"$TMP/e"; rc=$?
set -e
check "an allowed-ip that is not an address is rejected" test "$rc" -ne 0
check "a rejected allowed-ip renders nothing" test ! -s "$TMP/o"
set +e
"$RENDER" "$TMP/nope.yaml" >"$TMP/o" 2>"$TMP/e"; rc=$?
set -e
check "a missing base file fails" test "$rc" -eq 1
check "the failure names the file" contains "nope.yaml" "$(cat "$TMP/e")"
set +e
"$RENDER" --bogus >"$TMP/o" 2>"$TMP/e"; rc=$?
set -e
check "an unknown option is rejected" test "$rc" -eq 2

echo "other endpoints are left alone"
cat >"$TMP/other.yaml" <<'YAML'
network_policies:
  extra:
    endpoints:
      - host: host.docker.internal
        port: 9999
      - host: proxy.golang.org
        port: 443
      - host: host.docker.internal
YAML
other="$("$RENDER" "$TMP/other.yaml")"
check "a host.docker.internal endpoint on another port is unchanged" test "$(endpoint_of 9999 "$other")" = "      - host: host.docker.internal
        port: 9999"
check "an unrelated endpoint is unchanged" contains "host: proxy.golang.org" "$other"
check "a trailing host line at the end of the file is kept" test "$(tail -1 <<<"$other")" = "      - host: host.docker.internal"
check "no pin is added to an endpoint that was not rewritten" lacks "allowed_ips" "$other"

echo "checked-in files"
check "policy/kind.yaml is exactly what the script renders (no drift)" test "$(cat "$KIND")" = "$out"
check "three Services are defined" test "$(grep -c '^kind: Service' "$SERVICES")" -eq 3
check "every Service is an ExternalName to the Mac" test "$(grep -c 'externalName: host.lima.internal' "$SERVICES")" -eq 3
check "the names are control-plane, dolt and forgejo" test "$(grep -o 'name: [a-z-]*,' "$SERVICES" | tr -d ',' | sort | tr '\n' ' ')" = "name: control-plane name: dolt name: forgejo "
check "no Service has a selector key" test "$(grep -c '^ *selector:' "$SERVICES")" -eq 0

if [ "$FAIL" -eq 0 ]; then
  echo "render-policy_test.sh: all $PASS cases passed"
else
  echo "render-policy_test.sh: $FAIL of $((PASS + FAIL)) cases failed"
  exit 1
fi
