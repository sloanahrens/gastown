#!/usr/bin/env bash
# Stand up the local Kubernetes sandbox environment (see docs/design/k8s-sandboxes.md):
# a Colima VM, a kind cluster with a fixed gateway port, a local image
# registry, the pinned agent-sandbox controller and the OpenShell chart, then
# register the gateway as `k8s` in an isolated CLI config. Safe to rerun.
# --register-town also registers it in the town's OpenShell config, where the
# daemon looks, and leaves the previously active gateway selected (gt-0knzj).
set -euo pipefail
# shellcheck source=common.sh
. "$(dirname "$0")/common.sh"

register_town=0
while [ $# -gt 0 ]; do
  case "$1" in
    --register-town) register_town=1 ;;
    -h | --help)
      echo "usage: $0 [--register-town]"
      exit 0
      ;;
    *)
      echo "usage: $0 [--register-town]" >&2
      die "unknown argument: $1"
      ;;
  esac
  shift
done

for tool in colima kind kubectl helm docker openssl openshell curl; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is not on PATH (helm may live in $OUT/bin)"
done
mkdir -p "$OUT"

# port_in_use answers whether 127.0.0.1:$1 has a listener. A probe that cannot
# run is not a free port: reporting it as free would let the run bind a port
# someone else owns (or report a stale value), so it stops the run instead.
port_in_use() { # a listener on 127.0.0.1:$1, whoever owns it
  local out rc msg
  if command -v lsof >/dev/null 2>&1; then
    rc=0
    out="$(lsof -nP -iTCP:"$1" -sTCP:LISTEN 2>&1)" || rc=$?
    case "$rc" in
      0) [ -n "$out" ] ;;
      1) return 1 ;; # lsof's "no matching listener"
      *) die "cannot probe 127.0.0.1:$1 with lsof (exit $rc): $out" ;;
    esac
  else
    if msg="$({ exec 3<>"/dev/tcp/127.0.0.1/$1"; } 2>&1)"; then
      return 0
    fi
    case "$msg" in
      *"Connection refused"*) return 1 ;;
      *) die "cannot probe 127.0.0.1:$1: $msg" ;;
    esac
  fi
}

original_context="$(docker context show)"
cert_stage=""
town_rollback_pending=0
# colima start can fail after it has already switched docker's global context,
# so the restore is an EXIT trap rather than a call after the start returns.
# --register-town registers its rollback here too: a run that stops between
# removing the town's k8s entry and re-adding it puts the previous entry and
# the previous active gateway back (town_rollback, defined below).
cleanup() {
  if [ -n "$cert_stage" ]; then
    rm -rf "$cert_stage"
  fi
  if [ "$town_rollback_pending" = 1 ] && declare -F town_rollback >/dev/null; then
    town_rollback
  fi
  restore_context "$original_context" || true
}
trap cleanup EXIT

if ! colima status -p "$PROFILE" >/dev/null 2>&1; then
  say "starting colima profile $PROFILE"
  colima start "$PROFILE" --cpu 6 --memory 16 --disk 60 --vm-type vz --runtime docker ||
    die "colima start $PROFILE failed"
fi
restore_context "$original_context"

# The registry is ensured on every run, so a lost registry is repaired even
# when the cluster already exists.
if ! vmdocker inspect "$REGISTRY_NAME" >/dev/null 2>&1; then
  port_in_use "$REGISTRY_PORT" && die "127.0.0.1:$REGISTRY_PORT already has a listener; set GT_K8S_REGISTRY_PORT"
  say "starting local registry on 127.0.0.1:$REGISTRY_PORT"
  vmdocker run -d --restart=always -p "127.0.0.1:$REGISTRY_PORT:5000" --name "$REGISTRY_NAME" registry:2 >/dev/null
fi

if vmkind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  say "kind cluster $CLUSTER exists; leaving it"
else
  port_in_use "$GATEWAY_PORT" && die "127.0.0.1:$GATEWAY_PORT already has a listener (not ours to touch); set GT_K8S_GATEWAY_PORT to a free port"
  cat >"$OUT/kind-config.yaml" <<YAML
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: $CLUSTER
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/etc/containerd/certs.d"
nodes:
- role: control-plane
  extraPortMappings:
  - containerPort: 30443
    hostPort: $GATEWAY_PORT
    listenAddress: 127.0.0.1
YAML
  say "creating kind cluster $CLUSTER"
  vmkind create cluster --config "$OUT/kind-config.yaml" --wait 180s
fi

