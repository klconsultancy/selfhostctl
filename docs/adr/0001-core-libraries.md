# One module: CAPI fixed per release, Helm on its newest minor

The tool links the clusterctl library, controller-runtime, client-go, the Helm SDK and the Docker SDK into one module, with no shell-outs (#1). The Cluster API version is fixed per release (the support matrix). Helm follows its newest minor, because Helm patches only that one: no v4.2.x came out after v4.3.0. `k8s.io/*` and controller-runtime go wherever the newer of the two requires (#3).

For CAPI v1.14:

| Library | Version | Why this one |
| --- | --- | --- |
| `sigs.k8s.io/cluster-api` | v1.14.4 | The release's CAPI minor |
| `helm.sh/helm/v4` | v4.3.1 | Helm's newest minor, the only one it patches |
| `k8s.io/*` | v0.37.x | What Helm v4.3 requires. CAPI v1.14.4 asks for v0.36 |
| `sigs.k8s.io/controller-runtime` | v0.25.2 | What Helm v4.3 requires. CAPI v1.14.4 asks for v0.24.1 |
| `github.com/moby/moby/client` | v0.6.2 | The Docker SDK |

So the clusterctl library runs on a newer `k8s.io` and controller-runtime than it was built with. The smoke test (`go test -tags integration ./internal/smoke/`) checks that combination on every pull request: `clusterctl init`, its inventory read back, and a Helm install. The e2e checks `move` and upgrade on top (#9 onwards). A Helm minor that no longer fits CAPI's minor is not forced in: it comes with the tool release on the next CAPI minor.

**One `replace`:** `sigs.k8s.io/cluster-api/api => sigs.k8s.io/cluster-api/api v1.14.4`. Since v1.14, CAPI keeps its API types in their own module. It requires that module at a placeholder version (`v0.0.0-00010101000000-000000000000`) and resolves it with a local `replace`, which consumers don't inherit. The `replace` here points at the same release and moves with `sigs.k8s.io/cluster-api`.

**Vulnerabilities:** `govulncheck` runs on every pull request and daily on `main`. It reports advisories in code the binary actually reaches, including dependencies that haven't changed. Dependency review only looks at what a pull request changes.

## The Bootstrap cluster is one k0s container, not kind

The Bootstrap cluster is one k0s container (controller and worker), created through the Docker SDK. kind is not used. The kind library runs the `docker` CLI for everything it does, which the PRD's "no shell-outs" rules out, and which would require the CLI on the operator's machine. The Break-glass controller is a k0s container through the same SDK, so both temporary clusters share one mechanism.

The proof ran both side by side, through bootstrap, pivot and recovery (run 42495292653, branch `proof/capd-self-managed`), with no difference. The Bootstrap cluster's k0s version is pinned per tool release, like the CAPI core.

## Consequences

- The PRD's `bootstrapCluster.kindConfig` goes. What the Bootstrap cluster needs from the config (extra mounts, e.g. the Docker socket for CAPD) is declared on the container.
- The Docker SDK reads `DOCKER_HOST`, never the Docker CLI's contexts. On colima, Docker Desktop and Rancher Desktop the daemon is reached through a context, so the tool resolves the active context's endpoint itself (#4). Until then, the smoke test needs `DOCKER_HOST`.
- A first run of clusterctl's `Init` adds kubeadm's latest bootstrap and control-plane providers for any type left empty. The tool always passes every type explicitly.
- Open: k0s in Docker on Docker Desktop for macOS is unproven. Colima and the GitHub-hosted runners work.
