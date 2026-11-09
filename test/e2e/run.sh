#!/usr/bin/env bash
# The e2e: selfhostctl's own command line on CAPD + k0smotron + kine, from nothing to a
# self-managed Management cluster (docs/design.md#lifecycle-state).
#
#   1. without a kubeconfig or --first, bootstrap refuses to start (fail closed);
#   2. bootstrap --first stops at the recovery-bundle checkpoint; recovery export;
#   3. bootstrap pivots, then deletes the Bootstrap cluster once the stable window
#      has passed (it stops at that checkpoint until then);
#   4. bootstrap again runs nothing, even without the Recovery bundle;
#   5. the Management cluster scales itself to two machines;
#   6. without its kubeconfig, bootstrap refuses and creates no Bootstrap cluster;
#   7. recover refuses while the Management cluster answers;
#   8. a Management cluster only cut off from here (its kubeconfig points nowhere):
#      recover stops at the leases, the datastore's proof that it lives, and has
#      changed nothing in it;
#   9. every machine container removed (the datastore survives): recover runs a
#      Break-glass controller against the datastore and moves the objects into a new
#      Bootstrap cluster; again, it runs nothing, and bootstrap is blocked meanwhile.
#
# Flux comes from the cluster's own definition. Step 3's gitops phase gives it its
# credential; Flux then syncs a ConfigMap from a git server in a container, and
# `gitops sync` again writes nothing.
#
# Needs Docker, kubectl, and a host that reaches CAPD's load balancer (Linux; on macOS,
# run it inside the Docker VM), ssh-keygen and git. The PostgreSQL container for kine and
# the git server are test infrastructure, started here with the docker CLI.
# SELFHOSTCTL_BIN skips the build.
#
# usage: test/e2e/run.sh [run|clean]
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$DIR/../.." && pwd)
WORK=$DIR/.work
BIN=${SELFHOSTCTL_BIN:-$WORK/selfhostctl}
CFG=$DIR/management.yaml
KC=$WORK/mgmt.kubeconfig
DB=selfhostctl-e2e-db
BOOT=selfhostctl-mgmt-bootstrap
BG=selfhostctl-mgmt-break-glass
GIT=selfhostctl-e2e-git

