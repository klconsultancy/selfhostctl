# The config: `selfhostctl/v1alpha1`

One YAML file per Management cluster, kept in the operator's repo. Unknown fields are an error, reported with their full path, e.g. `bootstrapCluster.kindConfig`. This page grows with each slice of #1. It lists only the fields the tool reads today.

```yaml
apiVersion: selfhostctl/v1alpha1
kind: ManagementCluster
metadata:
  name: management
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
| `bootstrapCluster.network` | no | The Docker network the Bootstrap cluster joins. Empty: Docker's default network. An infrastructure provider in Docker (CAPD) puts its machines on `kind`, so the Bootstrap cluster must join it. A missing network is created as a bridge. |
| `bootstrapCluster.extraMounts[]` | no | Bind mounts from the host into the Bootstrap cluster: `hostPath` and `containerPath` (absolute), `readOnly`. |

Declare a mount only when a provider needs it. In particular, the Docker socket gives every pod in the Bootstrap cluster control over the host. CAPD needs it; CAPMOX doesn't.

The Bootstrap cluster's k0s version is not configurable. Each tool release pins one, like its CAPI core version (docs/adr/0001-core-libraries.md).
