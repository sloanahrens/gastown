#!/usr/bin/env bash
# Stand up the local Kubernetes sandbox environment (see docs/design/k8s-sandboxes.md):
# a Colima VM, a kind cluster with a fixed gateway port, a local image
# registry, the pinned metrics-server and agent-sandbox releases and the
# OpenShell chart, then register the gateway as `k8s` in an isolated CLI config.
# Safe to rerun.
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
town_unverified=0 # 1 while a freshly added town entry has not yet been seen healthy
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
  colima start "$PROFILE" --activate=false --cpu "$COLIMA_CPU" --memory "$COLIMA_MEMORY" --disk "$COLIMA_DISK" --vm-type vz --runtime docker ||
    die "colima start $PROFILE failed"
fi
restore_context "$original_context"

# The registry is ensured on every run, so a lost registry is repaired even
# when the cluster already exists.
if ! vmdocker inspect "$REGISTRY_NAME" >/dev/null 2>&1; then
  port_in_use "$REGISTRY_PORT" && die "127.0.0.1:$REGISTRY_PORT already has a listener; set GT_K8S_REGISTRY_PORT"
  say "starting local registry on 127.0.0.1:$REGISTRY_PORT"
  # The named volume keeps pushed images across a recreate of a registry created here; an existing
  # container is left alone with whatever volume it has. The delete flag lets the pruner remove
  # manifests, and the registry listens on the loopback address only.
  vmdocker run -d --restart=always -p "127.0.0.1:$REGISTRY_PORT:5000" -v gt-registry-data:/var/lib/registry -e REGISTRY_STORAGE_DELETE_ENABLED=true --name "$REGISTRY_NAME" registry:2 >/dev/null
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
# The pools mirror the cloud node pools: control-plane workloads on core, and
# only pods that tolerate the taint on sandbox (a node reads the secrets of
# the pods scheduled on it, so sandbox nodes hold none of the control plane's).
# Labels and the taint are set at node registration, so a rebuilt cluster is
# right from its first start.
- role: worker
  labels:
    gt.dev/pool: core
- role: worker
  labels:
    gt.dev/pool: sandbox
  kubeadmConfigPatches:
  - |
    kind: JoinConfiguration
    nodeRegistration:
      taints:
      - key: gt.dev/pool
        value: sandbox
        effect: NoSchedule
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

# A cluster built before the three-node layout has no pools. Refuse it rather
# than half-configure it: rebuild with down.sh then up.sh.
# A kubectl failure is not a missing layout: the two get different messages.
pool_list="$(kubectl get nodes -l gt.dev/pool -o name 2>&1)" ||
  die "cannot list the nodes of cluster $CLUSTER: $(tr '\n' ' ' <<<"$pool_list" | cut -c1-160)"
pool_nodes="$(grep -c . <<<"$pool_list" || true)"
[ "$pool_nodes" = 2 ] ||
  die "cluster $CLUSTER lacks the core and sandbox nodes ($pool_nodes labelled, want 2); rebuild it: deploy/k8s/kind/down.sh && deploy/k8s/kind/up.sh"
kubectl apply -f "$HERE/../priority-classes.yaml"

# fetch_pinned NAME VERSION URL DEST PIN downloads URL and moves it to DEST only
# once its sha256 matches PIN or the digest the first fetch recorded beside DEST,
# so a rejected manifest never replaces the verified copy a later run applies.
# A release tag can be re-pointed at different bytes; the digest is what pins
# them.
fetch_pinned() { # fetch_pinned NAME VERSION URL DEST PIN
  local name=$1 version=$2 url=$3 dest=$4 expected=${5:-} actual
  curl -fsSL -o "$dest.tmp" "$url"
  actual="$(sha256_of "$dest.tmp")"
  if [ -z "$expected" ] && [ -f "$dest.sha256" ]; then
    expected="$(cat "$dest.sha256")"
  fi
  if [ -z "$expected" ]; then
    printf '%s\n' "$actual" >"$dest.sha256"
    say "recorded the $name manifest digest ($actual) in $dest.sha256"
  elif [ "$actual" != "$expected" ]; then
    rm -f "$dest.tmp"
    die "$name $version is $actual, expected $expected; refusing to apply it"
  fi
  mv -f "$dest.tmp" "$dest"
}

# metrics-server feeds `kubectl top` and the resource sampler. kind's kubelet
# serves a certificate metrics-server cannot verify, so the metrics API answers
# only once its deployment allows insecure TLS; the flag is appended here and
# never in a values file a cloud environment reuses.
say "installing metrics-server $METRICS_SERVER_VERSION"
metrics_manifest="$OUT/metrics-server-$METRICS_SERVER_VERSION.yaml"
fetch_pinned metrics-server "$METRICS_SERVER_VERSION" \
  "https://github.com/kubernetes-sigs/metrics-server/releases/download/$METRICS_SERVER_VERSION/components.yaml" \
  "$metrics_manifest" "$METRICS_SERVER_SHA256"
kubectl apply -f "$metrics_manifest"
# stdout only: kubectl can warn on stderr during a successful read, and text
# appended to the args would hide the flag from the check below.
metrics_args="$(kubectl -n kube-system get deployment metrics-server -o jsonpath='{.spec.template.spec.containers[0].args[*]}' 2>/dev/null)" ||
  die "cannot read the metrics-server deployment's args in cluster $CLUSTER"
