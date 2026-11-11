# The command line

```sh
selfhostctl bootstrap       -f management.yaml [--from <phase>] [--first]
selfhostctl pivot           -f management.yaml
selfhostctl recover         -f management.yaml [--from <phase>]   # with the Recovery bundle, e.g. via op run
selfhostctl upgrade         -f management.yaml
selfhostctl status          -f management.yaml
selfhostctl providers adopt -f management.yaml
selfhostctl gitops sync     -f management.yaml
selfhostctl kubeconfig renew -f management.yaml   # with the Recovery bundle, e.g. via op run
selfhostctl recovery export -f management.yaml > into-your-secret-store
selfhostctl version
```

`recovery export` writes the Recovery bundle to stdout as JSON: the cluster's CA, service-account key, front-proxy CA and control-plane endpoint. It is secret. Pipe it into your secret store (e.g. `| op item create ...`), never into a file in git, and give it to the tool back through the environment variable `recovery.bundleFromEnv` names.

The config is described in [config.md](config.md). Terms such as Phase, Manual checkpoint and Bootstrap cluster are defined in [GLOSSARY.md](../GLOSSARY.md).

## Phases

`bootstrap` runs its Phases in order. Each Phase judges its own state from live state alone. Nothing is stored between runs, so running the command again resumes where it stopped.

| State | Meaning | What the command does |
| --- | --- | --- |
| done | its outcome is already in place | skips it |
| next | it can run now | runs it |
| blocked | a precondition is missing that it can't fix itself | stops, with the reason |
| manual | a Manual checkpoint: only the operator can complete it | stops, with the instruction |

`--from <phase>` starts at that Phase, skipping the ones before it, e.g. to resume a step that failed halfway. `status` lists every Phase with its state and reason, without changing anything. No command ever prompts.

**Location.** Every run first works out where the Management cluster's objects live (docs/design.md#lifecycle-state): `none`, `bootstrap`, `split` (a move stopped halfway), `management`, `break-glass` (during a Recovery), or `unknown`. `status` prints it first, and whether a Recovery is in progress.

| Location | Phases before `management-providers` | Bootstrap cluster |
| --- | --- | --- |
| `none`, `bootstrap` | judged as below | created if missing |
| `split`, `management` | done: "a move is in progress" / "pivoted" | never created |
| `unknown` | — | blocked, see below |

