# The config: `selfhostctl/v1alpha1`

One YAML file per Management cluster, kept in the operator's repo. Unknown fields are an error, reported with their full path, e.g. `bootstrapCluster.kindConfig`. This page grows with each slice of #1. It lists only the fields the tool reads today.

```yaml
apiVersion: selfhostctl/v1alpha1
kind: ManagementCluster
metadata:
  name: management
providers:
  clusterTopology: true
  bootstrap: [{name: k0sproject-k0smotron, version: v2.3.0}]
  controlPlane: [{name: k0sproject-k0smotron, version: v2.3.0}]
  infrastructure:
    - name: proxmox
      version: v0.9.2-kl.1
      url: https://github.com/klconsultancy/cluster-api-provider-proxmox/releases/latest/infrastructure-components.yaml
  ipam: [{name: in-cluster, version: v1.1.1}]
  variables:
    EXP_MACHINE_POOL: "true"
bootstrapCluster:
  network: kind
  extraMounts:
    - hostPath: /var/run/docker.sock
      containerPath: /var/run/docker.sock
```

| Field | Required | Meaning |
| --- | --- | --- |
| `apiVersion` | yes | `selfhostctl/v1alpha1` |
| `kind` | yes | `ManagementCluster` |
| `metadata.name` | yes | The Management cluster's name: a lowercase DNS label of at most 40 characters. It names the tool's own objects, e.g. the container `selfhostctl-<name>-bootstrap`. |
| `providers.bootstrap[]`, `providers.controlPlane[]`, `providers.infrastructure[]` | yes, at least one each | The Cluster API providers: `name` as clusterctl knows it, `version` (`v1.2.3`), and an optional `url` of the components for a provider clusterctl doesn't know or a fork. In a `url`, `latest` stands for the declared version. The core provider isn't declared: its version is the one this release is built against (support matrix in the README). |
| `providers.ipam[]`, `providers.addon[]` | no | The same, for IPAM and add-on providers. |
| `providers.clusterTopology` | no | `true` turns on Cluster API's ClusterTopology feature gate (`CLUSTER_TOPOLOGY=true`) where the core still needs it, before CAPI 1.15. From 1.15 it is on by default, and this is a no-op. |
| `providers.variables` | no | clusterctl variables for the providers' components, e.g. other feature gates. **They are the only ones used.** Variables in the operator's environment never reach clusterctl, so a run can't turn a gate off by accident. The one exception is `GITHUB_TOKEN`, which only lifts the GitHub rate limit for clusterctl's downloads. |
| `bootstrapCluster.network` | no | The Docker network the Bootstrap cluster joins. Empty: Docker's default network. An infrastructure provider in Docker (CAPD) puts its machines on `kind`, so the Bootstrap cluster must join it. A missing network is created as a bridge. |
| `bootstrapCluster.extraMounts[]` | no | Bind mounts from the host into the Bootstrap cluster: `hostPath` and `containerPath` (absolute), `readOnly`. |

Declare a mount only when a provider needs it. In particular, the Docker socket gives every pod in the Bootstrap cluster control over the host. CAPD needs it; CAPMOX doesn't.

Provider versions are plain `name`/`version` scalars on purpose: Renovate, yq and drift alerts read them from this file.

The Bootstrap cluster's k0s version is not configurable. Each tool release pins one, like its CAPI core version (docs/adr/0001-core-libraries.md).
