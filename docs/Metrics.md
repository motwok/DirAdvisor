# Metrics

All metrics have the prefix `hatchlens_`; all are gauges except `reconcile_errors_total`.

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
- Values are read from the filesystem at scrape time; scrapes within 2 s of each other share one
  reading.
- `state` (volumes only): `running` (owner runs), `stopped` (owner or stack known, not running),
  `unused`.
- Labels that change (`owner`, `state`, `project_id`) are on `directory_info`, so the value series stay
  continuous when a stack stops. Join them when needed:

  ```promql
  hatchlens_directory_bytes * on(instance, path) group_left(state, owner) hatchlens_directory_info
  ```

- `owner` is the container name, the same value as cAdvisor's `name` label. Volume bytes per container,
  ready to join with cAdvisor series on `name`:

  ```promql
  label_replace(
    sum by (instance, owner) (
      hatchlens_directory_bytes * on(instance, path) group_left(owner) hatchlens_directory_info
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
| `docker_root_unknown` | Docker never answered since the first start, so its data directory is unknown | make the Docker socket reachable, or add `volume` to `disable` |

## Image checks: containers

| Metric | Labels | Value | Present |
|---|---|---|---|
| `container_image_update_available` | `container`, `image`, `stack`, additional | 1: the registry has a newer image for the reference | `mode="checked"` |
| `container_image_update_available_since_timestamp_seconds` | same | when `update_available` went to 1 | while `update_available` is 1 |
| `container_image_update_pulled` | same | 1: the reference already points to another local image (pulled, container not recreated) | containers with a reference, not Swarm tasks |
| `container_image_info` | `container`, `reference`, `state`, `mode` | 1 | every reported container |

- `container`: the container name, the same value as cAdvisor's `name` label.
- `image`: the reference as written when the container was created (`nginx:1.27`). For Swarm tasks
  without the digest Swarm adds (`name:tag@sha256:...` shows as `name:tag`).
- `reference`: the normalized reference (`docker.io/library/nginx:1.27`); it joins `container_image_info`
  with the per-reference series below. No label for `mode="local"`.
- `stack`: the Compose project, for Swarm tasks the stack; no label without one.
- `state`: `running` (also paused and restarting) or `stopped` (created, exited, dead).
- Additional labels: see [Configuration.md](Configuration.md#additional-labels-on-the-image-metrics).

### Modes

| `mode` | Meaning |
|---|---|
| `checked` | a registry digest is known; `update_available` compares it with the running image |
| `pending` | not checked yet |
| `error` | the check failed and no registry digest is known; see `image_check_error` |
| `pinned` | the reference has a digest (`image@sha256:...`); nothing to check |
| `local` | the container was created from an image ID; nothing to check |

A failed check does not drop a known registry digest: the container stays `checked` and
`image_check_error` shows the failure.

## Image checks: references

| Metric | Labels | Value | Present |
|---|---|---|---|
| `image_check_success` | `reference` | 1 if the last registry check succeeded, else 0 | after the first check |
| `image_check_error` | `reference`, `reason` | 1 | while the last check failed |
| `image_check_timestamp_seconds` | `reference` | time of the last successful check | after the first success |

`reason`:

| `reason` | Registry answer | Usual cause |
|---|---|---|
| `not_found` | 404 | locally built image, tag deleted |
| `denied` | 401, 403 | private image without credentials; Docker Hub also answers 403 for a repository that does not exist |
| `rate_limited` | 429 | pull limit reached |
| `unreachable` | anything else, no answer within 30 seconds | network, registry down |

These series exist only while a reported container uses the reference.

A scrape answers from the last results; it never triggers a Docker or registry request.

## HatchLens itself

| Metric | Labels | Value |
|---|---|---|
| `reconcile_duration_seconds` | | duration of the last reconciles of both parts, added up |
| `reconcile_errors_total` | | counter of failed reconciles of both parts (Docker unreachable, ...) |
| `build_info` | `version`, `revision`, `goversion` | 1 |
| `build_timestamp_seconds` | | build time of the binary |

## Example alerts

```yaml
groups:
  - name: hatchlens
    rules:
      - alert: HatchLensDown
        expr: up{job="hatchlens"} == 0
        for: 5m
      - alert: VolumeGrowsFast
        expr: delta(hatchlens_directory_bytes[30m]) > 2e9
      - alert: HatchLensAdminAction
        expr: hatchlens_admin_action_needed == 1
        annotations:
          summary: '{{ $labels.action }} on {{ $labels.mountpoint }}, see the HatchLens log'
      - alert: DirectoryUnmeasurable
        expr: hatchlens_directory_unmeasurable == 1
        for: 15m
      - alert: HatchLensReconcileFailing
        expr: increase(hatchlens_reconcile_errors_total[15m]) > 0
      - alert: ImageUpdateNotRolledOut
        # Without the last part it also fires for containers stopped on purpose.
        expr: |
          (hatchlens_container_image_update_available == 1
            and time() - hatchlens_container_image_update_available_since_timestamp_seconds > 2 * 86400)
          and on(instance, container) hatchlens_container_image_info{state="running"}
      - alert: ImagePulledNotRecreated
        expr: hatchlens_container_image_update_pulled == 1
        for: 1h
      - alert: ImageCheckFailing
        # not_found and denied are mostly local images or missing credentials: exclude or configure them.
        expr: hatchlens_image_check_error{reason=~"rate_limited|unreachable"} == 1
        for: 1d
      - alert: ImageCheckStale
        # With check_interval 24h.
        expr: time() - hatchlens_image_check_timestamp_seconds > 3 * 86400
```

Thresholds and windows are examples; choose your own.
