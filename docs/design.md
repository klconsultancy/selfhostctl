# Design

How selfhostctl is built, and why. Change it when the design moves. Requirements: the PRD (#1). Terms: `GLOSSARY.md`.

- [Libraries](#libraries)
- [Bootstrap cluster](#bootstrap-cluster)
- [Lifecycle state](#lifecycle-state)

## Libraries

One Go module, no shell-outs (#1, #3).

| Library | For CAPI v1.14 | Rule |
| --- | --- | --- |
| `sigs.k8s.io/cluster-api` | v1.14.4 | One CAPI minor per tool release; raised by hand |
| `sigs.k8s.io/cluster-api/api` | `replace` → v1.14.4 | CAPI requires it at a placeholder version; the `replace` moves with CAPI |
| `helm.sh/helm/v4` | v4.3.1 | Newest minor: Helm patches no other |
| `k8s.io/*` | v0.37.x | Whatever the newer of CAPI and Helm requires |
| `sigs.k8s.io/controller-runtime` | v0.25.2 | Same |
| `github.com/moby/moby/client` | v0.6.2 | The Docker SDK |

- The clusterctl library runs on a newer `k8s.io`/controller-runtime than it was built with. The smoke test (`go test -tags integration ./internal/smoke/`) and the e2e check that on every PR.
- A Helm minor that no longer fits the CAPI minor comes with the release on the next CAPI minor.
- A dependency that lags behind a security fix is raised directly in `go.mod`, with the advisory named.
- `govulncheck`: every PR, and daily on `main`.
- clusterctl's `Init` adds kubeadm for an empty bootstrap or control-plane type. The tool always passes every type.

## Bootstrap cluster

One k0s container (controller and worker) through the Docker SDK. Not kind (#3).

- Why: the kind library runs the `docker` CLI.
- The Break-glass controller is a k0s container through the same code.
- Proof: kind and k0s side by side through bootstrap, pivot and recovery, with no difference (run 42495292653).
- k0s version: pinned per tool release, like the CAPI core.
- Host mounts: only what the config declares. The Docker socket only for CAPD.
- Docker endpoint: `DOCKER_HOST`, else the Docker CLI's active context, read from its files. The SDK ignores contexts.
- Open: Docker Desktop on macOS is unproven. Colima and the GitHub runners work.

## Lifecycle state

Agreed 2026-11-08, for #9 onwards.

**Problem:** every phase is judged against the Bootstrap cluster. After the pivot, `bootstrap` would start over and apply a second copy of the Cluster.

**State:** derived from live state each run, never stored. One variable, the *location* of the Management cluster's objects:

| Location | Derived from | `bootstrap` does |
| --- | --- | --- |
| `none` | no cluster holds the Cluster object | start at the first phase |
| `bootstrap` | the Bootstrap cluster holds it, unpaused | the phases up to the pivot |
| `split` | both hold it, paused (a `move` failed halfway) | resume the move |
| `management` | the Management cluster holds it and manages itself | pre-pivot phases are done ("pivoted"); no new Bootstrap cluster |

- `locate` computes it once per run, before the first phase. The phases read it; none checks for itself. `status` prints it first.
- Tests: one table, location → the expected state of every phase, on fake clusters.
- `recover` uses the same variable (#11, #12).

**Handle to the Management cluster:** the admin kubeconfig the `kubeconfig` phase wrote (`cluster.kubeconfig`). Never the Recovery bundle.

| | Admin kubeconfig | Recovery bundle |
| --- | --- | --- |
| Holds | a client certificate | the CA key: mints certificates at will |
| If it leaks | expires (Cluster API: about 1 year) | only replacing the CA helps |
| Read by | `locate`, pivot, upgrade, `gitops sync`, `recovery export` after the pivot | the `recovery-bundle` checkpoint (first bootstrap), `recover` |
| Lives | on the operator's machine, 0600 | in the secret store; in the environment only for the one command that reads it (`op run -- selfhostctl …`), never in `.envrc` or a shell profile |

- After the pivot, `recovery-bundle` is done ("pivoted"): a `bootstrap` then doesn't read the bundle.
- **Keeping the kubeconfig fresh:**

  | Situation | What happens |
  | --- | --- |
  | every `bootstrap`/`pivot` run | the `kubeconfig` phase rewrites the file from the `<cluster>-kubeconfig` Secret, on the Management cluster after the pivot, so a certificate Cluster API renewed there reaches the file |
  | the certificate expires within 30 days | `status` and the run warn |
  | expired, or refused by the API | `locate` → `unknown`, blocked even with `--first`; the reason says to renew, not to remove the file |
  | renew | `op run -- selfhostctl kubeconfig renew`: a client certificate signed by the bundle's CA, valid 30 days, as a bridge until the next run refreshes the file. The one use of the bundle outside a Recovery |

**Fail closed:**

| Situation | `bootstrap-cluster` |
| --- | --- |
| kubeconfig present, `locate` → `none` | creates the Bootstrap cluster |
| no kubeconfig | blocked: "no kubeconfig at `<path>`: copy it there, or run with `--first` if this is the cluster's first bootstrap" |
| `--first` | creates it; covers only that run |
| `--first`, and a kubeconfig that doesn't answer, has expired or is refused | refused |
| `--first`, and a known endpoint (from the manifests, e.g. a ProxmoxCluster) accepts connections | refused |

**#9 adds:** the e2e runs `bootstrap` after the pivot (nothing runs, no Bootstrap cluster appears) and without the kubeconfig (blocked). `recovery export` also works from the Management cluster.

## GitOps

Agreed 2026-11-09, for #10.

| Decision | Choice |
| --- | --- |
| GitOps required | no. A config without `gitops` is valid: the `gitops` phase is done, `gitops sync` is a usage error, `recover` doesn't suspend anything |
| Tools | one key per tool under `gitops`, `flux` the one so far (like `recovery.profile`). Another tool is a new key, not a change to `flux` |
| Seam | `recover` (#11, #12) talks to a small interface (suspend, resume, list suspended), not to Flux. Flux is its one implementation |
| Who installs Flux | the cluster's own definition (e.g. a k0s Helm extension: flux-operator and a FluxInstance). Never the tool |
| Where the phase runs | `gitops`, after `after-pivot`, before `delete-bootstrap-cluster`: the stable window covers Flux's first sync, and the Bootstrap cluster stays until then |
| Why after `move` | the credential is what lets Flux fetch. Written only once the Management cluster manages itself, so Flux never applies objects a move is about to create |
| Credential Secret | `identity` from `gitops.flux.credentialSecret.fromEnv`; `known_hosts` from the `knownHosts` file next to the config (public, in git). Never a key scan: that trusts whatever answers |
| Write guard | an empty field is refused before anything is written, so an empty value never replaces a working key. Server-side apply, dry run first: unchanged → no write |
| Root | `gitops.flux.root` `{namespace, name}`, default `flux-system/flux-system` (flux-operator's sync name). Its source is the root's `sourceRef` |
| Done | the Secret is unchanged; every FluxInstance is Ready; the source is Ready on its generation; the root is Ready and applied the source's revision |
| Run | requests a reconcile of the source (`reconcile.fluxcd.io/requestedAt`) and waits until Flux handled that request and is Ready: a fetch of the branch now, not the last good artifact. Then the same for the root |
| Source fails (e.g. authentication) | fails at once with Flux's message |
| No Flux CRDs in time | blocked: "is Flux in the cluster's definition?" |
| `gitops sync` | the `gitops` phase alone, against the live Management cluster; done → nothing is written |

**Suspend and resume** (for `recover`, #11 and #12):

| | Suspend | Resume |
| --- | --- | --- |
| Set | every Kustomization and HelmRelease in the cluster, from live state | those suspend marked (`selfhostctl.io/suspended`) |
| Order | the root first (no self-sync undoes the rest), then Kustomizations, then HelmReleases | the reverse: the root last |
| Already suspended by the operator | left alone, not marked | stays suspended |

- `status` prints a warning for every suspended Kustomization and HelmRelease.