case " $metrics_args " in
  *" --kubelet-insecure-tls "*) ;;
  *)
    kubectl -n kube-system patch deployment metrics-server --type json \
      -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
    ;;
esac
kubectl -n kube-system rollout status deployment/metrics-server --timeout=180s

say "installing agent-sandbox $AGENT_SANDBOX_VERSION"
sandbox_manifest="$OUT/agent-sandbox-$AGENT_SANDBOX_VERSION.yaml"
fetch_pinned agent-sandbox "$AGENT_SANDBOX_VERSION" \
  "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$AGENT_SANDBOX_VERSION/sandbox.yaml" \
  "$sandbox_manifest" "$AGENT_SANDBOX_SHA256"
kubectl apply -f "$sandbox_manifest"
# Until this patch the controller may start on the control-plane node; it can
# never start on the sandbox node (the taint), and the patch rolls it onto core
# before the rollout below is awaited.
kubectl -n agent-sandbox-system patch deployment agent-sandbox-controller --type merge \
  -p '{"spec":{"template":{"spec":{"nodeSelector":{"gt.dev/pool":"core"}}}}}'
kubectl -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s

# helm_release_deployed answers whether openshell is a deployed release. helm
# status exits 0 for a failed release too, and skipping helm for one would leave
# the gateway down while the marker says it is installed.
helm_release_deployed() {
  [ "$(helm list --namespace openshell --deployed --filter '^openshell$' --short 2>/dev/null)" = openshell ]
}

# The chart's certificate job is a helm pre-install AND pre-upgrade hook, so
# every `helm upgrade` regenerates the CA and every certificate and rolls the
# gateway: each exported copy of the client certificates (the town
# registration included) goes stale at once. A marker records the chart version
# and the values file the release was installed with; an unchanged marker on a
# deployed release means no helm call. GT_K8S_FORCE_HELM=1 forces one.
helm_marker="$CHART_VERSION:$(sha256_of "$VALUES")"
helm_recorded="$(kubectl -n openshell get configmap gt-bootstrap -o 'jsonpath={.data.helm}' 2>/dev/null || true)"
if [ -z "${GT_K8S_FORCE_HELM:-}" ] && [ "$helm_recorded" = "$helm_marker" ] &&
  helm_release_deployed; then
  say "OpenShell chart $CHART_VERSION and its values are unchanged; not running helm (every helm upgrade regenerates the gateway's certificates)"
else
  say "installing OpenShell chart $CHART_VERSION"
  helm upgrade --install openshell oci://ghcr.io/nvidia/openshell/helm-chart --version "$CHART_VERSION" \
    --namespace openshell --create-namespace -f "$VALUES" --wait --timeout 7m
  kubectl -n openshell create configmap gt-bootstrap --from-literal="helm=$helm_marker" --dry-run=client -o yaml |
    kubectl apply -f -
fi
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

# gateway_healthy reads the status gateway info reports. A word match would call
# a gateway healthy on a line like "last check: the certificate is not healthy",
# so only the Status field counts and only when it is exactly healthy.
gateway_healthy() { # gateway_healthy INFO_OUTPUT
  local status
  status="$(sed -n 's/^[[:space:]]*Status:[[:space:]]*\([^[:space:]]*\).*/\1/p' <<<"$1")"
  [ "$status" = healthy ]
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
  if gateway_healthy "$info"; then healthy=1; break; fi
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
  # An entry this run added where none existed and that never turned healthy
  # (the town health check failed) does not stay behind. A failure of only the
  # active-gateway restore leaves the working entry in place.
  if [ "$town_entry_before" = 0 ] && [ "$town_unverified" = 1 ] && town_gateway_registered; then
    town_openshell gateway remove "$GATEWAY_NAME" >/dev/null 2>&1 ||
      warn "the unverified $GATEWAY_NAME entry could not be removed from $TOWN_CONFIG; run: openshell gateway remove $GATEWAY_NAME"
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
  # The export above can land between two rewrites of the secrets, so the town
  # copy is only accepted once the gateway is healthy under the TOWN config,
  # like the private copy: re-export and retry, bounded. A failure here leaves
  # the rollback armed, so the previous entry and active gateway come back.
  town_unverified=1
  town_tries="${GT_K8S_CERT_TRIES:-30}"
  until town_info="$(town_openshell -g "$GATEWAY_NAME" gateway info 2>&1 || true)"; gateway_healthy "$town_info"; do
    town_tries=$((town_tries - 1))
    [ "$town_tries" -gt 0 ] ||
      die "$GATEWAY_NAME is not healthy under $TOWN_CONFIG: $(tr '\n' ' ' <<<"$town_info" | tr -s ' ' | cut -c1-160)"
    say "town copy not healthy yet; re-exporting its client certificates ($town_tries tries left)"
    export_certs "$TOWN_CONFIG/gateways/$GATEWAY_NAME/mtls"
    sleep "$interval"
  done
  town_unverified=0
  town_rollback_pending=0
  say "registered $GATEWAY_NAME in $TOWN_CONFIG (active gateway: $(town_active_gateway))"
fi

say "ready: openshell -g $GATEWAY_NAME ..."
say "export XDG_CONFIG_HOME=$XDG_CONFIG_HOME KUBECONFIG=$KUBECONFIG"
