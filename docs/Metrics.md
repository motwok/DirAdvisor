# Metrics

All metrics have the prefix `diradvisor_`; all are gauges except `reconcile_errors_total`.

## Directories

| Metric | Labels | Value |
|---|---|---|
| `directory_bytes` | `path`, `source`, `volume`, `stack`, additional labels | allocated bytes |
| `directory_inodes` | same | inodes |
| `directory_info` | `path`, `owner`, `state`, `project_id` | 1 |
| `directory_last_used_timestamp_seconds` | `path` | last time a running container used the volume |
| `directory_assigning` | `path` | 1 while the first project ID walk runs (values too low until 0) |
| `directory_unmeasurable` | `path`, `reason` | 1 |
| `directory_overlap` | `path` (outer), `inner` | 1 |

- `path` is the host path. `source` is `volume` or `config`. `volume` is the volume name (empty for
  configured directories).
- `state` (volumes only): `running` (owner runs), `stopped` (owner or stack known, not running),
  `unused`.
- Labels that change (`owner`, `state`, `project_id`) are on `directory_info`, so the value series stay
  continuous when a stack stops. Join them when needed:

  ```promql
  diradvisor_directory_bytes * on(instance, path) group_left(state, owner) diradvisor_directory_info
  ```

- `owner` is the container name, the same value as cAdvisor's `name` label. Volume bytes per container,
  ready to join with cAdvisor series on `name`:

  ```promql
  label_replace(
    sum by (instance, owner) (
      diradvisor_directory_bytes * on(instance, path) group_left(owner) diradvisor_directory_info
    ),
    "name", "$1", "owner", "(.+)")
  ```

## Problems and admin actions

| Metric | Labels | Value |
|---|---|---|
| `admin_action_needed` | `mountpoint`, `fstype`, `action` | 1 per missing step; the log has the command |
| `directory_unmeasurable` | `path`, `reason` | 1 |

| Reason / action | Meaning | Fix |
|---|---|---|
| `enable_project_quota_feature` | ext4 without project quota | `tune2fs -O project,quota`, see [Install.md](Install.md#ext4) |
| `mount_with_prjquota` | XFS mounted without `prjquota` | mount option, see [Install.md](Install.md#xfs) |
| `enable_zfs_project_quota` | pool feature off | `zpool set feature@project_quota=enabled <pool>` |
| `install_zfs_userland` | no `zfs`/`zpool` on the host | install the ZFS tools |
| `upgrade_kernel` | kernel older than 5.14 | upgrade the kernel |
| `filesystem_not_supported` | not ext4, XFS or ZFS | move the data or stop measuring it |
| `remote_filesystem` | NFS/CIFS | not measurable (no admin action) |
| `unsupported_volume_driver` | volume driver other than `local` | not measurable |
| `foreign_project_id` | directory already has someone else's project ID | see [Troubleshooting.md](Troubleshooting.md#foreign_project_id) |
| `path_missing` | directory (or a glob's parent) does not exist | check the mount or the config |
| `duplicate_path` | path already measured by another entry | see [Troubleshooting.md](Troubleshooting.md#duplicate_path) |
| `excluded_path` | bind volume pointing into an excluded tree | not measurable |
| `project_ids_exhausted` | no free ID in `project_ids` | widen the range |
| `quota_access_failed` | the quota state or usage of the filesystem could not be read | see [Troubleshooting.md](Troubleshooting.md#quota_access_failed) |
| `docker_root_unknown` | Docker never answered since the first start, so its data directory is unknown | make the Docker socket reachable, or set `docker.enabled: false` |

## DirAdvisor itself

| Metric | Labels | Value |
|---|---|---|
| `reconcile_duration_seconds` | | duration of the last reconcile |
| `reconcile_errors_total` | | counter of failed reconciles (Docker unreachable, ...) |
| `config_last_reload_successful` | | 0/1 |
| `build_info` | `version`, `revision`, `goversion` | 1 |
| `build_timestamp_seconds` | | build time of the binary |
| `image_update_check` | `mode` (`latest`, `pinned`, `disabled`, `unknown`) | 1 |
| `image_update_available` | | 0/1, only with `mode="latest"` |
| `image_update_available_since_timestamp_seconds` | | first time the newer image was seen |
| `image_check_success` | | 0/1 |

## Example alerts

```yaml
groups:
  - name: diradvisor
    rules:
      - alert: DirAdvisorDown
        expr: up{job="diradvisor"} == 0
        for: 5m
      - alert: VolumeGrowsFast
        expr: delta(diradvisor_directory_bytes[30m]) > 2e9
      - alert: DirAdvisorAdminAction
        expr: diradvisor_admin_action_needed == 1
        annotations:
          summary: '{{ $labels.action }} on {{ $labels.mountpoint }}, see the DirAdvisor log'
      - alert: DirectoryUnmeasurable
        expr: diradvisor_directory_unmeasurable == 1
        for: 15m
      - alert: DirAdvisorReconcileFailing
        expr: increase(diradvisor_reconcile_errors_total[15m]) > 0
      - alert: DirAdvisorUpdateNotRolledOut
        expr: >
          diradvisor_image_update_available == 1
          and time() - diradvisor_image_update_available_since_timestamp_seconds > 2 * 86400
```

Thresholds and windows are examples; choose your own.
