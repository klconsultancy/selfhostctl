# selfhostctl

The lifecycle of one self-managed Cluster API management cluster: bootstrap, pivot, upgrade, recovery and status, driven by one declarative config.

## Clusters

**Management cluster**:
The one cluster this tool exists for. It runs the Cluster API providers and manages itself, and it manages every Child cluster.
_Avoid_: mothership, management plane, target (except as the other side of a move)

**Child cluster**:
A cluster the Management cluster manages. It is defined by the Management cluster's own definitions, never created or recovered by this tool.
_Avoid_: workload cluster (in this project's docs), downstream cluster

**Bootstrap cluster**:
The temporary local cluster (Cluster API's glossary term) that holds the Management cluster's objects while the Management cluster cannot: before the first pivot, and during a recovery.
_Avoid_: kind cluster, ephemeral cluster, seed cluster

**Break-glass controller**:
A temporary control-plane process, run against the Management cluster's surviving datastore with its PKI, that serves the cluster's objects when no machine of it is left. It runs no workloads and exists only during a recovery.
_Avoid_: rescue node, temporary controller, emergency API

**Infrastructure adapter**:
What a Recovery asks of the infrastructure the Management cluster's machines ran on, before it replaces them. The default asks nothing.
_Avoid_: infrastructure provider (that is Cluster API's), plugin, driver

## Lifecycle

**Phase**:
One ordered step of a command, whose completion is judged from live state alone: done, next, or blocked.
_Avoid_: stage, step (in docs and output)

**Manual checkpoint**:
A Phase that only the operator can complete. The command stops there with an instruction and the operator runs it again.
_Avoid_: prompt, pause, gate

**Gate**:
A precondition a Phase refuses to run without, such as the providers matching on both sides of a move.
_Avoid_: check (when it blocks)

**Pivot**:
The move of the Management cluster's objects from the Bootstrap cluster into the Management cluster itself, after which it manages itself.
_Avoid_: handover, self-hosting step

**Recovery**:
Bringing a Management cluster whose machines are all gone back to managing itself, from its surviving datastore.
_Avoid_: restore, disaster recovery (for this specific flow)

**Recovery bundle**:
The material a Recovery needs and only the cluster holds after a bootstrap: its certificate authorities, service-account key and control-plane endpoint. The operator keeps it in their own secret store.
_Avoid_: PKI export, backup

## Configuration

**Recovery profile**:
The built-in description of how a given control-plane and datastore combination is recovered, selected in the config.
_Avoid_: recovery plugin, strategy

**Infrastructure adapter**:
The built-in handling of one infrastructure provider's quirks during a Recovery, selected in the config.
_Avoid_: hook, plugin, driver
