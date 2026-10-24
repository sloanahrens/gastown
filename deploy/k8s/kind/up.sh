#!/usr/bin/env bash
# Stand up the local Kubernetes sandbox environment (see docs/design/k8s-sandboxes.md):
# a Colima VM, a kind cluster with a fixed gateway port, a local image
# registry, the pinned agent-sandbox controller and the OpenShell chart, then
# register the gateway as `k8s` in an isolated CLI config. Safe to rerun.
set -euo pipefail
# shellcheck source=common.sh
. "$(dirname "$0")/common.sh"

for tool in colima kind kubectl helm docker openssl openshell curl; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is not on PATH (helm may live in $OUT/bin)"
done
mkdir -p "$OUT"

port_in_use() { # a listener on 127.0.0.1:$1, whoever owns it
  if command -v lsof >/dev/null 2>&1; then
    [ -n "$(lsof -nP -iTCP:"$1" -sTCP:LISTEN 2>/dev/null)" ]
  else
    (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
  fi
}

original_context="$(docker context show)"

if ! colima status -p "$PROFILE" >/dev/null 2>&1; then
  say "starting colima profile $PROFILE"
  colima start "$PROFILE" --cpu 6 --memory 16 --disk 60 --vm-type vz --runtime docker
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

say "installing agent-sandbox $AGENT_SANDBOX_VERSION"
kubectl apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$AGENT_SANDBOX_VERSION/sandbox.yaml"
kubectl -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s

say "installing OpenShell chart $CHART_VERSION"
helm upgrade --install openshell oci://ghcr.io/nvidia/openshell/helm-chart --version "$CHART_VERSION" \
  --namespace openshell --create-namespace -f "$VALUES" --wait --timeout 7m
kubectl apply -f "$HERE/gateway-nodeport.yaml"
kubectl -n openshell rollout status statefulset/openshell --timeout=300s

# Client certificates for the CLI. The key is never printed; the CLI wants PKCS8.
mtls="$XDG_CONFIG_HOME/openshell/gateways/$GATEWAY_NAME/mtls"
export_certs() {
  mkdir -p "$mtls"
  chmod 700 "$XDG_CONFIG_HOME" "$mtls"
  for key in ca.crt tls.crt tls.key; do
    kubectl -n openshell get secret openshell-client-tls -o "jsonpath={.data['${key//./\\.}']}" | base64 -d >"$mtls/$key"
    chmod 600 "$mtls/$key"
  done
  openssl pkcs8 -topk8 -nocrypt -in "$mtls/tls.key" -out "$mtls/tls.key.pk8"
  mv "$mtls/tls.key.pk8" "$mtls/tls.key"
  chmod 600 "$mtls/tls.key"
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
  export_certs
  openshell gateway list 2>/dev/null | grep -q "$GATEWAY_NAME" ||
    openshell gateway add "https://127.0.0.1:$GATEWAY_PORT" --local --name "$GATEWAY_NAME"
  info="$(openshell -g "$GATEWAY_NAME" gateway info 2>&1 || true)"
  if grep -q healthy <<<"$info"; then healthy=1; break; fi
  attempts=$((attempts - 1))
  say "gateway not healthy yet ($(tr '\n' ' ' <<<"$info" | tr -s ' ' | cut -c1-110)); re-exporting client certificates ($attempts tries left)"
  sleep "$interval"
done
[ "$healthy" = 1 ] || die "gateway $GATEWAY_NAME is not healthy after re-exporting its client certificates"
say "ready: openshell -g $GATEWAY_NAME ... with XDG_CONFIG_HOME=$XDG_CONFIG_HOME and KUBECONFIG=$KUBECONFIG"