# Nodes pull localhost:$REGISTRY_PORT/... from the registry container over the kind network.
vmdocker network connect kind "$REGISTRY_NAME" >/dev/null 2>&1 || true
for node in $(vmkind get nodes --name "$CLUSTER"); do
  vmdocker exec "$node" mkdir -p "/etc/containerd/certs.d/localhost:$REGISTRY_PORT"
  printf 'server = "http://%s:5000"\n[host."http://%s:5000"]\n  capabilities = ["pull", "resolve"]\n' "$REGISTRY_NAME" "$REGISTRY_NAME" |
    vmdocker exec -i "$node" sh -c "cat > /etc/containerd/certs.d/localhost:$REGISTRY_PORT/hosts.toml"
done

# The release tag can be re-pointed at different bytes; the digest is what
# pins them. The first fetch records its digest beside the manifest and later
# runs refuse a changed asset; GT_K8S_AGENT_SANDBOX_SHA256 pins it from the
# first run.
say "installing agent-sandbox $AGENT_SANDBOX_VERSION"
sandbox_manifest="$OUT/agent-sandbox-$AGENT_SANDBOX_VERSION.yaml"
curl -fsSL -o "$sandbox_manifest" \
  "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$AGENT_SANDBOX_VERSION/sandbox.yaml"
actual="$(sha256_of "$sandbox_manifest")"
expected="$AGENT_SANDBOX_SHA256"
if [ -z "$expected" ] && [ -f "$sandbox_manifest.sha256" ]; then
  expected="$(cat "$sandbox_manifest.sha256")"
fi
if [ -z "$expected" ]; then
  printf '%s\n' "$actual" >"$sandbox_manifest.sha256"
  say "recorded the agent-sandbox manifest digest ($actual) in $sandbox_manifest.sha256"
elif [ "$actual" != "$expected" ]; then
  die "agent-sandbox $AGENT_SANDBOX_VERSION is $actual, expected $expected; refusing to apply it"
fi
kubectl apply -f "$sandbox_manifest"
kubectl -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s

say "installing OpenShell chart $CHART_VERSION"
helm upgrade --install openshell oci://ghcr.io/nvidia/openshell/helm-chart --version "$CHART_VERSION" \
  --namespace openshell --create-namespace -f "$VALUES" --wait --timeout 7m
kubectl apply -f "$HERE/gateway-nodeport.yaml"
kubectl apply -f "$HERE/../services/control-plane.yaml"
kubectl -n openshell rollout status statefulset/openshell --timeout=300s

# Client certificates for the CLI. The key is never printed; the CLI wants
# PKCS8. Each file is written under umask 077 into a mktemp-created 700
# staging directory inside the destination and only then moved into place, so
# no reader ever sees a world-readable key or a half-written one.
mtls="$XDG_CONFIG_HOME/openshell/gateways/$GATEWAY_NAME/mtls"
export_certs() { # export_certs MTLS_DIR
  local dest=$1 old_umask key
  old_umask="$(umask)"
  umask 077
  mkdir -p "$dest"
  chmod 700 "$(dirname "$dest")" "$dest"
  cert_stage="$(mktemp -d "$dest/.stage.XXXXXX")"
  for key in ca.crt tls.crt tls.key; do
    kubectl -n openshell get secret openshell-client-tls -o "jsonpath={.data['${key//./\\.}']}" |
      base64 -d >"$cert_stage/$key"
  done
  openssl pkcs8 -topk8 -nocrypt -in "$cert_stage/tls.key" -out "$cert_stage/tls.key.pk8"
  chmod 600 "$cert_stage/ca.crt" "$cert_stage/tls.crt" "$cert_stage/tls.key.pk8"
  mv -f "$cert_stage/tls.key.pk8" "$cert_stage/tls.key"
  mv -f "$cert_stage/ca.crt" "$cert_stage/tls.crt" "$cert_stage/tls.key" "$dest/"
  rm -rf "$cert_stage"
  cert_stage=""
  umask "$old_umask"
}

# Exact field match: a gateway named k8s-other in the list is not this one.
gateway_registered() {
  openshell gateway list 2>/dev/null |
    awk -v name="$GATEWAY_NAME" '{ for (i = 1; i <= NF; i++) if ($i == name) found = 1 } END { exit !found }'
}

# Colima forwards a published port to the Mac a moment after the container
# publishes it, so wait (bounded) for the gateway to answer before using it.
tries="${GT_K8S_POLL_TRIES:-30}"
interval="${GT_K8S_POLL_INTERVAL:-2}"
until curl -sk -o /dev/null "https://127.0.0.1:$GATEWAY_PORT/"; do
  tries=$((tries - 1))
  [ "$tries" -gt 0 ] || die "the gateway does not answer on 127.0.0.1:$GATEWAY_PORT (kind port mapping or Colima port forward)"
  sleep "$interval"
done

