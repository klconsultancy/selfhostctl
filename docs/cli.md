# The command line

```sh
selfhostctl bootstrap       -f management.yaml [--from <phase>] [--first]
selfhostctl pivot           -f management.yaml
selfhostctl status          -f management.yaml
selfhostctl providers adopt -f management.yaml
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

**Location.** Every run first works out where the Management cluster's objects live (docs/design.md#lifecycle-state): `none`, `bootstrap`, `split` (a move stopped halfway), `management`, or `unknown`. `status` prints it first.

| Location | Phases before `management-providers` | Bootstrap cluster |
| --- | --- | --- |
| `none`, `bootstrap` | judged as below | created if missing |
| `split`, `management` | done: "a move is in progress" / "pivoted" | never created |
| `unknown` | — | blocked, see below |

**Fail closed.** The handle to the Management cluster is its kubeconfig (`cluster.kubeconfig`). Without one, nothing proves there is no Management cluster yet, so `bootstrap-cluster` is blocked unless you run `bootstrap --first`. `--first` covers only that run, and is refused when:
- a kubeconfig exists but its cluster doesn't answer: a network blip (run again), a lost cluster (`recover`) or a stale file (remove it);
- the endpoint known from the manifests (e.g. a ProxmoxCluster's) accepts connections.

`pivot` runs bootstrap's phases from `management-providers`. `providers adopt` stamps a Management cluster's providers, installed some other way, with the config's variables fingerprint, after checking their versions. Run it only if they run with exactly `providers.variables`.

`bootstrap` has these Phases:

| Phase | Done when |
| --- | --- |
| `bootstrap-cluster` | The Bootstrap cluster runs: the container `selfhostctl-<name>-bootstrap` (one k0s controller and worker, at the version this release pins) exists, and its node is Ready. Its API is published on localhost only. It mounts nothing from the host unless the config declares it. |
| `providers` | The Bootstrap cluster runs exactly the declared providers, the core at this release's version, each installed with the declared variables. The tool records a fingerprint of the variables (never the values) on every Provider object as `selfhostctl.io/variables-fingerprint`. With some providers already installed, or other versions or variables, the phase is blocked and names the difference. The Bootstrap cluster holds nothing yet, so the fix is to delete its container and run again. |
| `before-apply` | Every `beforeApply` entry for `bootstrap` is in place on the Bootstrap cluster. A manifest counts as in place when a server-side dry-run apply would change nothing. A chart counts as in place when its release is deployed at the declared version, with the declared values. Running installs or upgrades only what isn't in place, in order. A release left pending by an interrupted Helm operation blocks the phase. |
| `apply` | `cluster.manifests` is applied to the Bootstrap cluster: a server-side dry-run would change nothing. |
| `ready` | The Cluster is `ControlPlaneAvailable`, its control plane has as many ready replicas as it wants, and every Machine is `NodeHealthy`. Running waits up to 30 minutes. `status` says what it still waits for. |
| `recovery-bundle` | **A Manual checkpoint.** The Recovery bundle in `recovery.bundleFromEnv` matches the cluster: the same CA, service-account key, front-proxy CA and endpoint, each private key belonging to its certificate. The variables `recovery.encryptionKeyFromEnv` and `recovery.kine.datasourceFromEnv` name are set too. Until then, the command stops with exit 4 and tells you to run `recovery export`. It warns when no `beforeApply` file uses the encryption-key variable. It is done right away when the config has no `recovery`. |
| `kubeconfig` | The Management cluster's admin kubeconfig (Cluster API's `<cluster>-kubeconfig` Secret) is at `cluster.kubeconfig`, mode 0600. |
| `management-providers` | The Management cluster runs exactly the declared providers, with the declared variables. Providers without a selfhostctl fingerprint: **manual**, `providers adopt`. Other versions: blocked. |
| `management-before-apply` | The `beforeApply` entries for `management` are in place on the Management cluster. |
| `move` | The Cluster's namespace is on the Management cluster, unpaused. **Gates:** the providers match on both sides (versions and fingerprints; an unstamped side is **manual**, `providers adopt`), and the namespace holds no other Cluster. Running does a dry run, then the move. A move that stopped halfway (`split`) resumes, and a move that ended before unpausing is unpaused. |
| `after-pivot` | The `afterPivot` entries are in place on the Management cluster. |
| `delete-bootstrap-cluster` | No Bootstrap cluster exists. Before it is deleted, the Management cluster must be `Available` with `RemoteConnectionProbe`, have every Machine `NodeHealthy`, and have had no change (condition or new Machine) for `bootstrapCluster.deleteAfterStable`. Until then it is **manual**: "stable for X of Y, run again then". |

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
