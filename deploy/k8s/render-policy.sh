#!/usr/bin/env bash
# Render the polecat sandbox policy for a Kubernetes gateway from the Docker
# policy (deploy/polecat/policy.yaml), so the two cannot drift apart.
#
# The Docker policy reaches Dolt (:3307) and Forgejo (:3000) as
# host.docker.internal. On Kubernetes the sandbox names an in-cluster Service
# (an ExternalName pointing at the Mac), and the policy must also pin the
# address that name resolves to: OpenShell refuses private addresses unless the
# endpoint lists them in allowed_ips. Verified on kind 2026-10-24: Dolt over
# raw TCP and Forgejo's git smart-HTTP paths pass; the web UI, API and LFS are
# denied by the layer-7 rules.
#
# The source header names Docker Desktop's host alias; the rendered header names
# the in-cluster Services, so a kind policy does not describe the Docker route.
#
# usage: render-policy.sh [--dolt-host H] [--forgejo-host H] [--allowed-ip IP] [BASE]
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
dolt_host="control-plane.openshell.svc.cluster.local"
forgejo_host="control-plane.openshell.svc.cluster.local"
allowed_ip="192.168.5.2" # host.lima.internal from inside the kind node
base="$here/../polecat/policy.yaml"

while [ $# -gt 0 ]; do
  case "$1" in
    --dolt-host) dolt_host="$2"; shift 2 ;;
    --forgejo-host) forgejo_host="$2"; shift 2 ;;
    --allowed-ip) allowed_ip="$2"; shift 2 ;;
    -*) echo "render-policy: unknown option $1" >&2; exit 2 ;;
    *) base="$1"; shift ;;
  esac
done
[ -r "$base" ] || { echo "render-policy: cannot read $base" >&2; exit 1; }
case "$allowed_ip" in
  *[!0-9.]*|"") echo "render-policy: --allowed-ip must be an IPv4 address, got '$allowed_ip'" >&2; exit 2 ;;
esac

awk -v dolt="$dolt_host" -v forgejo="$forgejo_host" -v ip="$allowed_ip" \
    -v stale="Docker Desktop's host alias" -v route="in-cluster Services" '
  /^ *- host: host\.docker\.internal *$/ { held = $0; next }
  held != "" {
    indent = $0; sub(/port:.*/, "", indent)
    port = $0; sub(/^ *port: */, "", port)
    if (port == "3000") name = forgejo
    else if (port == "3307") name = dolt
    else { print held; print; held = ""; next }
    sub(/host: host\.docker\.internal/, "host: " name, held)
    print held
    print indent "allowed_ips: [\"" ip "/32\"]"
    print
    held = ""
    next
  }
  index($0, stale) { sub(stale, route) }
  { print }
  END { if (held != "") { print held } }
' "$base"