log() { printf '\n=== %s: %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
k() { kubectl --kubeconfig "$KC" --request-timeout=20s "$@"; }

clean() {
  set +e
  docker rm -f -v "$BOOT" "$BG" "$DB" "$GIT" >/dev/null 2>&1
  docker ps -aq --filter label=io.x-k8s.kind.cluster=mgmt | xargs -r docker rm -f -v >/dev/null 2>&1
  rm -rf "$WORK"
}

# expect <code> <command...>: runs it, and fails unless it exits with code.
expect() {
  local want=$1 got=0; shift
  "$@" || got=$?
  [[ $got == "$want" ]] || die "$* exited $got, want $want"
}

bootstrap_exists() { docker container inspect "$BOOT" >/dev/null 2>&1; }

# The Management cluster's machine containers; CAPD's load balancer isn't one.
machine_containers() {
  docker ps -a --filter label=io.x-k8s.kind.cluster=mgmt --format '{{.Names}} {{.Label "io.x-k8s.kind.role"}}' \
    | awk '$2 != "external-load-balancer" {print $1}'
}

# kb runs kubectl inside the Bootstrap cluster's container.
kb() { docker exec "$BOOT" k0s kubectl "$@"; }

# git_server: an SSH git server on the kind network with one repo, fleet.git, whose
# clusters/mgmt holds a ConfigMap. Writes the client key to E2E_FLUX_DEPLOY_KEY and
# the host key to .work/known_hosts.
git_server() {
  local keys=$WORK/git-keys repo=$WORK/fleet
  mkdir -p "$keys"
  ssh-keygen -q -t ed25519 -N '' -C e2e-host -f "$keys/ssh_host_ed25519_key"
  ssh-keygen -q -t ed25519 -N '' -C e2e-flux -f "$keys/flux"
  cp "$keys/flux.pub" "$keys/authorized_keys"
  printf '%s %s\n' "$GIT" "$(cut -d' ' -f1-2 "$keys/ssh_host_ed25519_key.pub")" >"$WORK/known_hosts"
  E2E_FLUX_DEPLOY_KEY=$(cat "$keys/flux")
  export E2E_FLUX_DEPLOY_KEY

  mkdir -p "$repo/clusters/mgmt"
  cat >"$repo/clusters/mgmt/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: [configmap.yaml]
YAML
  cat >"$repo/clusters/mgmt/configmap.yaml" <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: e2e-flux-synced
  namespace: default
data:
  from: fleet.git
YAML
  git -C "$repo" init -q -b main
  git -C "$repo" add .
  git -C "$repo" -c user.name=e2e -c user.email=e2e@example.invalid commit -q -m fleet
  git clone -q --bare "$repo" "$WORK/fleet.git"

  docker create --name "$GIT" --network kind alpine:3.22 sh -c '
    set -e
    apk add --no-cache -q git openssh-server
    adduser -D -s /usr/bin/git-shell git
    sed -i "s/^git:!/git:*/" /etc/shadow
    install -d -o git -m 700 /home/git/.ssh
    install -o git -m 600 /keys/authorized_keys /home/git/.ssh/authorized_keys
    install -m 600 /keys/ssh_host_ed25519_key /etc/ssh/ssh_host_ed25519_key
    chown -R git /srv/fleet.git
    exec /usr/sbin/sshd -D -e -o HostKey=/etc/ssh/ssh_host_ed25519_key -o PasswordAuthentication=no
  ' >/dev/null
  docker cp "$keys" "$GIT:/keys"
  docker cp "$WORK/fleet.git" "$GIT:/srv/fleet.git"
  docker start "$GIT" >/dev/null
}

run() {
  mkdir -p "$WORK"
  if [[ -z ${SELFHOSTCTL_BIN:-} ]]; then
    log "build"
    (cd "$ROOT" && go build -o "$BIN" ./cmd/selfhostctl)
  fi

  E2E_ENCRYPTION_KEY=$(head -c 32 /dev/urandom | base64)
  export E2E_ENCRYPTION_KEY
  export E2E_KINE_DATASOURCE="postgres://kine:kine-e2e@$DB:5432/kine?sslmode=disable"
  unset E2E_RECOVERY_BUNDLE

  log "PostgreSQL for kine"
  docker network inspect kind >/dev/null 2>&1 || docker network create kind >/dev/null
  docker run -d --name "$DB" --network kind -e POSTGRES_USER=kine -e POSTGRES_PASSWORD=kine-e2e \
    -e POSTGRES_DB=kine postgres:17.6 >/dev/null
  until docker exec "$DB" pg_isready -U kine >/dev/null 2>&1; do sleep 2; done

  log "a git server for Flux"
  git_server

  log "1. no kubeconfig, no --first: fail closed"
  expect 3 "$BIN" bootstrap -f "$CFG"
  ! bootstrap_exists || die "a Bootstrap cluster was created without --first"

  log "2. bootstrap --first: up to the recovery-bundle checkpoint"
  expect 4 "$BIN" bootstrap --first -f "$CFG"
  E2E_RECOVERY_BUNDLE=$("$BIN" recovery export -f "$CFG")
  export E2E_RECOVERY_BUNDLE
  grep -q '"kind": "RecoveryBundle"' <<<"$E2E_RECOVERY_BUNDLE" || die "recovery export wrote no bundle"

  log "3. bootstrap: pivot, then wait out the stable window"
  local code
  for i in $(seq 30); do
    code=0
    "$BIN" bootstrap -f "$CFG" || code=$?
    [[ $code == 0 ]] && break
    [[ $code == 4 ]] || die "bootstrap exited $code"
    ((i < 30)) || die "the Bootstrap cluster was never deleted"
    sleep 15
  done
  ! bootstrap_exists || die "the Bootstrap cluster is still there"
  k get nodes -o wide
  k -n default get configmap e2e-flux-synced >/dev/null || die "Flux didn't sync the fleet repo"
  out=$("$BIN" gitops sync -f "$CFG")
  echo "$out"
  ! grep -q running <<<"$out" || die "gitops sync wrote something on a synced cluster"

  log "4. bootstrap again, without the Recovery bundle: nothing runs"
  unset E2E_RECOVERY_BUNDLE
  out=$("$BIN" bootstrap -f "$CFG")
  echo "$out"
  ! grep -q running <<<"$out" || die "a bootstrap after the pivot ran a phase"
  ! bootstrap_exists || die "a bootstrap after the pivot created a Bootstrap cluster"
  # Captured, not piped: grep -q would stop reading early, and pipefail would count
  # the writer's SIGPIPE as a failure.
  out=$("$BIN" status -f "$CFG")
  echo "$out"
  grep -q '^location: management$' <<<"$out" || die "status doesn't say management"
  ! grep -q '^warning: .* is suspended' <<<"$out" || die "status reports a suspended Flux object"

  log "5. the Management cluster manages itself: two machines"
  k -n mgmt patch k0scontrolplane mgmt --type=merge -p '{"spec":{"replicas":2}}'
  for i in $(seq 60); do
    ready=$(k -n mgmt get k0scontrolplane mgmt -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)
    [[ $ready == 2 ]] && break
    ((i < 60)) || die "the Management cluster didn't scale itself to 2"
    sleep 10
  done
  k -n mgmt wait machines.cluster.x-k8s.io --all --for=condition=NodeHealthy --timeout=10m
  k get nodes -o wide

  log "6. without its kubeconfig: fail closed again"
  mv "$KC" "$KC.away"
  expect 3 "$BIN" bootstrap -f "$CFG"
  ! bootstrap_exists || die "a Bootstrap cluster was created next to the Management cluster"
  mv "$KC.away" "$KC"
  out=$("$BIN" recovery export -f "$CFG")
  grep -q '"kind": "RecoveryBundle"' <<<"$out" || die "recovery export from the Management cluster failed"
  E2E_RECOVERY_BUNDLE=$out
  export E2E_RECOVERY_BUNDLE

  log "7. recover while the Management cluster answers: nothing to recover"
  expect 3 "$BIN" recover -f "$CFG"
  ! bootstrap_exists || die "recover created a Bootstrap cluster next to a healthy Management cluster"

  log "8. only cut off from here: recover stops at the leases, having changed nothing"
  cp "$KC" "$KC.real"
  sed -i.bak 's#server: https://.*#server: https://127.0.0.1:9#' "$KC"
  code=0
  out=$("$BIN" recover -f "$CFG" 2>&1) || code=$?
  echo "$out"
  [[ $code == 3 ]] || die "recover against a live, cut-off cluster exited $code, want 3"
  grep -q 'leases: blocked: the datastore shows a live cluster' <<<"$out" || die "recover didn't stop at the leases"
  mv "$KC.real" "$KC"
  k get validatingwebhookconfigurations -l cluster.x-k8s.io/provider -o name | grep -q . || die "the live cluster lost its webhooks"
  [[ $(k get nodes -o name | wc -l) == 2 ]] || die "the live cluster lost Nodes"
  docker rm -f -v "$BG" "$BOOT" >/dev/null

  log "9. every machine removed: recover, through the Break-glass controller"
  local c
  for c in $(machine_containers); do docker rm -f -v "$c" >/dev/null; done
  [[ -z $(machine_containers) ]] || die "machines left"
  ! k get --raw /readyz >/dev/null 2>&1 || die "the Management cluster's API still answers"
  # The machines' leases may still be fresh: leases blocks for up to 2 minutes.
  for i in $(seq 8); do
    code=0
    out=$("$BIN" recover -f "$CFG" 2>&1) || code=$?
    echo "$out"
    [[ $code == 0 ]] && break
    { [[ $code == 3 ]] && grep -q 'leases: blocked' <<<"$out"; } || die "recover exited $code"
    ((i < 8)) || die "the machines' leases never went quiet"
    sleep 30
  done
  ! docker container inspect "$BG" >/dev/null 2>&1 || die "the Break-glass controller is still there"
  kb -n mgmt get cluster mgmt >/dev/null || die "the Cluster isn't in the Bootstrap cluster"
  kb -n mgmt get k0scontrolplanes,machines
  kb -n kube-system get configmap selfhostctl-recovery-webhooks -o jsonpath='{.data.webhooks\.json}' | grep -q WebhookConfiguration \
    || die "the providers' webhooks weren't saved"
  out=$("$BIN" recover -f "$CFG")
  echo "$out"
  ! grep -q running <<<"$out" || die "a second recover ran a phase"
  out=$("$BIN" status -f "$CFG")
  echo "$out"
  grep -q '^location: bootstrap, a Recovery is in progress$' <<<"$out" || die "status doesn't show the Recovery"
  expect 3 "$BIN" bootstrap -f "$CFG"

  log "E2E PASSED"
}

dump() {
  set +e
  echo "--- containers"
  docker ps -a
  if docker container inspect "$BG" >/dev/null 2>&1; then
    echo "--- break-glass"
    docker logs --tail 50 "$BG"
  fi
  if bootstrap_exists; then
    echo "--- bootstrap cluster"
    kb get clusters,k0scontrolplanes,machines -A
    kb get pods -A -o wide | grep -v Running
  fi
  [[ -s $KC ]] || return
  k get clusters,k0scontrolplanes,machines -A
  k get nodes -o wide
  k get pods -A -o wide | grep -v Running
  k get fluxinstances,gitrepositories,kustomizations -A
  k get events -A --field-selector type=Warning | tail -30
  docker logs --tail 30 "$GIT"
}

case ${1:-run} in
  # die exits explicitly, which no ERR trap sees: dump on any failing exit.
  run) clean; trap 'code=$?; ((code == 0)) || dump' EXIT; run ;;
  clean) clean ;;
  *) die "usage: $0 [run|clean]" ;;
esac
