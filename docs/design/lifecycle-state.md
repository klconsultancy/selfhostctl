# Lifecycle state: where the Management cluster's objects live

Status: agreed with the user on 2026-11-08, for #9 onwards. This is a design document, not a record: revise it when the design moves.

## Problem

Each Phase (see `GLOSSARY.md`) judges itself from live state, with no state file. Up to #8, every bootstrap phase looks only at the Bootstrap cluster. That is enough until there is a Pivot. After it, the Bootstrap cluster is gone. A new `bootstrap` run would then see every phase as not done: it would create a new Bootstrap cluster, install the providers, and apply the Cluster's manifests to it. That leaves a second copy of the Cluster objects next to the real Management cluster, and it can create machines against the same infrastructure.

## A state machine with derived state

The commands are a state machine whose state is never stored. Each run derives it again from live state, and the Phases act on it. The linear phase list was a simplification that holds only before the first Pivot. One variable decides the rest: **where the Management cluster's objects live**, its *location*.

| Location | Derived from | `bootstrap` |
| --- | --- | --- |
| `none` | No Bootstrap cluster holds the Cluster object, and the Management cluster can't be reached or doesn't hold it | starts from the first phase |
| `bootstrap` | The Bootstrap cluster holds the Cluster object, unpaused | the phases up to and including the Pivot |
| `split` | Both hold it, paused: a `move` that failed halfway | resumes the move (#9's `--from move` case) |
| `management` | The Management cluster holds the Cluster object and manages itself | every phase before the Pivot is done ("pivoted"); it never creates a Bootstrap cluster |

`recover` (#11, #12) has the same variable. Its precondition is that the Management cluster's API doesn't answer, and it moves the objects through `bootstrap` (via the Break-glass controller) and back to `management`.

### One `locate`, read by every Phase

The location is computed **once per run**, by a `locate` step that runs before the first Phase. The Phases receive it and read it; none checks for itself where the objects are. That makes the state machine visible in one place, and testable as a table: for each location (built with fake clusters), the expected state of every Phase. `status` prints the location first.

## Reaching the Management cluster without a state file

`locate` must reach the Management cluster even when the Bootstrap cluster and any kubeconfig file are gone. The handle is the **Recovery bundle**, which the operator keeps in their environment anyway: since #8, `bootstrap` can't get past `recovery-bundle` without it. The bundle holds the cluster CA and its key, and the control-plane endpoint. From them the tool:

- creates a client certificate for one run: in memory only, never written; it expires after an hour; CN `selfhostctl`, O `system:masters`;
- connects to the endpoint, verifying the server against the bundle's CA.

Without `recovery` in the config, there is no bundle. Then the handle is the kubeconfig the `kubeconfig` phase wrote (`cluster.kubeconfig`). That handle is weaker: it can be missing on another operator's machine.

## Fail closed

The tool can't prove the location is `none` when it holds no handle to the Management cluster: no bundle in the environment and no kubeconfig file. That is exactly the situation of a first bootstrap, and also of an operator who forgot to load the environment. The rule:

- **Creating a Bootstrap cluster** needs one of these:
  - a handle with which `locate` found the location `none`;
  - or **`bootstrap --first`**: the operator asserts that no Management cluster exists yet.

  Otherwise the phase `bootstrap-cluster` is blocked: "can't tell whether the Management cluster already exists: put the Recovery bundle in `<VAR>`, or run with `--first` if this is the cluster's first bootstrap".
- **When the endpoint is known** without a handle (e.g. an infrastructure cluster object in `cluster.manifests` declares it, as a ProxmoxCluster does), the tool also probes it. Any TLS answer means a cluster runs there, and `--first` is refused. This is the guard the first user's Makefile has (`_cluster-down`).
- `--first` is never remembered. It only covers the run it is given to.

## Consequences

- The Phases before the Pivot gain one rule each: at the location `management` they are done, with the note "pivoted". `bootstrap-cluster` follows the rule above.
- `status` without a handle reports the location as unknown and says why, instead of guessing `none`.
- The e2e (#9) runs `bootstrap` again after the Pivot and checks that nothing runs and no Bootstrap cluster appears. It also runs it without the bundle and checks that it is blocked.
- `recovery export` works from either side: from the Bootstrap cluster before the Pivot, from the Management cluster after it.