# The chart's certificate job can rewrite the secrets after the first read
# (seen 2026-10-24: the CA read at install was not the one the gateway then
# served), so the real acceptance is a healthy handshake, not a successful
# export. Re-export and retry, bounded.
healthy=0
attempts="${GT_K8S_CERT_TRIES:-30}"
while [ "$attempts" -gt 0 ]; do
  export_certs "$mtls"
  gateway_registered ||
    openshell gateway add "https://127.0.0.1:$GATEWAY_PORT" --local --name "$GATEWAY_NAME"
  info="$(openshell -g "$GATEWAY_NAME" gateway info 2>&1 || true)"
  if grep -qw healthy <<<"$info"; then healthy=1; break; fi
  attempts=$((attempts - 1))
  say "gateway not healthy yet ($(tr '\n' ' ' <<<"$info" | tr -s ' ' | cut -c1-110)); re-exporting client certificates ($attempts tries left)"
  sleep "$interval"
done
[ "$healthy" = 1 ] || die "gateway $GATEWAY_NAME is not healthy after re-exporting its client certificates"

# --register-town: the daemon runs `openshell -g k8s ...` against the town
# config, not the private one above, so copy the certificates and the entry
# there. A rebuild changes the client certificates, so every run replaces the
# k8s entry. `gateway add` makes the new gateway active, so read the active
# gateway first and select it back afterwards. The town helpers live in common.sh.

# town_rollback runs from the EXIT trap when the run stops after removing the
# town's k8s entry but before its replacement is in place: the entry is added
# back (with the certificates this run exported) and the gateway that was
# active before is selected again, so a failed or interrupted registration
# never leaves the daemon without k8s or on a gateway nobody chose. A rollback
# that cannot put the old state back warns loudly rather than failing twice.
town_rollback() {
  if [ "$town_entry_before" = 1 ] && ! town_gateway_registered; then
    if town_openshell gateway add "https://127.0.0.1:$GATEWAY_PORT" --local --name "$GATEWAY_NAME" >/dev/null 2>&1; then
      warn "the run stopped while re-registering $GATEWAY_NAME; its previous entry is back in $TOWN_CONFIG"
    else
      warn "the run stopped while re-registering $GATEWAY_NAME and $TOWN_CONFIG has no $GATEWAY_NAME entry; restore it with: openshell gateway add https://127.0.0.1:$GATEWAY_PORT --local --name $GATEWAY_NAME"
    fi
  fi
  if [ -n "$town_active_before" ] && [ "$town_active_before" != "$GATEWAY_NAME" ] &&
    ! town_openshell gateway select "$town_active_before" >/dev/null 2>&1; then
    warn "the active gateway in $TOWN_CONFIG was not restored to $town_active_before; run: openshell gateway select $town_active_before"
  fi
}

if [ "$register_town" = 1 ]; then
  town_active_before="$(town_active_gateway)"
  town_entry_before=0
  town_gateway_registered && town_entry_before=1
  if [ -z "$town_active_before" ]; then
    warn "no active gateway is named in $TOWN_CONFIG; $GATEWAY_NAME becomes the active gateway (openshell gateway select <name> to change it)"
  fi
  # From the remove to the add the config has no k8s entry, so the EXIT trap
  # owns putting it back; town_rollback_pending arms it and the successful add
  # disarms it. The old entry is removed before the new certificates are
  # written, so a stale entry never points at the new cluster's certificates.
  town_rollback_pending=1
  if [ "$town_entry_before" = 1 ]; then
    town_openshell gateway remove "$GATEWAY_NAME" >/dev/null ||
      die "cannot remove the stale $GATEWAY_NAME entry from $TOWN_CONFIG"
  fi
  export_certs "$TOWN_CONFIG/gateways/$GATEWAY_NAME/mtls"
  town_openshell gateway add "https://127.0.0.1:$GATEWAY_PORT" --local --name "$GATEWAY_NAME" ||
    die "cannot register $GATEWAY_NAME in $TOWN_CONFIG"
  if [ -n "$town_active_before" ] && [ "$town_active_before" != "$GATEWAY_NAME" ]; then
    town_openshell gateway select "$town_active_before" ||
      die "$GATEWAY_NAME is registered in $TOWN_CONFIG but the active gateway could not be restored to $town_active_before; run: openshell gateway select $town_active_before"
    active_after="$(town_active_gateway)"
    [ "$active_after" = "$town_active_before" ] ||
      die "$GATEWAY_NAME is registered in $TOWN_CONFIG but the active gateway is $active_after, not $town_active_before; run: openshell gateway select $town_active_before"
  fi
  town_rollback_pending=0
  say "registered $GATEWAY_NAME in $TOWN_CONFIG (active gateway: $(town_active_gateway))"
fi

say "ready: openshell -g $GATEWAY_NAME ..."
say "export XDG_CONFIG_HOME=$XDG_CONFIG_HOME KUBECONFIG=$KUBECONFIG"
