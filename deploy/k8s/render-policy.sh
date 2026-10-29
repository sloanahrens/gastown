#!/usr/bin/env bash
# Render the polecat sandbox policy for a Kubernetes gateway from the Docker
# policy (deploy/polecat/policy.yaml), so the two cannot drift apart.
#
# The Docker policy reaches Dolt (:3307), Forgejo (:3000) and the tracker
# (:7433) as host.docker.internal. On Kubernetes the sandbox names an
# in-cluster Service (an ExternalName pointing at the Mac), and the policy must
# also pin the address that name resolves to: OpenShell refuses private
# addresses unless the endpoint lists them in allowed_ips. Verified on kind
# 2026-10-24: Dolt over
# raw TCP and Forgejo's git smart-HTTP paths pass; the web UI, API and LFS are
# denied by the layer-7 rules.
#
# The source header names Docker Desktop's host alias; the rendered header names
# the in-cluster Services, so a kind policy does not describe the Docker route.
#
# usage: render-policy.sh [--dolt-host H] [--forgejo-host H] [--tracker-host H] [--allowed-ip IP] [BASE]
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
dolt_host="control-plane.openshell.svc.cluster.local"
forgejo_host="control-plane.openshell.svc.cluster.local"
tracker_host="tracker.openshell.svc.cluster.local"
tracker_port="7433" # trackerd's default port, the same value as the Docker policy
allowed_ip="192.168.5.2" # host.lima.internal from inside the kind node
base="$here/../polecat/policy.yaml"

while [ $# -gt 0 ]; do
  case "$1" in
    --dolt-host) dolt_host="$2"; shift 2 ;;
    --forgejo-host) forgejo_host="$2"; shift 2 ;;
    --tracker-host) tracker_host="$2"; shift 2 ;;
    --allowed-ip) allowed_ip="$2"; shift 2 ;;
    -*) echo "render-policy: unknown option $1" >&2; exit 2 ;;
    *) base="$1"; shift ;;
  esac
done
[ -r "$base" ] || { echo "render-policy: cannot read $base" >&2; exit 1; }

# Both producers of rendered YAML, so neither may carry YAML syntax: a name is
# a dotted label sequence, an address is four decimal octets (gt-ywwf2).
valid_host() {
  local name=$1 label
  case "$name" in ''|*[!A-Za-z0-9.-]*) return 1 ;; esac
  [ ${#name} -le 253 ] || return 1
  local IFS=.
  # shellcheck disable=SC2086 # deliberate: split the name on dots
  set -- $name
  for label in "$@"; do
    case "$label" in ''|-*|*-) return 1 ;; esac
    [ ${#label} -le 63 ] || return 1
  done
}

valid_ipv4() {
  local addr=$1 octet
  case "$addr" in ''|*[!0-9.]*) return 1 ;; esac
  local IFS=.
  # shellcheck disable=SC2086 # deliberate: split the address on dots
  set -- $addr
  [ $# -eq 4 ] || return 1
  for octet in "$@"; do
    case "$octet" in ''|*[!0-9]*) return 1 ;; esac
    [ "$octet" -le 255 ] || return 1
  done
}

valid_ipv4 "$allowed_ip" || {
  echo "render-policy: --allowed-ip must be an IPv4 address, got '$allowed_ip'" >&2
  exit 2
}
valid_host "$dolt_host" || {
  echo "render-policy: --dolt-host must be a DNS name, got '$dolt_host'" >&2
  exit 2
}
valid_host "$forgejo_host" || {
  echo "render-policy: --forgejo-host must be a DNS name, got '$forgejo_host'" >&2
  exit 2
}
valid_host "$tracker_host" || {
  echo "render-policy: --tracker-host must be a DNS name, got '$tracker_host'" >&2
  exit 2
}

# The endpoint's port is not required to be the line after its host, and two
# host lines may be adjacent: buffer the block until the port decides it, and
# flush it unchanged when the block ends first (gt-ywwf2).
awk -v dolt="$dolt_host" -v forgejo="$forgejo_host" -v tracker="$tracker_host" \
    -v tport="$tracker_port" -v ip="$allowed_ip" \
    -v stale="Docker Desktop's host alias" -v route="in-cluster Services" '
  {
    if (pending) {
      if ($0 ~ /^ *port:/) {
        port = $0; sub(/^ *port:[ \t]*/, "", port); sub(/[^0-9].*$/, "", port)
        if (port == "3000") name = forgejo
        else if (port == "3307") name = dolt
        else if (port == tport) name = tracker
        else name = ""
        if (name != "") {
          indent = $0; sub(/port:.*/, "", indent)
          h = held; sub(/host: *host\.docker\.internal/, "host: " name, h)
          print h
          print indent "allowed_ips: [\"" ip "/32\"]"
          print
          printf "%s", buf
        } else {
          print held
          printf "%s", buf
          print
        }
        pending = 0; held = ""; buf = ""
        next
      }
      if ($0 ~ /^ *- host: *host\.docker\.internal[ \t]*$/) {
        print held; printf "%s", buf
        held = $0; buf = ""
        next
      }
      if ($0 ~ /^ *- /) {
        print held; printf "%s", buf
        pending = 0; held = ""; buf = ""
      } else {
        buf = buf $0 "\n"
        next
      }
    }
    if ($0 ~ /^ *- host: *host\.docker\.internal[ \t]*$/) {
      held = $0; buf = ""; pending = 1
      next
    }
    if (index($0, stale)) sub(stale, route)
    print
  }
  END { if (pending) { print held; printf "%s", buf } }
' "$base"
