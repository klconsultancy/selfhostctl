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

**Phases (#11):** `outage`, `datastore`, `bootstrap-cluster`, `host`, `providers`, `before-apply`, `break-glass`, `infrastructure-gone`, `webhooks`, `suspend-gitops`, `pause-children`, `delete-nodes`, `move-to-bootstrap`, `delete-break-glass`. Every phase from `break-glass` to `delete-nodes` is done ("moved") once the Bootstrap cluster holds the objects.

**Phases (#12), in the same `recover`:** `infrastructure`, `replace-machines`, `ready`, `move-to-management`, `restore-webhooks`, `leftovers`, `resume-gitops`, `unpause-children`, `delete-bootstrap-cluster`. The first three are done ("moved back") once the objects are in, or on their way to, the Management cluster. A Recovery ends when the Bootstrap cluster goes; `recover` then finds nothing to recover.

**Proof of the outage** (`datastore`, before anything starts on the datastore). `outage` only sees that the API doesn't answer from here; a cut-off VPN or API proxy looks the same. The Break-glass controller would then run next to a live cluster, and every later step changes it.

| | Choice |
| --- | --- |
| Evidence | the datastore itself: a live cluster writes to it every few seconds (API servers, controllers and kubelets renew their leases) |
| Gate | kine's revision (`MAX(id)` of table `kine`) read twice, 60 s apart, from the operator's host; grown → blocked: "the datastore shows a live cluster" |
| Clock | none: revisions, not timestamps |
| Datastore | Postgres only (`postgres://`, `postgresql://`); another one → the phase fails. Read with the same data source as the Break-glass controller, so it must resolve from the operator's host too |
| Once started | done while a Recovery is in progress: the Break-glass controller writes to it itself |
| Limit | machines cut off from the datastore, but not from the operator, write nothing and pass the gate. Their control plane is down (no API, controllers lose their leader leases), but the VMs still run: once the network returns, they run next to the machines `recover` replaces (#12), and their kubelets register again. Only an Infrastructure adapter's check covers this: `infrastructure-gone` (capmox, #13); with `none`, nothing does |
| Not chosen | the leases, read through the Break-glass controller (it runs on a live datastore before the proof); a confirmation flag per run, or the operator's monitoring alone (both see from the operator's side of a cut-off); an infrastructure check in the gate itself (not generic: the adapter's comes after the Break-glass controller, which holds the machines' objects) |

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
| Saved webhooks | ConfigMap `kube-system/selfhostctl-recovery-webhooks` in the Bootstrap cluster, key `webhooks.json`: a `List` of the Validating and MutatingWebhookConfigurations labelled `cluster.x-k8s.io/provider`, without uid, resourceVersion and managedFields. Saved once; `restore-webhooks` reads them back; gone with the Bootstrap cluster. Not in the cluster's namespace: `move` would carry it |
| GitOps | through the seam (#gitops): suspended on the Break-glass controller. No `gitops` in the config: done |
| Children | every `Cluster` outside the Management cluster's namespace: `spec.paused: true` and the marker `selfhostctl.io/paused`. One the operator paused is left alone and stays paused |
| Nodes | every Node on the Break-glass controller is deleted: a new machine with a reused IP would match an old one |
| Move | Break-glass controller → Bootstrap cluster, behind the pivot's gates; dry run first; a split resumes |
| `status` | warns for every suspended GitOps object and every paused `Cluster`, read from the Break-glass controller while it runs, else from the Management cluster |

**The second half (#12).** Researched against k0smotron v2.3.0 and CAPI v1.14.4 (CAPD):

| Fact | Consequence |
| --- | --- |
| CAPD doesn't recreate the container of a provisioned DockerMachine | the old Machines must be deleted |
| k0smotron's pre-terminate hook (`kcp-cleanup`) on each control-plane Machine asks the workload API, which is down: the deletion never ends, and the control plane creates no new Machine meanwhile | `recover` removes that hook from the old Machines |
| CAPI drains a Machine with a Node, and waits for its volumes, through the workload API | `machine.cluster.x-k8s.io/exclude-node-draining` and `exclude-wait-for-node-volume-detach` on the old Machines |
| With no old Machine left, k0smotron starts the first new one without a join token, from the CA secrets and `spec.k0s` (the same kine data source); later ones join through the API once the first answers | the new control plane comes up on the datastore; whether k0s does is what the e2e proves |

| Decision | Choice |
| --- | --- |
| Infrastructure adapter | `infrastructure.adapter`, optional: `none` (the default, which CAPD needs) does nothing. Asked twice: `infrastructure-gone` (a gate, through the Break-glass controller, before anything moves) and `infrastructure` (like a phase: Status and Run on the old Machines, before they are deleted). Blocked blocks `recover` |
| Old Machines | the Cluster's Machines when `replace-machines` first runs, recorded by name in the recovery ConfigMap (key `machines.json`). Only those are deleted: a re-run never touches the new ones |
| Deleting them | control plane and workers at once: the annotations above, `pre-terminate.delete.hook.machine.cluster.x-k8s.io/kcp-cleanup` removed, then deleted, waited for (10 min) |
| Other lifecycle hooks | any other `pre-drain.delete.hook…` or `pre-terminate.delete.hook…` on an old Machine blocks, naming it, before any Machine is deleted: its owner's cleanup is the operator's call. Not chosen: removing every hook (an unknown owner's cleanup would be skipped silently) |
| `ready` | the pivot's condition (#8), on the Bootstrap cluster |
| Back | the pivot's move and gates, Bootstrap cluster → Management cluster. Providers that differ: upgrade the Management cluster's first |
| Webhooks | the saved ones created again where missing |
| Leftovers | for the Break-glass controller and every old Machine: its autopilot `ControlNode` and its leases `kube-node-lease/k0s-ctrl-<name>` and `<name>`, which the removed hook would have deleted |
| GitOps | through the seam: what `suspend-gitops` marked, resumed, the root last |
| Children | each one `pause-children` marked, unpaused (marker removed) once the Management cluster is healthy (as for `delete-bootstrap-cluster`) and the child's API, through Cluster API's Secret `<name>-kubeconfig`, answers with every Node Ready. One that doesn't stays paused: blocked after 10 min, a re-run tries again |
| Bootstrap cluster | deleted under the pivot's stability conditions (#9) |

**Adapter `capmox` (#13).** Researched against CAPMOX v0.9.2-kl.1:

| Fact | Consequence |
| --- | --- |
| A failure domain is a zone (`ProxmoxCluster.spec.zoneConfig[]`, each with `nodes[]`), not a Proxmox node. CAPMOX builds `status.failureDomains` from them once the ProxmoxCluster's IPAM is ready; a Machine created before that lands in the default zone | with zones, `infrastructure` waits for the Cluster's failure domains (10 min) |
| Deleting a VM, CAPMOX asks one node first: `status.proxmoxNode`, else the node it cloned from. Unreachable → the deletion hangs. Then it checks the VMID cluster-wide: free → done | `infrastructure` points an old ProxmoxMachine whose node is unset or not online at the first online node; one already online is kept |
| A moved ProxmoxMachine with a VMID is never cloned again. If Proxmox still has the VM, on any node, CAPMOX takes it over and starts it | `infrastructure-gone` blocks while any old VM exists, running or not: start them, or destroy them to recover |
| Proxmox reuses a freed VMID | an old VMID held by a VM with another name blocks too: CAPMOX would take it over and delete it |
| CAPMOX's credentials: the Secret in `spec.credentialsRef` (`url`, `token`, `secret`, `insecure`, `root_ca`), which moves with the cluster | the adapter reads Proxmox with that Secret: no environment variables |

| Decision | Choice |
| --- | --- |
| Config | `infrastructure: {adapter: capmox}`, nothing else |
| Proxmox API | GETs only (`cluster/resources?type=vm`, `nodes`), with a plain HTTP client: a read-only token is enough. Deleting VMs stays CAPMOX's job |
| TLS | CAPMOX's rule: `insecure` unset or true-ish → no certificate check; `root_ca` added to the system roots |
| Blocked | no `credentialsRef`, its Secret missing or incomplete, the API not answering, no node online, or an old VM still there |
| Skipped | ProxmoxMachines without `virtualMachineID`: they never had a VM |
| Tests | against a fake Proxmox API; no e2e (CI has no Proxmox). The first real run is a planned exercise on a real cluster |
| Not chosen | the node of the Machine's failure domain (a zone has several nodes, and the dead one may be among them); an outage check in `datastore` (it runs before the machines' objects are readable) |

