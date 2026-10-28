# Security Policy

DirAdvisor runs as a privileged container, reads the Docker API and sets project IDs on directories of
the host. That makes it security-sensitive by nature, and reports are taken seriously.

## Reporting a vulnerability

**Please report vulnerabilities privately; do not open a public issue, pull request, or discussion for a
security problem.**

Use **GitHub's private vulnerability reporting**:

1. Go to the repository's **Security** tab ->
   **[Report a vulnerability](https://github.com/motwok/DirAdvisor/security/advisories/new)**.
2. Describe the issue with enough detail to reproduce and assess it.

This keeps the report private between you and the maintainer and integrates with GitHub Security
Advisories for a coordinated fix and disclosure.

### What to include

- A clear description of the vulnerability and its **impact**.
- **Steps to reproduce** (or a proof of concept).
- Affected **version** (image tag or digest) and your **kernel and Docker versions**.
- Any suggested mitigation, if you have one.

## What to expect

This is a single-maintainer project, so responses are best-effort rather than bound to a fixed SLA. You
can expect an acknowledgement of your report, an assessment, and, for confirmed issues, a coordinated fix
and disclosure. Credit is given to reporters who want it.

Please give a reasonable amount of time for a fix before any public disclosure.

## Supported versions

Only the **latest release** (`:latest`) receives security fixes. Older releases are not patched; please
update to the newest one.

| Version | Supported |
|---|---|
| Latest release (`:latest`) | yes |
| Older releases | no |

## Automatic dependency releases

Updates of Go and of DirAdvisor's dependencies are merged automatically by Dependabot once the build and
the smoke test pass, and **every such merge is a release**. Security fixes therefore reach `:latest`
quickly, but a vulnerable or compromised Go release or dependency would be shipped automatically as
well. If you want to review updates first, pin a version tag instead of `:latest`. Details and
mitigations: [RELEASING.md](RELEASING.md#dependency-updates-are-released-automatically).

## Scope

DirAdvisor deliberately does things that can look alarming but are by design:

- it runs **privileged** (or with `SYS_ADMIN` / `SYS_RESOURCE`), because reading and setting project
  quotas needs these capabilities,
- it mounts the **Docker socket** to discover volumes, bind mounts and their containers,
- it mounts the **host filesystem read-write** and sets the **project ID** and the project-inherit flag
  on directories it measures,
- `/metrics` exposes **paths, volume, container and stack names**. Do not publish its port to untrusted
  networks.

These documented behaviors are **not** vulnerabilities on their own. Genuine security issues include, for
example: changing file contents, owners or permissions on the host; any write or control call against the
Docker API; acting on directories outside the discovered and configured ones (for example through a
crafted path, glob or volume name); execution of untrusted or attacker-controlled code; or disclosure of
data beyond what is documented for `/metrics`.
