# Design

How selfhostctl is built, and why. Change it when the design moves. Requirements: the PRD (#1). Terms: `GLOSSARY.md`.

- [Libraries](#libraries)
- [Bootstrap cluster](#bootstrap-cluster)
- [Lifecycle state](#lifecycle-state)
- [GitOps](#gitops)
- [Recovery](#recovery)

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

## Recovery

Agreed 2026-11-09, for #11 and #12. Profile `k0smotron-kine`.

**State:** `locate` also looks at the Break-glass controller's container (`selfhostctl-<name>-break-glass`).

| Location | Derived from |
| --- | --- |
| `break-glass` | the Break-glass controller holds the Cluster, the Bootstrap cluster doesn't |
| `split` | both hold it: a move between them stopped halfway (also the pivot's case) |

- **A Recovery is in progress** while the Break-glass controller's container exists or the Bootstrap cluster holds the saved webhooks. `bootstrap` and `pivot` are then blocked ("run `selfhostctl recover`"), and `status` lists `recover`'s phases.

**`outage`, the first phase:**

| Situation | `outage` |
| --- | --- |
| the kubeconfig's cluster doesn't answer (not: refuses its credentials) | done: the Recovery starts |
| a Recovery is in progress | done |
| anything else, e.g. the Management cluster answers | blocked: nothing to recover |

**Phases (#11):** `outage`, `bootstrap-cluster`, `host`, `providers`, `before-apply`, `break-glass`, `leases`, `webhooks`, `suspend-gitops`, `pause-children`, `delete-nodes`, `move-to-bootstrap`, `delete-break-glass`. Every phase from `break-glass` to `delete-nodes` is done ("moved") once the Bootstrap cluster holds the objects.

**Proof of the outage** (`leases`, before anything changes in the datastore). `outage` only sees that the API doesn't answer from here; a cut-off VPN or API proxy looks the same, and the steps after it would then change a live cluster.

| | Choice |
| --- | --- |
| Evidence | the datastore itself: every live kube-apiserver renews its identity lease (`kube-system`, label `apiserver.kubernetes.io/identity`), every k0s controller and kubelet its lease in `kube-node-lease`, about every 10 s |
| Gate | blocked while any lease but the Break-glass controller's own (by hostname, `k0s-ctrl-<container>`) was renewed within 2 minutes: "the datastore shows a live cluster" |
| Clock | compared with the Break-glass controller's own renewal, not the operator's clock |
| Cost | the Break-glass controller runs against the datastore before the proof; it writes only its own leases and registration there |
| Not chosen | a confirmation flag per run (only the operator's belief); an infrastructure check (not generic; an Infrastructure adapter may add one, #13) |

| Decision | Choice |
| --- | --- |
| Inputs | the Recovery bundle, the kine data source and the encryption key, from the variables `recovery` names. Checked by `recover` itself: missing → usage error |
| `host` | Docker's memory (`Info`) at least 4 GiB; `fs.inotify.max_user_watches` ≥ 524288 and `max_user_instances` ≥ 512 (kind's known-issues values), read with a Docker exec in the Bootstrap cluster's container: the Docker API, no os/exec |
| Break-glass version | `spec.version` of the Cluster's `K0sControlPlane` in `cluster.manifests`. Once its API answers: the live `K0sControlPlane`'s; a different one → the container is recreated at it |
| Break-glass args | `controller --config … --enable-dynamic-config`, `--disable-components` from the manifest's `args` plus `konnectivity-server`. No worker. Other `args` are ignored |
| Dynamic config | always on: k0smotron always passes it (v2.3.0). The local config is `api`, `storage` and the network CIDRs; the rest comes from the datastore |
| Local config | `api.extraArgs` and `storage` from the manifest, with `storage.kine.dataSource` from the environment; `network.podCIDR`, `serviceCIDR`. Fixed: `externalAddress` = the bundle's endpoint host, SANs `127.0.0.1` and `localhost`, the API published on localhost only |
| PKI | the bundle's CA, service-account key and front-proxy CA, written to `/var/lib/k0s/pki` before the container starts |
| Encryption | at the path in `api.extraArgs.encryption-provider-config`: `aescbc` with key `key1`, then `identity`. The machines' config must use that format |
| Saved webhooks | ConfigMap `kube-system/selfhostctl-recovery-webhooks` in the Bootstrap cluster, key `webhooks.json`: a `List` of the Validating and MutatingWebhookConfigurations labelled `cluster.x-k8s.io/provider`, without uid, resourceVersion and managedFields. Saved once, never re-read; gone with the Bootstrap cluster. Not in the cluster's namespace: `move` would carry it |
| GitOps | through the seam (#gitops): suspended on the Break-glass controller. No `gitops` in the config: done |
| Children | every `Cluster` outside the Management cluster's namespace: `spec.paused: true` and the marker `selfhostctl.io/paused`. One the operator paused is left alone and stays paused |
| Nodes | every Node on the Break-glass controller is deleted: a new machine with a reused IP would match an old one |
| Move | Break-glass controller → Bootstrap cluster, behind the pivot's gates; dry run first; a split resumes |
| `status` | warns for every suspended GitOps object and every paused `Cluster`, read from the Break-glass controller while it runs, else from the Management cluster |

