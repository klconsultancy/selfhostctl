# One module, pinned to the CAPI minor's Kubernetes line

The tool links the clusterctl library, controller-runtime, client-go, the Helm SDK and the Docker SDK into one module, with no shell-outs (#1). Every Kubernetes-facing library follows the `k8s.io/*` line of the Cluster API minor the release is built against. For CAPI v1.14 that is `k8s.io/*` v0.36 and controller-runtime v0.24 (#3).

| Library | Version | Why this one |
| --- | --- | --- |
| `sigs.k8s.io/cluster-api` | v1.14.4 | The release's CAPI minor |
| `sigs.k8s.io/controller-runtime` | v0.24.1 | The one CAPI v1.14.4 requires |
| `k8s.io/*` | v0.36.x | The line CAPI v1.14.4 requires |
| `helm.sh/helm/v4` | v4.2.4 | The last Helm minor on `k8s.io` v0.36 / controller-runtime v0.24. v4.3 moves to v0.37 / v0.25 |
| `github.com/moby/moby/client` | v0.6.2 | The Docker SDK |

**One `replace`:** `sigs.k8s.io/cluster-api/api => sigs.k8s.io/cluster-api/api v1.14.4`. Since v1.14, CAPI keeps its API types in their own module. It requires that module at a placeholder version (`v0.0.0-00010101000000-000000000000`) and resolves it with a local `replace`, which consumers don't inherit. The `replace` here points at the same release and is bumped with `sigs.k8s.io/cluster-api`.

**Moving to the next CAPI minor** is a release of its own (the support matrix):
- bump `sigs.k8s.io/cluster-api`, its `api` `replace` and controller-runtime to what that CAPI requires;
- move Helm to the newest minor on the same `k8s.io` line;
- run the smoke test (`go test -tags integration ./internal/smoke/`).

Renovate leaves those minors alone for that reason.

## The Bootstrap cluster is one k0s container, not kind

The Bootstrap cluster is one k0s container (controller and worker), created through the Docker SDK. kind is not used. The kind library runs the `docker` CLI for everything it does, which the PRD's "no shell-outs" rules out, and which would require the CLI on the operator's machine. The Break-glass controller is a k0s container through the same SDK, so both temporary clusters share one mechanism.

The proof ran both side by side, through bootstrap, pivot and recovery (run 42495292653, branch `proof/capd-self-managed`), with no difference. The Bootstrap cluster's k0s version is pinned per tool release, like the CAPI core.

## Consequences

- The PRD's `bootstrapCluster.kindConfig` goes. What the Bootstrap cluster needs from the config (extra mounts, e.g. the Docker socket for CAPD) is declared on the container.
- The Docker SDK reads `DOCKER_HOST`, never the Docker CLI's contexts. On colima, Docker Desktop and Rancher Desktop the daemon is reached through a context, so the tool resolves the active context's endpoint itself (#4). Until then, the smoke test needs `DOCKER_HOST`.
- A first run of clusterctl's `Init` adds kubeadm's latest bootstrap and control-plane providers for any type left empty. The tool always passes every type explicitly.
- Open: k0s in Docker on Docker Desktop for macOS is unproven. Colima and the GitHub-hosted runners work.
