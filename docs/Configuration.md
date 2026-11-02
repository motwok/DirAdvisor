# Configuration

All settings are in one YAML config. It is read once at start; a change needs a restart.

## Where the config comes from

In this order, the first one set wins:

1. `hatchlens -config <file>`
2. the file named in `HATCHLENS_CONFIG_FILE`, e.g. `/data/config.yml` on the data volume
3. the YAML itself in `HATCHLENS_CONFIG`, e.g. inline in the compose file or Portainer stack

None set: the defaults (Docker volumes only). A named file that does not exist is an error. HatchLens
never writes a config file. `-config` is the only flag; an unknown flag exits with code 2.

Inline in a compose file:

```yaml
    environment:
      HATCHLENS_CONFIG: |
        directories:
          - path: /srv/backup
```

Compose replaces `${VAR}` in the compose file itself; write `$${VAR}` and `$$1` to hand them to HatchLens.

## Keys

Everything is optional.

```yaml
global:
  listen_address: ":8080"          # /metrics; the default changes with 1.0
  log_level: info                  # debug, info, warn, error
  log_format: logfmt               # logfmt or json
  storage_path: /data              # directory of state.json
  rootfs: /host                    # where the host root filesystem is mounted
  reconcile_interval: 60s          # discovery, ownership, ID assignment; also on Docker events
  admin_action_log_interval: 1h    # repeat pending admin actions in the log
  walk_files_per_second: 0         # limit for the project ID walks, 0 = no limit, max 1000000000 (always idle I/O)
  image_update_check:
    enabled: false                 # is the running image still the current :latest?
    interval: 24h
  project_ids:                     # range for new project IDs
    from: 100000
    to: 199999

docker:
  socket: /var/run/docker.sock     # empty: no Docker
  enabled: true                    # measure Docker volumes
  exclude:
    volumes: [ "buildx_*" ]        # volume names, glob
  container_labels:                # copy labels of the owner container
    - com.example.team             # as container_label_com_example_team
    - { from: com.example.backup, to: backup }
  volume_labels:                   # copy labels of the volume
    - com.example.retention        # as volume_label_com_example_retention
  labels:                          # static labels by rule, applied in order
    - match: { stack: mailcow }
      labels: { team: mail }
    - match: { volume: "nextcloud_*" }
      labels: { backup: daily }
    - match: { container: "paperless*", path: "/var/lib/docker/volumes/*/_data" }
      labels: { team: office }

directories:
  - path: /srv/backup
    labels: { stack: backup, owner: borg, tier: cold }
  - path: /home/*                  # every match is its own entry
    labels: { user: "$1" }
  - path: ${DATA_ROOT:-/srv}/archive
```

### Loading

- Unknown keys are an error.
- An invalid config: HatchLens exits with code 1; the log says why.
- Read once at start. `SIGHUP` is ignored.

### Environment variables

In string values: `${VAR}`, `${VAR:-default}` (default if unset or empty), `$$` for a literal `$`. A
variable without default that is not set makes the config invalid. `$1`, `$2`, ... are not variables; they
are glob matches (below). Variables are replaced after parsing, so a value cannot change the structure.

### Paths and globs

- Paths are host paths and absolute.
- `*` matches exactly one directory level and any characters in it (`/srv/data-*/files`). There is no
  `**`. Only directories match; symlinks are not followed.
- In label values `$1`, `$2`, ... are the parts matched by the `*`, in order.
- In name globs (`exclude.volumes`, `match.stack`, `match.volume`, `match.container`) `*` matches any
  characters.
- Never measured: `/` itself, `/var/lib/docker`, Docker's actual data root, `/var/lib/containerd`,
  `/proc`, `/sys`, `/dev`, `/run` and everything below them. Such a path in the config is an error; a
  glob match there is skipped silently.
- The same path twice: a Docker volume wins over a configured directory, among volumes the oldest, among
  configured directories the first in the file. The others are reported as `duplicate_path`.
- Directories inside other entries are measured on their own; the outer entry counts the rest
  (`hatchlens_directory_overlap`).

### What gets measured

- **Docker volumes** of the `local` driver, automatically, including volumes with bind options
  (`type: none`, `o: bind`, `device: <path>`); those are measured at `<path>`.
- **Bind mounts are not discovered.** Add them under `directories`.
- NFS/CIFS volumes and volumes of other drivers are reported as unmeasurable.

### Labels

HatchLens's own labels (`path`, `source`, `volume`, `stack`, `owner`, `state`, `project_id`) are always
set by HatchLens. Only for configured directories `stack` and `owner` come from `directories[].labels`.

Additional labels on Docker volumes, highest priority first:

1. `docker.labels` rules (the last matching rule wins per label);
2. Docker labels `hatchlens.label.<name>=<value>` on the owner container or on the volume (container
   wins), set in the compose file by the stack author;
3. the allowlists `container_labels` and `volume_labels`.

Names from Docker labels get every character outside `[a-zA-Z0-9_]` replaced by `_`; a name that is
still no valid Prometheus label name (empty, starting with a digit or with `__`) is dropped. Label values
that are empty are left out. Every additional label adds series, so keep the allowlists short.

### Ownership

- The owner is a container that mounts the volume read-write; its name is the same as cAdvisor's `name`
  label. With several writers the first one stays the owner.
- `stack` is the Compose project in the volume's own label; for volumes without one, the project of the
  writers. Volumes written by several stacks without such a label count as `unused`.
- Owners and stacks are remembered, so a stack that is down keeps its volumes (`state="stopped"`).

### Removing entries

- A directory removed from `directories`, or a volume newly excluded, gets its project ID reset (to 0, or
  to the ID of an enclosing entry) and then the ID is released.
- A directory that is missing on disk is reported as `path_missing` and keeps its ID, because a missing
  mount looks the same as a deleted directory.
- `docker.enabled: false` stops reporting volumes but keeps their IDs.
