#!/usr/bin/env bash
# The PRD's first proof step (#1), throwaway: can a k0smotron control plane on kine,
# with CAPD machines, be pivoted to self-managed and recovered through a break-glass
# controller?
#
#   1. bootstrap: kind as the Bootstrap cluster, the providers, PostgreSQL for kine, the
#      cluster; the PKI copied out of kind.
#   2. pivot: the providers on the target, `clusterctl move`, kind deleted; the target
#      then creates a second machine itself.
#   3. recover: every machine container removed (the datastore survives), a new kind, a
#      break-glass k0s against the datastore, `move` to kind, new machines, `move` back.
#
# Runs on a Linux host with Docker, whose container IPs it can reach. Needs kind,
# clusterctl (the core version), kubectl, envsubst and jq. GITHUB_TOKEN, when set,
# lifts clusterctl's GitHub rate limit.
#
# usage: proof/proof.sh [all|bootstrap|pivot|recover|dump|clean]
set -euo pipefail

CAPI_VERSION=v1.14.4
K0SMOTRON_VERSION=v2.3.0
K0S_VERSION=v1.36.5+k0s.0
POSTGRES_IMAGE=postgres:17.6

DIR=$(cd "$(dirname "$0")" && pwd)
WORK=${WORK:-$DIR/.work}
KIND_NAME=selfhostctl-proof
KIND_KUBECONFIG=$WORK/kind.kubeconfig
TARGET_KUBECONFIG=$WORK/target.kubeconfig
BG_KUBECONFIG=$WORK/break-glass.kubeconfig
DB=selfhostctl-proof-db
BG=selfhostctl-proof-break-glass
BG_PORT=16443
NS=mgmt
CLUSTER=mgmt

# Proof-only values: nothing here is a real secret.
KINE_PASSWORD=kine-proof
KINE_DATASOURCE="postgres://kine:$KINE_PASSWORD@$DB:5432/kine?sslmode=disable"
ENCRYPTION_KEY=$(printf 'selfhostctl-proof-key-32-bytes!!' | base64)

