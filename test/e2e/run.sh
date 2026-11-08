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
#   6. without its kubeconfig, bootstrap refuses and creates no Bootstrap cluster.
#
# Needs Docker, kubectl, and a host that reaches CAPD's load balancer (Linux; on macOS,
# run it inside the Docker VM). The PostgreSQL container for kine is test
# infrastructure, started here with the docker CLI. SELFHOSTCTL_BIN skips the build.
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

log() { printf '\n=== %s: %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
k() { kubectl --kubeconfig "$KC" --request-timeout=20s "$@"; }

clean() {
  set +e
  docker rm -f -v "$BOOT" "$DB" >/dev/null 2>&1
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

  log "E2E PASSED"
}

dump() {
  set +e
  echo "--- containers"
  docker ps -a
  [[ -s $KC ]] || return
  k get clusters,k0scontrolplanes,machines -A
  k get nodes -o wide
  k get pods -A -o wide | grep -v Running
  k get events -A --field-selector type=Warning | tail -30
}

case ${1:-run} in
  # die exits explicitly, which no ERR trap sees: dump on any failing exit.
  run) clean; trap 'code=$?; ((code == 0)) || dump' EXIT; run ;;
  clean) clean ;;
  *) die "usage: $0 [run|clean]" ;;
esac
