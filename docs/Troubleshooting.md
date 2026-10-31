# Troubleshooting

Start with `docker logs diradvisor` and the series `diradvisor_admin_action_needed` and
`diradvisor_directory_unmeasurable`; their `action` or `reason` says what is wrong
([Metrics.md](Metrics.md#problems-and-admin-actions)).

## DirAdvisor does not start or is not ready

| Symptom | Cause | Fix |
|---|---|---|
| exits with code 1, log `invalid config` | the config file is invalid | fix the line named in the log |
| exits with code 1, log `unsupported Docker` | Docker API older than 1.41 | update Docker |
| exits with code 2 | unknown flag or value | check the command line |
| `/-/ready` stays 503, `reconcile_errors_total` grows | Docker socket not reachable | mount `/var/run/docker.sock`; configured directories are measured anyway once Docker answered at least once (see `docker_root_unknown`) |
| `directory_unmeasurable{reason="quota_access_failed"}`, log `quotactl_fd ... operation not permitted` | capabilities or seccomp | add `SYS_ADMIN`; if that is not enough, try `privileged: true` |
| `listen failed` | port in use (cAdvisor also uses 8080) | map another host port or set `--web.listen-address` |

## Values look wrong

**Too low right after start or after adding a directory.** DirAdvisor first walks the directory to set
the project ID; `diradvisor_directory_assigning` is 1 until it is done. The walk runs with idle I/O
priority, so on a busy disk it can take a while. To make it gentler still, set
`global.walk_files_per_second`.

**A volume is missing.** Check that it is a `local` volume and not excluded by `docker.exclude.volumes`.
Bind mounts are never discovered; add them under `directories`.

**Bytes differ from `du`.** DirAdvisor counts allocated blocks like `du` (not `du -b`). On ZFS the size
is after compression, and blocks kept only by snapshots are not counted. Sockets, FIFOs and device
files that existed before the walk keep their old project ID; only their inode count is affected.

**An outer directory counts less than expected.** Directories inside it that are measured on their own
(another entry, a volume) are counted there, not in the outer one; see `diradvisor_directory_overlap`.

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

The directory already carries a project ID outside the configured range that DirAdvisor did not assign,
for example from an admin or from Docker's own XFS quotas. DirAdvisor never overwrites it. If the ID is
not needed any more, reset it (ext4/XFS) and DirAdvisor takes over on the next reconcile:

```bash
lsattr -pd /path            # shows the current project ID
chattr -R -p 0 /path
```

If the ID belongs to your own range from an earlier setup, widen `global.project_ids` instead.

### `duplicate_path`

Two entries resolve to the same directory, for example a configured directory that is also the `device`
of a bind-option volume, or two such volumes. One of them is measured (volume before directory, then the
oldest volume, then the first in the config); remove the other one from the config.

### `path_missing`

The directory, or the parent of a glob, does not exist. If a mount is missing, mount it; the ID is kept.
If the directory is gone for good, remove it from the config.

### `project_ids_exhausted`

All IDs in `global.project_ids` are in use. Widen the range; IDs already assigned stay valid.

### `docker_root_unknown`

Docker is enabled but has never answered since the first start, so DirAdvisor does not know Docker's
data directory, which it must never walk into. Configured directories wait until Docker answers once;
after that the directory is remembered across restarts. Without Docker on this host set
`docker.enabled: false`.

### `quota_access_failed`

DirAdvisor could not find the mount of the directory or could not read the quota state or usage of its
filesystem. The log line next to it has the error: `operation not permitted` means a missing capability
or a seccomp profile without `quotactl_fd` (add `SYS_ADMIN`, or `privileged: true`); on ZFS check that
`/dev/zfs` is passed to the container and that `zpool`/`zfs` work on the host.

## Docker volume removed, state lost

- A removed volume releases its ID. A bind-option volume first gets its directory reset to ID 0, because
  Docker leaves that directory in place.
- If `state.json` is lost or corrupt (it is then kept as `state.json.bak`), DirAdvisor takes the IDs it
  finds on the directories again, so the series keep their `project_id`.

## Removing project IDs

After uninstalling, the IDs stay on the files; without limits they do nothing. To remove them:

```bash
chattr -R -p 0 /path                  # ext4 and XFS
zfs project -C -r /path               # ZFS
```

Do this only for directories DirAdvisor no longer measures.
