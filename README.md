# selfhostctl

[![CI](https://github.com/klconsultancy/selfhostctl/actions/workflows/ci.yml/badge.svg)](https://github.com/klconsultancy/selfhostctl/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/klconsultancy/selfhostctl)](https://github.com/klconsultancy/selfhostctl/releases)
[![Go version](https://img.shields.io/github/go-mod/go-version/klconsultancy/selfhostctl)](go.mod)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/klconsultancy/selfhostctl/badge)](https://scorecard.dev/viewer/?uri=github.com/klconsultancy/selfhostctl)
[![License](https://img.shields.io/github/license/klconsultancy/selfhostctl)](LICENSE)
[![golangci-lint](https://img.shields.io/badge/linted_by-golangci--lint-blue)](https://golangci-lint.run)
[![Conventional Commits](https://img.shields.io/badge/Conventional%20Commits-1.0.0-yellow.svg)](https://conventionalcommits.org)

One Go binary for the lifecycle of a self-managed [Cluster API](https://cluster-api.sigs.k8s.io) management cluster: bootstrap through a kind **Bootstrap cluster**, pivot, provider upgrade, recovery and status, driven by one declarative config file.

> **Status: early development.** `bootstrap` creates the Bootstrap cluster and nothing more yet. The design is the [PRD](https://github.com/klconsultancy/selfhostctl/issues/1). See [the command line](docs/cli.md) and [the config](docs/config.md).

## Scope

- **In scope:** the lifecycle of **one self-managed management cluster**: bootstrap, pivot, provider upgrade, recovery, status.
- **Out of scope:** child (workload) clusters. The management cluster's GitOps applies them; they never need `selfhostctl`.
- **Out of scope:** secret stores. The tool reads secrets from its environment and never talks to a vault.

## Support matrix

Each release is built against exactly one Cluster API minor version.

| selfhostctl | Cluster API |
| ----------- | ----------- |
| —           | —           |

No release has been published yet.

## Releases

Releases are built by GitHub Actions on GitHub-hosted runners: one binary per OS and architecture (Linux and macOS, amd64 and arm64), with `checksums.txt` and a signed build provenance attestation. Verify a binary with:

```sh
gh attestation verify selfhostctl_<version>_<os>_<arch> --repo klconsultancy/selfhostctl
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues as described in [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE)
