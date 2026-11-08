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
cluster:
  manifests: management/
  namespace: management
recovery:
  profile: k0smotron-kine
  bundleFromEnv: MANAGEMENT_RECOVERY_BUNDLE
  encryptionKeyFromEnv: MANAGEMENT_ENCRYPTION_KEY
  kine: {datasourceFromEnv: MANAGEMENT_KINE_DATASOURCE}
beforeApply:
  - manifest: {path: bootstrap-secret.yaml}
    clusters: [bootstrap]
  - helm:
      chart: oci://ghcr.io/klconsultancy/charts/capmox-pve-proxy
      version: 0.2.0
      release: capmox-pve-proxy
      namespace: capmox-system
      values: proxy-values.yaml
    clusters: [bootstrap, management]
```

Relative paths are relative to the config file.

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
| `bootstrapCluster.deleteAfterStable` | no | How long the Management cluster must manage itself unchanged before the Bootstrap cluster is deleted. A duration; default `10m`. |

| `cluster.manifests` | yes | The Management cluster's own definition: a kustomization directory (built in-process), a directory of YAML files (applied in name order), or one file. It holds the Cluster, its control plane, infrastructure and machine templates. No `${VAR}` substitution: secrets come through `beforeApply`. |
| `cluster.namespace` | yes | The Cluster object's namespace: the one namespace a move covers. |
| `cluster.name` | no | The Cluster object's name; `metadata.name` when empty. |
| `cluster.kubeconfig` | no | Where bootstrap writes the admin kubeconfig (mode 0600); `~/.kube/<metadata.name>.kubeconfig` when empty. |
| `recovery.profile` | with `recovery` | How the Management cluster is recovered: `k0smotron-kine`, the one profile so far. Without `recovery`, bootstrap doesn't ask for a Recovery bundle. |
| `recovery.bundleFromEnv` | with `recovery` | The environment variable holding the Recovery bundle (`selfhostctl recovery export`). Bootstrap's `recovery-bundle` checkpoint doesn't let a bootstrap finish without it. |
| `recovery.encryptionKeyFromEnv` | with `recovery` | The variable holding the key the cluster's Secrets are encrypted with at rest. Use the same variable in the `beforeApply` file that gives the machines their encryption config. |
| `recovery.kine.datasourceFromEnv` | with `k0smotron-kine` | The variable holding kine's data source, e.g. `postgres://user:password@host:5432/db`. |

None of the `recovery` variables has to be set to start a bootstrap: the checkpoint asks for them once the cluster exists.

| Field | Required | Meaning |
| --- | --- | --- |
| `afterPivot[]` | no | Entries like `beforeApply`'s (`manifest` or `helm`), without `clusters`: installed on the Management cluster once it manages itself, before the Bootstrap cluster goes. Empty when GitOps comes from the cluster's own definition. |
| `beforeApply[]` | no | What must be on a cluster before the Management cluster is applied to it: an API proxy the infrastructure provider talks to, a Secret the machines read at boot. Each entry has exactly one of `manifest` and `helm`, and `clusters`. |
| `beforeApply[].clusters` | yes | Where the entry is installed: `bootstrap` (the Bootstrap cluster), `management` (the Management cluster itself, after the pivot), or both. The key is not `on`, because YAML 1.1 reads `on` as the boolean `true`. |
| `beforeApply[].manifest.path` | yes | A YAML file of Kubernetes objects, applied server-side (field manager `selfhostctl`). Every namespaced object names its namespace. |
| `beforeApply[].helm` | — | A chart installed, or upgraded when its version or values changed, and waited for. `chart`: an `oci://` reference, a chart name together with `repo` (a classic repository's URL), or a local directory. `version` is required for a remote chart. `release` and `namespace` name the release; the namespace is created when missing. `values` is an optional values file. |

**`${VAR}` in manifests and values files** is replaced with the environment variable `VAR`, and nothing else is: no defaults, no `$VAR`, no other templating. Every variable these files refer to is checked before anything runs; an unset or empty one is an error (exit 2), with all of them named. Substituted values never appear in output or errors: they are replaced with `<redacted>`, even in object names and in messages from the API server.

YAML 1.1 reads bare `y`, `n`, `yes`, `no`, `on` and `off` as booleans. Quote such a value: `namespace: "n"`.

Declare a mount only when a provider needs it. In particular, the Docker socket gives every pod in the Bootstrap cluster control over the host. CAPD needs it; CAPMOX doesn't.

Provider versions are plain `name`/`version` scalars on purpose: Renovate, yq and drift alerts read them from this file.

The Bootstrap cluster's k0s version is not configurable. Each tool release pins one, like its CAPI core version (docs/design.md#bootstrap-cluster).
