# Security policy

## Supported versions

No release has been published yet. Once there are releases, only the latest one receives security fixes.

## Reporting a vulnerability

Report vulnerabilities privately through [GitHub's private vulnerability reporting](https://github.com/klconsultancy/selfhostctl/security/advisories/new), not in a public issue.

Include what an attacker can do, the steps to reproduce it, and the affected version or commit. You will get an answer within a week. Once a fix is released, the advisory is published with credit to you unless you prefer otherwise.

The tool handles a management cluster's secrets: its PKI, its datastore encryption key and its GitOps credentials, all read from the environment. Issues that write such material anywhere other than where the operator asked (a file, a log, an error message), or that silently regenerate it, are treated as the most severe.