log() { printf '\n=== %s: %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
kk() { kubectl --kubeconfig "$KIND_KUBECONFIG" "$@"; }
kt() { kubectl --kubeconfig "$TARGET_KUBECONFIG" "$@"; }
kb() { kubectl --kubeconfig "$BG_KUBECONFIG" "$@"; }

# until <timeout seconds> <description> <command...>
until_ok() {
  local timeout=$1 what=$2; shift 2
  local end=$((SECONDS + timeout))
  until "$@" >/dev/null 2>&1; do
    ((SECONDS < end)) || die "$what: not after ${timeout}s"
    sleep 5
  done
}

need() {
  for tool in kind clusterctl kubectl envsubst jq docker; do
    command -v "$tool" >/dev/null || die "$tool not found"
  done
  [[ $(clusterctl version -o short) == "$CAPI_VERSION" ]] || die "clusterctl must be $CAPI_VERSION"
  mkdir -p "$WORK"
}

kind_up() {
  log "Bootstrap cluster (kind)"
  if ! kind get clusters | grep -qx "$KIND_NAME"; then
    kind create cluster --name "$KIND_NAME" --config "$DIR/kind.yaml" --kubeconfig "$KIND_KUBECONFIG" --wait 2m
  fi
  kind get kubeconfig --name "$KIND_NAME" >"$KIND_KUBECONFIG"
}

kind_down() {
  log "Bootstrap cluster deleted"
  kind delete cluster --name "$KIND_NAME"
}

# providers <kubeconfig>
providers() {
  log "providers on $(basename "$1")"
  if kubectl --kubeconfig "$1" get providers.clusterctl.cluster.x-k8s.io -A 2>/dev/null | grep -q cluster-api; then
    echo "already installed"
    return
  fi
  clusterctl init --kubeconfig "$1" \
    --core "cluster-api:$CAPI_VERSION" \
    --bootstrap "k0sproject-k0smotron:$K0SMOTRON_VERSION" \
    --control-plane "k0sproject-k0smotron:$K0SMOTRON_VERSION" \
    --infrastructure "docker:$CAPI_VERSION" \
    --wait-providers
}

provider_versions() {
  kubectl --kubeconfig "$1" get providers.clusterctl.cluster.x-k8s.io -A \
    -o jsonpath='{range .items[*]}{.metadata.name} {.version}{"\n"}{end}' | sort
}

# The gate the tool will enforce before every move.
providers_match() {
  local a b
  a=$(provider_versions "$1") b=$(provider_versions "$2")
  [[ -n $a && $a == "$b" ]] || { printf '%s\n---\n%s\n' "$a" "$b"; die "providers differ"; }
  echo "providers match"
}

datastore() {
  log "PostgreSQL for kine"
  if ! docker container inspect "$DB" >/dev/null 2>&1; then
    docker run -d --name "$DB" --network kind \
      -e POSTGRES_USER=kine -e POSTGRES_PASSWORD="$KINE_PASSWORD" -e POSTGRES_DB=kine \
      "$POSTGRES_IMAGE" >/dev/null
  fi
  until_ok 60 "postgres ready" docker exec "$DB" pg_isready -U kine
}

apply_cluster() {
  log "cluster applied to kind"
  REPLICAS=1 K0S_VERSION=$K0S_VERSION KINE_DATASOURCE=$KINE_DATASOURCE ENCRYPTION_KEY=$ENCRYPTION_KEY \
    envsubst '${REPLICAS} ${K0S_VERSION} ${KINE_DATASOURCE} ${ENCRYPTION_KEY}' <"$DIR/cluster.yaml" \
    | kk apply -f -
}

# ready <kubeconfig of the cluster that holds the objects>
ready() {
  local k=$1 replicas
  log "waiting for the control plane ($(basename "$k"))"
  kubectl --kubeconfig "$k" -n $NS wait cluster/$CLUSTER --for=condition=ControlPlaneAvailable --timeout=20m
  replicas=$(kubectl --kubeconfig "$k" -n $NS get k0scontrolplane $CLUSTER -o jsonpath='{.spec.replicas}')
  kubectl --kubeconfig "$k" -n $NS wait k0scontrolplane/$CLUSTER --for=jsonpath='{.status.readyReplicas}'="$replicas" --timeout=20m
  kubectl --kubeconfig "$k" -n $NS wait machines.cluster.x-k8s.io --all --for=condition=NodeHealthy --timeout=15m
  kubectl --kubeconfig "$k" -n $NS get cluster,k0scontrolplane,machines,dockermachines
}

kubeconfig_from() {
  clusterctl --kubeconfig "$1" get kubeconfig $CLUSTER -n $NS >"$TARGET_KUBECONFIG.tmp"
  mv "$TARGET_KUBECONFIG.tmp" "$TARGET_KUBECONFIG"
  until_ok 300 "target API" kt get --raw /readyz
  kt get nodes -o wide
}

# The PKI, while kind holds it as Secrets: what the break-glass controller needs.
pki_export() {
  log "PKI copied out of kind"
  mkdir -p "$WORK/pki"
  local suffix crt key
  for pair in ca:ca.crt:ca.key sa:sa.pub:sa.key proxy:front-proxy-ca.crt:front-proxy-ca.key; do
    IFS=: read -r suffix crt key <<<"$pair"
    kk -n $NS get secret "$CLUSTER-$suffix" -o jsonpath='{.data.tls\.crt}' | base64 -d >"$WORK/pki/$crt"
    kk -n $NS get secret "$CLUSTER-$suffix" -o jsonpath='{.data.tls\.key}' | base64 -d >"$WORK/pki/$key"
    [[ -s $WORK/pki/$crt && -s $WORK/pki/$key ]] || die "secret $CLUSTER-$suffix incomplete"
  done
  kk -n $NS get cluster $CLUSTER -o jsonpath='{.spec.controlPlaneEndpoint.host}' >"$WORK/endpoint"
  ls -l "$WORK/pki"
}

wait_providers_running() {
  log "providers running on $(basename "$1")"
  local ns
  for ns in capi-system capd-system k0smotron; do
    until_ok 900 "deployments in $ns" kubectl --kubeconfig "$1" -n "$ns" wait deployment --all --for=condition=Available --timeout=10s
  done
}

bootstrap() {
  need
  kind_up
  providers "$KIND_KUBECONFIG"
  datastore
  apply_cluster
  ready "$KIND_KUBECONFIG"
  pki_export
  kubeconfig_from "$KIND_KUBECONFIG"
}

pivot() {
  need
  providers "$TARGET_KUBECONFIG"
  providers_match "$KIND_KUBECONFIG" "$TARGET_KUBECONFIG"
  log "move kind -> target"
  clusterctl move --kubeconfig "$KIND_KUBECONFIG" -n $NS --to-kubeconfig "$TARGET_KUBECONFIG" --dry-run
  clusterctl move --kubeconfig "$KIND_KUBECONFIG" -n $NS --to-kubeconfig "$TARGET_KUBECONFIG"
  ready "$TARGET_KUBECONFIG"
  kind_down
  # The proof that the target manages itself: it creates a machine of its own.
  log "self-managed: scale the control plane to 2 from inside the target"
  kt -n $NS patch k0scontrolplane $CLUSTER --type=merge -p '{"spec":{"replicas":2}}'
  ready "$TARGET_KUBECONFIG"
  kt get nodes -o wide
}

machine_containers() {
  docker ps -a --filter "label=io.x-k8s.kind.cluster=$CLUSTER" --format '{{.Names}} {{.Label "io.x-k8s.kind.role"}}' \
    | awk '$2 != "external-load-balancer" {print $1}'
}

outage() {
  log "outage: every machine container removed"
  local c
  for c in $(machine_containers); do docker rm -f -v "$c"; done
  [[ -z $(machine_containers) ]] || die "machines left"
  if kt get --raw /readyz --request-timeout=5s >/dev/null 2>&1; then die "target API still answers"; fi
  docker exec "$DB" psql -U kine -d kine -tAc 'select count(*) from kine' | sed 's/^/kine rows: /'
}

break_glass() {
  log "break-glass controller against the datastore"
  local endpoint
  endpoint=$(cat "$WORK/endpoint")
  if ! docker container inspect "$BG" >/dev/null 2>&1; then
    docker create --name "$BG" --hostname "$BG" --privileged --network kind \
      -p 127.0.0.1:$BG_PORT:6443 -v /var/lib/k0s \
      "docker.io/k0sproject/k0s:${K0S_VERSION/+/-}" \
      k0s controller --config=/etc/k0s/k0s.yaml --enable-dynamic-config >/dev/null
    local stage=$WORK/break-glass
    rm -rf "$stage" && mkdir -p "$stage/var/lib/k0s/pki" "$stage/etc/k0s"
    cp "$WORK"/pki/* "$stage/var/lib/k0s/pki/"
    # The same encryption config as the machines', from the key (env in the real tool).
    jq -n --arg key "$ENCRYPTION_KEY" '{
      apiVersion: "apiserver.config.k8s.io/v1", kind: "EncryptionConfiguration",
      resources: [{resources: ["secrets"], providers: [{aescbc: {keys: [{name: "key1", secret: $key}]}}, {identity: {}}]}]
      }' >"$stage/etc/k0s/encryption-config.yaml"
    jq -n --arg ds "$KINE_DATASOURCE" --arg ep "$endpoint" '{
      apiVersion: "k0s.k0sproject.io/v1beta1", kind: "ClusterConfig", metadata: {name: "k0s"},
      spec: {
        api: {externalAddress: $ep, sans: ["127.0.0.1", "localhost"],
              extraArgs: {"anonymous-auth": "true", "encryption-provider-config": "/etc/k0s/encryption-config.yaml"}},
        storage: {type: "kine", kine: {dataSource: $ds}},
        network: {podCIDR: "10.244.0.0/16", serviceCIDR: "10.96.0.0/12"}
      }}' >"$stage/etc/k0s/k0s.yaml"
    tar -C "$stage" -cf - . | docker cp - "$BG:/"
    docker start "$BG" >/dev/null
  fi
  until_ok 300 "break-glass admin.conf" docker exec "$BG" test -s /var/lib/k0s/pki/admin.conf
  docker exec "$BG" cat /var/lib/k0s/pki/admin.conf \
    | sed "s#https://localhost:6443#https://127.0.0.1:$BG_PORT#" >"$BG_KUBECONFIG"
  until_ok 300 "break-glass API" kb get --raw /readyz
  kb -n $NS get cluster,k0scontrolplane,machines
}

recover_prepare() {
  log "recover: webhooks saved and removed, old Nodes deleted"
  if [[ ! -s $WORK/webhooks.json ]]; then
    kb get validatingwebhookconfigurations,mutatingwebhookconfigurations -l cluster.x-k8s.io/provider -o json \
      | jq '{apiVersion: "v1", kind: "List", items: [.items[] | del(.metadata.uid, .metadata.resourceVersion,
              .metadata.creationTimestamp, .metadata.generation, .metadata.managedFields,
              .metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"])]}' >"$WORK/webhooks.json.tmp"
    mv "$WORK/webhooks.json.tmp" "$WORK/webhooks.json"
  fi
  jq -r '.items[].metadata.name' "$WORK/webhooks.json"
  kb delete validatingwebhookconfigurations,mutatingwebhookconfigurations -l cluster.x-k8s.io/provider --ignore-not-found
  kb delete nodes --all
}

recover() {
  need
  outage
  kind_up
  providers "$KIND_KUBECONFIG"
  break_glass
  recover_prepare
  log "move break-glass -> kind"
  clusterctl move --kubeconfig "$BG_KUBECONFIG" -n $NS --to-kubeconfig "$KIND_KUBECONFIG" --dry-run
  clusterctl move --kubeconfig "$BG_KUBECONFIG" -n $NS --to-kubeconfig "$KIND_KUBECONFIG"
  log "break-glass controller deleted"
  docker rm -f -v "$BG"
  log "recover: old Machines deleted, so k0smotron creates new ones against the datastore"
  kk -n $NS delete machines.cluster.x-k8s.io --all --wait=false
  ready "$KIND_KUBECONFIG"
  kubeconfig_from "$KIND_KUBECONFIG"
  wait_providers_running "$TARGET_KUBECONFIG"
  providers_match "$KIND_KUBECONFIG" "$TARGET_KUBECONFIG"
  log "move kind -> target"
  clusterctl move --kubeconfig "$KIND_KUBECONFIG" -n $NS --to-kubeconfig "$TARGET_KUBECONFIG"
  log "recover: webhooks restored, the break-glass controller's leftovers removed"
  kt apply -f "$WORK/webhooks.json"
  kt delete controlnodes.autopilot.k0sproject.io "$BG" --ignore-not-found
  kt -n kube-node-lease delete lease "k0s-ctrl-$BG" --ignore-not-found
  ready "$TARGET_KUBECONFIG"
  kind_down
  # Self-managed again: the target replaces a machine on its own.
  log "self-managed again: one Machine deleted from inside the target, and replaced"
  local victim
  victim=$(kt -n $NS get machines.cluster.x-k8s.io -o jsonpath='{.items[0].metadata.name}')
  kt -n $NS delete machine.cluster.x-k8s.io "$victim" --wait=false
  until_ok 600 "$victim gone" bash -c "! kubectl --kubeconfig '$TARGET_KUBECONFIG' -n $NS get machine.cluster.x-k8s.io '$victim'"
  ready "$TARGET_KUBECONFIG"
  kt get nodes -o wide
  log "PROOF PASSED"
}

dump() {
  set +e
  local k
  for k in "$KIND_KUBECONFIG" "$TARGET_KUBECONFIG" "$BG_KUBECONFIG"; do
    [[ -s $k ]] || continue
    echo "##### $(basename "$k")"
    kubectl --kubeconfig "$k" --request-timeout=10s get nodes -o wide
    kubectl --kubeconfig "$k" --request-timeout=10s get pods -A -o wide
    kubectl --kubeconfig "$k" --request-timeout=10s -n $NS get cluster,k0scontrolplane,machines,dockermachines,dockercluster -o yaml
    for ns in capi-system capd-system k0smotron; do
      for d in $(kubectl --kubeconfig "$k" --request-timeout=10s -n "$ns" get deploy -o name); do
        echo "--- logs $ns/$d"
        kubectl --kubeconfig "$k" --request-timeout=10s -n "$ns" logs "$d" --all-containers --tail=150
      done
    done
  done
  echo "##### docker"
  docker ps -a
  for c in $(docker ps -a --filter "label=io.x-k8s.kind.cluster=$CLUSTER" --format '{{.Names}}') "$BG"; do
    echo "--- $c"
    docker logs --tail 100 "$c" 2>&1
    docker exec "$c" sh -c 'journalctl -u k0scontroller --no-pager -n 150; cat /var/log/cloud-init-output.log 2>/dev/null | tail -50' 2>&1
  done
}

clean() {
  set +e
  kind delete cluster --name "$KIND_NAME"
  docker rm -f -v "$BG" "$DB" $(docker ps -aq --filter "label=io.x-k8s.kind.cluster=$CLUSTER") 2>/dev/null
  rm -rf "$WORK"
}

case ${1:-all} in
  all) bootstrap; pivot; recover ;;
  bootstrap | pivot | recover | dump | clean) "$1" ;;
  *) die "usage: $0 [all|bootstrap|pivot|recover|dump|clean]" ;;
esac
