# Troubleshooting

Start with `docker logs hatchlens` and the series `hatchlens_admin_action_needed` and
`hatchlens_directory_unmeasurable`; their `action` or `reason` says what is wrong
([Metrics.md](Metrics.md#problems-and-admin-actions)).

## HatchLens does not start or is not ready

| Symptom | Cause | Fix |
|---|---|---|
| exits with code 1, log `invalid config` | the config is invalid or its file is missing | fix the line named in the log |
| exits with code 1, log `unsupported Docker` | Docker API older than 1.41 | update Docker |
| exits with code 2 | unknown flag or value | check the command line |
| `/-/ready` stays 503, `reconcile_errors_total` grows | Docker socket not reachable | mount `/var/run/docker.sock`; configured directories are measured anyway once Docker answered at least once (see `docker_root_unknown`) |
| `directory_unmeasurable{reason="quota_access_failed"}`, log `quotactl_fd ... operation not permitted` | capabilities or seccomp | add `SYS_ADMIN`; if that is not enough, try `privileged: true` |
| `listen failed` | port in use (cAdvisor also uses 8080) | map another host port or set `listen_address` |
| Windows: log `reconcile failed` with "Access is denied" | the container does not run as `ContainerAdministrator` (a `user:` in the stack overrides the image) | remove the `user:` |
| Windows: exits with code 1, `host_socket is empty` | `directory` or `volume` runs without the host service | set `host_socket` or add them to `disable` |
| Windows: admin action `install_host_service` | the HatchLens Host service is not installed, not running or its pipe is not mounted | install the MSI, check `Get-Service HatchLensHost`, mount `\\.\pipe\hatchlens-host` |

## Values look wrong

**Too low right after start or after adding a directory.** HatchLens first walks the directory to set
the project ID; `hatchlens_directory_assigning` is 1 until it is done. The walk runs with idle I/O
priority, so on a busy disk it can take a while. To make it gentler still, set
`walk_files_per_second`.

**A volume is missing.** Check that it is a `local` volume and not excluded by `exclude.volumes`, and that its owner container is
not excluded (`exclude.containers`, `exclude.images`, `de.mo2000.hatchlens.enable=false`).
Bind mounts are never discovered; add them under `directories`.

**Bytes differ from `du`.** HatchLens counts allocated blocks like `du` (not `du -b`). On ZFS the size
is after compression, and blocks kept only by snapshots are not counted. Sockets, FIFOs and device
files that existed before the walk keep their old project ID; only their inode count is affected.

**The log says `walk failed ... invalid argument` on XFS.** An XFS v4 filesystem without the
`projid32bit` feature accepts project IDs up to 65535 only; the default range starts at 100000. Set
`project_ids` below 65536 for such hosts, or migrate the filesystem to XFS v5.

**An outer directory counts less than expected.** Directories inside it that are measured on their own
(another entry, a volume) are counted there, not in the outer one; see `hatchlens_directory_overlap`.

## Image checks

**A container is missing.** It is excluded (`exclude.containers`, `exclude.images` or the label
`de.mo2000.hatchlens.enable=false`), or it is an old Swarm task (not running) or being removed; those
are not reported. With `image` in `disable` there are no image metrics at all.

**A container stays `pending` or shows `error`.**

- `pending`: not checked yet. Checks go out one at a time, at least 10 seconds apart per registry, and a
  registry that answered 429 is paused, so after a start with many images this takes a while.
- `error` with `hatchlens_image_check_error`:
  - `not_found`: the image is not in the registry, usually a locally built image. Exclude it.
  - `denied`: a private image without credentials (add them under `images.registries`), or a Docker Hub
    repository that does not exist (also a locally built image).
  - `rate_limited`: the registry's pull limit; see
    [Configuration.md](Configuration.md#docker-hub-pull-limit).
  - `unreachable`: the daemon could not reach the registry.

  Each failed check logs a `registry check failed` line with the daemon's message. A failed check is
  retried after 5 minutes, doubling up to `check_interval`.

**`update_available` is 1, but the container is up to date.** The comparison is with the digest the
registry gives for the reference the container was created with. If the container was created from
another tag or an image ID, `image` and `mode` show it. `pulled` 1 means the new image is already on the
host, but the container still runs the old one: recreate it (`docker compose up -d`, stack redeploy).

## Container usage

**No CPU, memory or disk series, but network series.** The log says `no cgroup v2 found`: the host uses
cgroup v1, or the host root is not mounted at `rootfs` (`/:/host`). Only cgroup v2 is read.

**A container has no usage series at all.** It is not running, it is excluded, or its function is in
`disable`. A log line `container cgroup not found` names a container whose cgroup HatchLens could not
locate.

**No network series for a container.** Its network mode is `host`, `none` or `container:<other>`; those
are left out on purpose. An interface without `network` label belongs to no Docker network.

**Counters drop to zero.** The container was restarted; `rate()` and `increase()` handle that.

## Ownership and labels

**`stack` is missing.** The owner container has no Compose project. Add a relabel rule if you want a
value ([Install.md](Install.md#3-prometheus)).

**A volume used by containers is `unused`.** It has no Compose label and is written by containers of
different stacks, so there is no single owner. Give it a Compose label (create it from one stack's
compose file) or make the other mounts read-only.

**The owner is not the container you expected.** With several writers the first one stays the owner as
long as it writes; read-only mounts never own a volume. With a Compose label on the volume only
containers of that stack are candidates.

## Reasons

### `foreign_project_id`

The directory already carries a project ID outside the configured range that HatchLens did not assign,
for example from an admin or from Docker's own XFS quotas. HatchLens never overwrites it. If the ID is
not needed any more, reset it (ext4/XFS) and HatchLens takes over on the next reconcile:

```bash
lsattr -pd /path            # shows the current project ID
chattr -R -p 0 /path
```

If the ID belongs to your own range from an earlier setup, widen `project_ids` instead.

### `duplicate_path`

Two entries resolve to the same directory, for example a configured directory that is also the `device`
of a bind-option volume, or two such volumes. One of them is measured (volume before directory, then the
oldest volume, then the first in the config); remove the other one from the config.

### `path_missing`

The directory, or the parent of a glob, does not exist. If a mount is missing, mount it; the ID is kept.
If the directory is gone for good, remove it from the config.

### `project_ids_exhausted`

All IDs in `project_ids` are in use. Widen the range; IDs already assigned stay valid.

### `docker_root_unknown`

Docker is enabled but has never answered since the first start, so HatchLens does not know Docker's
data directory, which it must never walk into. Configured directories wait until Docker answers once;
after that the directory is remembered across restarts. Without Docker on this host add `volume` and
`image` to `disable` and set `docker_socket` to empty.

### `quota_access_failed`

HatchLens could not find the mount of the directory or could not read the quota state or usage of its
filesystem. The log line next to it has the error: `operation not permitted` means a missing capability
or a seccomp profile without `quotactl_fd` (add `SYS_ADMIN`, or `privileged: true`); on ZFS check that
`/dev/zfs` is passed to the container and that `zpool`/`zfs` work on the host.

## Docker volume removed, state lost

- A removed volume releases its ID. A bind-option volume first gets its directory reset to ID 0, because
  Docker leaves that directory in place.
- If `state.json` is lost or corrupt (it is then kept as `state.json.bak`), HatchLens takes the IDs it
  finds on the directories again, so the series keep their `project_id`.

## Removing project IDs

After uninstalling, the IDs stay on the files; without limits they do nothing. To remove them:

```bash
chattr -R -p 0 /path                  # ext4 and XFS
zfs project -C -r /path               # ZFS
```

Do this only for directories HatchLens no longer measures.