**Fail closed.** The handle to the Management cluster is its kubeconfig (`cluster.kubeconfig`). Without one, nothing proves there is no Management cluster yet, so `bootstrap-cluster` is blocked unless you run `bootstrap --first`. `--first` covers only that run, and is refused when:
- a kubeconfig exists but its cluster doesn't answer: a network blip (run again), a lost cluster (`recover`) or a stale file (remove it);
- the endpoint known from the manifests (e.g. a ProxmoxCluster's) accepts connections.

`kubeconfig renew` writes a new admin kubeconfig signed with the Recovery bundle's CA, valid 30 days, for when the file has expired or is lost. Load the bundle for that one command only: `op run -- selfhostctl kubeconfig renew -f management.yaml`.

`pivot` runs bootstrap's phases from `management-providers`.

`gitops sync` runs the `gitops` phase alone against the live Management cluster, e.g. after a new deploy key or a moved root Kustomization. Done already: nothing is written. `status` prints a warning for every suspended Flux Kustomization and HelmRelease, and for every paused Cluster. `providers adopt` stamps a Management cluster's providers, installed some other way, with the config's variables fingerprint, after checking their versions. Run it only if they run with exactly `providers.variables`.

`bootstrap` has these Phases:

| Phase | Done when |
| --- | --- |
| `bootstrap-cluster` | The Bootstrap cluster runs: the container `selfhostctl-<name>-bootstrap` (one k0s controller and worker, at the version this release pins) exists, and its node is Ready. Its API is published on localhost only. It mounts nothing from the host unless the config declares it. |
| `providers` | The Bootstrap cluster runs exactly the declared providers, the core at this release's version, each installed with the declared variables. The tool records a fingerprint of the variables (never the values) on every Provider object as `selfhostctl.io/variables-fingerprint`. With some providers already installed, or other versions or variables, the phase is blocked and names the difference. The Bootstrap cluster holds nothing yet, so the fix is to delete its container and run again. |
| `before-apply` | Every `beforeApply` entry for `bootstrap` is in place on the Bootstrap cluster. A manifest counts as in place when a server-side dry-run apply would change nothing. A chart counts as in place when its release is deployed at the declared version, with the declared values. Running installs or upgrades only what isn't in place, in order. A release left pending by an interrupted Helm operation blocks the phase. |
| `apply` | `cluster.manifests` is applied to the Bootstrap cluster: a server-side dry-run would change nothing. |
| `ready` | The Cluster is `ControlPlaneAvailable`, its control plane has as many ready replicas as it wants, and every Machine is `NodeHealthy`. Running waits up to 30 minutes. `status` says what it still waits for. |
| `recovery-bundle` | **A Manual checkpoint.** The Recovery bundle in `recovery.bundleFromEnv` matches the cluster: the same CA, service-account key, front-proxy CA and endpoint, each private key belonging to its certificate. The variables `recovery.encryptionKeyFromEnv` and `recovery.kine.datasourceFromEnv` name are set too. Until then, the command stops with exit 4 and tells you to run `recovery export`. It warns when no `beforeApply` file uses the encryption-key variable. It is done right away when the config has no `recovery`. |
| `kubeconfig` | `cluster.kubeconfig` (mode 0600) equals Cluster API's `<cluster>-kubeconfig` Secret: from the Bootstrap cluster before the pivot, from the Management cluster after it. So it follows a renewed certificate. The note shows its expiry, with a warning within 30 days. |
| `management-providers` | The Management cluster runs exactly the declared providers, with the declared variables. Providers without a selfhostctl fingerprint: **manual**, `providers adopt`. Other versions or variables: blocked, run `upgrade`. |
| `management-before-apply` | The `beforeApply` entries for `management` are in place on the Management cluster. |
| `move` | The Cluster's namespace is on the Management cluster, unpaused. **Gates:** the providers match on both sides (versions and fingerprints; an unstamped side is **manual**, `providers adopt`), and the namespace holds no other Cluster. Running does a dry run, then the move. A move that stopped halfway (`split`) resumes, and a move that ended before unpausing is unpaused. |
| `after-pivot` | The `afterPivot` entries are in place on the Management cluster. |
| `gitops` | With `gitops`: the credential Secret holds the key from `gitops.flux.credentialSecret.fromEnv` and the `knownHosts` file, every FluxInstance is Ready, the root Kustomization's source is Ready, and the root applied its revision. Running it writes the Secret when it differs (never with an empty field), then has Flux fetch and apply now, and waits: a fresh fetch, not the last good artifact. A failing fetch fails with Flux's message. Without Flux's CRDs in time it is **blocked**: "is Flux in the cluster's definition?". Without `gitops`: done. |
| `delete-bootstrap-cluster` | No Bootstrap cluster exists. Before it is deleted, the Management cluster must be `Available` with `RemoteConnectionProbe`, have every Machine `NodeHealthy`, and have had no change (condition or new Machine) for `bootstrapCluster.deleteAfterStable`. Until then it is **manual**: "stable for X of Y, run again then". |

## recover

`recover` brings back a Management cluster whose machines are all gone, from its surviving datastore (profile `k0smotron-kine`). It reads the Recovery bundle, the kine data source and the encryption key from the variables `recovery` names; unset or empty → exit 2. Load them for this one command only: `op run -- selfhostctl recover -f management.yaml`.

While a Recovery is in progress (the Break-glass controller's container exists, or the Bootstrap cluster holds the saved webhooks), `bootstrap` and `pivot` are blocked and `status` lists `recover`'s Phases.

| Phase | Done when |
| --- | --- |
| `outage` | **The gate.** The kubeconfig's cluster doesn't answer at all, or a Recovery is in progress. Blocked when it answers ("nothing to recover"), refuses the kubeconfig (renew it), the objects are in the Bootstrap cluster before a pivot, or there is no kubeconfig (write one with `kubeconfig renew`). |
| `datastore` | **The proof of the outage**, before anything starts: nothing writes to the datastore for 60 seconds (kine's revision doesn't grow). Otherwise blocked: a live cluster writes to it, and may only be cut off from here (a VPN, an API proxy); nothing was started. Postgres only; the data source must resolve from this host. Done while a Recovery is in progress. |
| `bootstrap-cluster` | As in `bootstrap`. |
| `host` | Docker has at least 4 GiB of memory, and the Docker host's `fs.inotify.max_user_watches` ≥ 524288 and `max_user_instances` ≥ 512, read inside the Bootstrap cluster's container. Otherwise blocked, with the `sysctl -w` to run on the Docker host. |
| `providers`, `before-apply` | As in `bootstrap`. |
| `break-glass` | The Break-glass controller (`selfhostctl-<name>-break-glass`, k0s without a worker, the API on localhost only) serves the datastore, at the live K0sControlPlane's version. It is started at the version in `cluster.manifests`; if the live one differs, it is recreated at that. Its config comes from the K0sControlPlane (see [design.md](design.md#recovery)): `--disable-components`, `api.extraArgs`, `storage`, the network CIDRs, with dynamic config on. The bundle gives its PKI and `externalAddress`; the encryption key its encryption config, which must be `aescbc` with key `key1`, then `identity`. |
| `infrastructure-gone` | The Infrastructure adapter, through the Break-glass controller: the old machines can't come back. `capmox`: Proxmox no longer has any of their VMs, on any node, running or not; one still there blocks (start it, or destroy it to recover; on a node that isn't online, bring the node back, or move the VM's config to an online node and destroy it there), as does its VMID now held by another VM. `none`: done. |
| `webhooks` | The providers' webhook configurations (label `cluster.x-k8s.io/provider`) are saved in the Bootstrap cluster's ConfigMap `kube-system/selfhostctl-recovery-webhooks` and gone from the datastore: no pod serves them, and the move must pass. Saved once; a re-run keeps the saved ones. |
| `suspend-gitops` | Every Flux Kustomization and HelmRelease is suspended, the root first. Without `gitops`: done. |
| `pause-children` | Every Cluster outside the Management cluster's namespace is paused, marked `selfhostctl.io/paused`. One you paused yourself is left alone. |
| `delete-nodes` | The datastore holds no Node: a new machine with a reused IP would match an old one. |
| `move-to-bootstrap` | The Cluster's namespace is in the Bootstrap cluster, unpaused. The pivot's gates; a dry run first; a `split` resumes. |
| `delete-break-glass` | The Break-glass controller's container is gone. |
| `infrastructure` | The Infrastructure adapter (`infrastructure.adapter`) has readied the old Machines' infrastructure. `capmox`: every old ProxmoxMachine names a Proxmox node that is online, so CAPMOX can tell its VM is gone, and, with zones, the Cluster shows its failure domains (waited for up to 10 minutes). Blocked without `spec.credentialsRef` on the ProxmoxCluster, when the Proxmox certificate doesn't verify (add `root_ca` to that Secret, or set `insecure: "true"`), or with no node online. `none`, the default: done. |
| `replace-machines` | The Cluster's Machines from before (recorded by name in the recovery ConfigMap, key `machines.json`) are gone: deleted without drain, volume wait or k0smotron's `kcp-cleanup` hook, since their nodes are gone. Another lifecycle hook on one of them blocks, before any is deleted: do or skip its cleanup, remove the annotation, run again. The control plane creates new ones, which come up on the datastore. |
| `ready` | As in `bootstrap`, on the Bootstrap cluster: the new machines are healthy. |
| `move-to-management` | The Cluster's namespace is back in the Management cluster, unpaused. The pivot's gates; with providers that differ, run `upgrade` (it runs at this step of a Recovery), then `recover` again. |
| `restore-webhooks` | The saved webhook configurations are in the Management cluster again. |
| `leftovers` | The Break-glass controller's and the old machines' autopilot `ControlNode`s and `kube-node-lease` leases are gone. |
| `resume-gitops` | What `suspend-gitops` suspended is resumed, the root last. |
| `unpause-children` | Every Cluster `pause-children` paused is unpaused, once the Management cluster is healthy and that child's API answers with all its Nodes Ready. One that doesn't stays paused: blocked after 10 minutes; run again later. |
| `delete-bootstrap-cluster` | As in `bootstrap`: after the stable window, the Bootstrap cluster is deleted. Until then, exit 4: run again. That ends the Recovery. |

The phases from `break-glass` to `delete-nodes` are done ("moved") once the objects are in the Bootstrap cluster; `infrastructure` to `ready` once they are back in the Management cluster. After the Recovery, `recover` finds nothing to recover (exit 3).

## upgrade

`upgrade` brings the live Management cluster's providers to the config's versions and `providers.variables`, through clusterctl. The core is always this release's (README, support matrix).

| Phase | Done when |
| --- | --- |
| `management` | The Management cluster manages itself and answers through `cluster.kubeconfig`. Otherwise blocked: finish `bootstrap` first. During a Recovery only at the step where `move-to-management` asks for it: the objects wait in the Bootstrap cluster and the new machines answer; otherwise blocked. |
| `providers` | Every declared provider is installed at its version, with the config's variables (fingerprint). Its reason lists them. When it runs, its reason is the plan: each provider that goes to another version, or is installed again because a variable changed (then every provider is). clusterctl also brings cert-manager to the version it ships. Blocked: a declared provider that isn't installed or an installed one that isn't declared (upgrade adds and removes nothing), a core newer than this release's (use the release for that Cluster API minor), or one more than 3 minors behind (upgrade through a release in between). Providers without a fingerprint at the config's versions: **manual**, `providers adopt`. |

A second `upgrade` does nothing.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Failure: something went wrong while running a Phase; the error says what |
| 2 | Usage or config error: an unknown command or flag, a missing or invalid config, an unset or empty environment variable the config needs, an unknown `--from` Phase |
| 3 | Blocked: a Phase's precondition is missing; the reason says what to fix |
| 4 | Manual checkpoint: the instruction says what to do, then run the same command again |

Errors, reasons and instructions go to stderr; progress and `status` go to stdout.

## Environment

- **The config's variables:** every environment variable the config names is checked before anything runs. All of the unset or empty ones are reported at once (exit 2).
- **Docker:** the daemon is `DOCKER_HOST` when set, else the Docker CLI's active context (`DOCKER_CONTEXT`, else `currentContext` in `~/.docker/config.json`, or `DOCKER_CONFIG`). The context is read from its files; the `docker` binary is not needed.
