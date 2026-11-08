# The command line

```sh
selfhostctl bootstrap -f management.yaml [--from <phase>]
selfhostctl status    -f management.yaml
selfhostctl version
```

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

`bootstrap` has these Phases so far:

| Phase | Done when |
| --- | --- |
| `bootstrap-cluster` | The Bootstrap cluster runs: the container `selfhostctl-<name>-bootstrap` (one k0s controller and worker, at the version this release pins) exists, and its node is Ready. Its API is published on localhost only. It mounts nothing from the host unless the config declares it. |

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
