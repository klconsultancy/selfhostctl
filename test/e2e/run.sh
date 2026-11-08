#!/usr/bin/env bash
# The e2e: selfhostctl's own command line, from nothing to a Ready Management cluster
# on CAPD + k0smotron + kine, through the Recovery bundle checkpoint.
#
#   1. bootstrap stops at the recovery-bundle checkpoint (exit 4);
#   2. recovery export writes the bundle, which goes into the environment;
#   3. bootstrap runs to the end, and a third run changes nothing.
#
# Needs Docker (DOCKER_HOST or the active context) and Go; the PostgreSQL container
# for kine is test infrastructure, started here with the docker CLI.
#
# usage: test/e2e/run.sh [run|clean]
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$DIR/../.." && pwd)
WORK=$DIR/.work
BIN=$WORK/selfhostctl
CFG=$DIR/management.yaml
DB=selfhostctl-e2e-db

log() { printf '\n=== %s: %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

clean() {
  set +e
  docker rm -f -v selfhostctl-mgmt-bootstrap "$DB" >/dev/null 2>&1
  docker ps -aq --filter label=io.x-k8s.kind.cluster=mgmt | xargs -r docker rm -f -v >/dev/null 2>&1
  rm -rf "$WORK"
}

# expect <code> <command...>: runs it, and fails unless it exits with code.
expect() {
  local want=$1 got=0; shift
  "$@" || got=$?
  [[ $got == "$want" ]] || die "$* exited $got, want $want"
}

run() {
  mkdir -p "$WORK"
  log "build"
  (cd "$ROOT" && go build -o "$BIN" ./cmd/selfhostctl)

  export E2E_ENCRYPTION_KEY=$(head -c 32 /dev/urandom | base64)
  export E2E_KINE_DATASOURCE="postgres://kine:kine-e2e@$DB:5432/kine?sslmode=disable"
  unset E2E_RECOVERY_BUNDLE

  log "PostgreSQL for kine"
  docker network inspect kind >/dev/null 2>&1 || docker network create kind >/dev/null
  docker run -d --name "$DB" --network kind -e POSTGRES_USER=kine -e POSTGRES_PASSWORD=kine-e2e \
    -e POSTGRES_DB=kine postgres:17.6 >/dev/null
  until docker exec "$DB" pg_isready -U kine >/dev/null 2>&1; do sleep 2; done

  log "status before"
  "$BIN" status -f "$CFG"

  log "bootstrap: up to the recovery-bundle checkpoint"
  expect 4 "$BIN" bootstrap -f "$CFG"

  log "recovery export"
  E2E_RECOVERY_BUNDLE=$("$BIN" recovery export -f "$CFG")
  export E2E_RECOVERY_BUNDLE
  grep -q '"kind": "RecoveryBundle"' <<<"$E2E_RECOVERY_BUNDLE" || die "recovery export wrote no bundle"

  log "bootstrap: to the end"
  "$BIN" bootstrap -f "$CFG"

  log "bootstrap again: nothing runs"
  out=$("$BIN" bootstrap -f "$CFG")
  echo "$out"
  ! grep -q running <<<"$out" || die "a third bootstrap ran a phase"

  log "status after"
  out=$("$BIN" status -f "$CFG")
  echo "$out"
  ! grep -qvE '^\S+\s+DONE' <<<"$out" || die "a phase is not done"

  log "the written kubeconfig reaches the Management cluster"
  [[ $(stat -c %a "$WORK/mgmt.kubeconfig" 2>/dev/null || stat -f %Lp "$WORK/mgmt.kubeconfig") == 600 ]] || die "kubeconfig is not 0600"
  if kubectl --kubeconfig "$WORK/mgmt.kubeconfig" --request-timeout=10s get nodes -o wide; then
    :
  elif [[ $(uname) == Linux ]]; then
    die "the kubeconfig doesn't reach the cluster"
  else
    echo "(not on Linux: CAPD's load balancer address isn't reachable from this host)"
  fi
  log "E2E PASSED"
}

case ${1:-run} in
  run) clean; trap 'echo "--- containers"; docker ps -a' ERR; run ;;
  clean) clean ;;
  *) die "usage: $0 [run|clean]" ;;
esac
