# Configuration

All settings are in one YAML config. It is read once at start; a change needs a restart.

## Where the config comes from

In this order, the first one set wins:

1. `hatchlens -config <file>`
2. the file named in `HATCHLENS_CONFIG_FILE`, e.g. `/data/config.yml` on the data volume
3. the YAML itself in `HATCHLENS_CONFIG`, e.g. inline in the compose file or Portainer stack

None set: the defaults (Docker volumes and image checks of all containers). A named file that does not
exist is an error. HatchLens never writes a config file. `-config` is the only flag; an unknown flag exits with code 2.

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
disable: [ ]                       # functions to switch off: directory, volume, image, cpu, ram, disk, net

listen_address: ":8080"            # /metrics; the default changes with 1.0
log_level: info                    # debug, info, warn, error
log_format: logfmt                 # logfmt or json
storage_path: /data                # directory of state.json and images.json; Windows C:\data
rootfs: /host                      # where the host root filesystem is mounted
docker_socket: /var/run/docker.sock   # Windows \\.\pipe\docker_engine
host_socket: \\.\pipe\hatchlens-host     # Windows only: the HatchLens Host service
reconcile_interval: 60s            # discovery, ownership, ID assignment, image comparison; also on Docker events
admin_action_log_interval: 1h      # repeat pending admin actions in the log
walk_files_per_second: 0           # limit for the project ID walks, 0 = no limit, max 1000000000 (always idle I/O)
project_ids:                       # range for new project IDs
  from: 100000
  to: 199999

exclude:                           # for every function the object appears in
  volumes: [ "buildx_*" ]          # volume names, glob: not measured, project ID reset
  containers: [ "buildx_buildkit_*" ]   # container names, glob: no image checks, its volumes excluded
  images: [ "ghcr.io/example/*" ]  # image references, glob (as written or normalized): as containers

container_labels:                  # copy labels of the container: the owner container of a volume,
  - com.example.team               # the container itself for the image metrics;
  - { from: com.example.backup, to: backup }   # as container_label_com_example_team or under `to`
volume_labels:                     # copy labels of the volume (volumes only)
  - com.example.retention          # as volume_label_com_example_retention
labels:                            # static labels by rule, applied in order
  - match: { stack: mailcow }               # volumes, directories and image metrics of the stack
    labels: { team: mail }
  - match: { volume: "nextcloud_*" }        # volumes only
    labels: { backup: daily }
  - match: { image: "ghcr.io/motwok/*" }    # image metrics only
    labels: { own: "true" }
  - match: { path: "/srv/*" }               # volumes and directories
    labels: { tier: cold }

directories:                       # Windows: host paths like C:\data, through the host service
  - path: /srv/backup
    labels: { stack: backup, owner: borg, tier: cold }   # win over rules
  - path: /home/*                  # every match is its own entry
    labels: { user: "$1" }
  - path: ${DATA_ROOT:-/srv}/archive

images:
  check_interval: 24h              # registry check per image reference
  forget_after: 168h               # keep the state of removed containers this long
  registries:                      # credentials; other registries are asked anonymously
    - host: ghcr.io
      username: ${GHCR_USER}
      password: ${GHCR_TOKEN}
    - host: docker.io              # Docker Hub
      username: ${HUB_USER:-}
      password: ${HUB_TOKEN:-}
```

### Switching functions off

`disable` lists the functions that do not run: `directory` (the entries under `directories`), `volume`
(Docker volumes), `image` (image checks), and the per-container usage `cpu`, `ram`, `disk` and `net`
(see [Metrics.md](Metrics.md#container-usage)). Any other name is an error. A disabled function reports
nothing and changes nothing; project IDs it set stay on the files and in the state, and it continues
without a new walk when it is switched on again.

All functions except `directory` need the Docker socket. An empty `docker_socket` while one of them is
not disabled is an error ("docker_socket is empty, but volume, image, cpu, ram, disk, net need it; add
them to disable"), so a forgotten socket mount does not go unnoticed.

On Windows `directory` and `volume` go through the HatchLens Host service on the host (see
[Install.md](Install.md#windows)) at `host_socket`. An empty `host_socket` while one of them is not
disabled is an error. Without the service running, the entries show the admin action
`install_host_service`.

As the Windows service `HatchLens` on the host (see [Install.md](Install.md#windows-without-a-container-hatchlens-as-a-service))
the host part runs in the same process, so `host_socket` is not used; the config is
`%ProgramData%\HatchLens\config.yml` if it exists, and `storage_path` defaults to
`%ProgramData%\HatchLens`.

Excluded containers (`exclude.containers`, `exclude.images`, label `de.mo2000.hatchlens.enable=false`)
get no usage metrics either. The rules under `labels`, the self-service labels and `container_labels`
apply to the usage metrics exactly as to the image metrics; their own labels are `container`, `stack`,
`network` and `interface`, so these names cannot be set.

### Loading

- Unknown keys are an error.
- An invalid config: HatchLens exits with code 1; the log says why. An error about a value names the key,
  never the value, so credentials do not end up in the log.
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
- In name globs (`exclude.*`, `match.stack`, `match.volume`, `match.container`, `match.image`) `*`
  matches any characters, including `/`.
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

The rules under `labels` and the allowlist `container_labels` are shared by all functions. A rule matches
a series only if every key in its `match` exists for that series and matches:

| `match` key | exists for |
|---|---|
| `stack` | volumes, directories, image metrics |
| `container` | volumes (the owner container), image metrics (the container) |
| `volume` | volumes |
| `image` | image metrics (the reference as written or normalized) |
| `path` | volumes, directories |

HatchLens's own labels (`path`, `source`, `volume`, `stack`, `owner`, `state`, `project_id` on the
directory metrics; `container`, `image`, `reference`, `stack`, `state`, `mode` on the image metrics) are
always set by HatchLens; rules and the allowlist cannot set any of them. Only for configured directories
`stack` and `owner` come from `directories[].labels`.

Additional labels on Docker volumes, highest priority first:

1. rules under `labels` (the last matching rule wins per label);
2. Docker labels `de.mo2000.hatchlens.label.<name>=<value>` on the owner container or on the volume
   (container wins), set in the compose file by the stack author;
3. the allowlists `container_labels` and `volume_labels`.

Configured directories: their own `labels` first, then matching rules.

Names from Docker labels get every character outside `[a-zA-Z0-9_]` replaced by `_`; a name that is
still no valid Prometheus label name (empty, starting with a digit or with `__`) is dropped. Label values
that are empty are left out. Every additional label adds series, so keep the allowlists short.

### Ownership

- The owner is a container that mounts the volume read-write; its name is the `owner`
  label. With several writers the first one stays the owner.
- `stack` is the Compose project in the volume's own label; for volumes without one, the project of the
  writers. Volumes written by several stacks without such a label count as `unused`.
- Owners and stacks are remembered, so a stack that is down keeps its volumes (`state="stopped"`).

### Removing entries

- A directory removed from `directories`, or a volume newly excluded (by `exclude.volumes`, or because
  its owner container is excluded), gets its project ID reset (to 0, or to the ID of an enclosing entry)
  and then the ID is released. A volume of another owner stays, even if an excluded container mounts it.
- A directory that is missing on disk is reported as `path_missing` and keeps its ID, because a missing
  mount looks the same as a deleted directory.
- `disable: [ volume ]` or `[ directory ]` stops reporting them but keeps their IDs.

## Image checks

For every container whose image comes from a registry, HatchLens reports whether the registry has a
newer image for the reference the container was started with. Nothing is pulled or restarted.

### Intervals

- **`reconcile_interval`:** how often the containers are read and compared with the known registry
  digests. Docker events (container created, started, stopped, removed; image pulled, tagged, removed)
  trigger a reconcile at once, so this is only the fallback.
- **`images.check_interval`:** how often the registry is asked per image reference. A new reference is
  checked at once; after that the checks of all references are spread over the interval. Checks go out
  one at a time, at least 10 seconds apart per registry.
- A failed check is repeated after 5 minutes, doubling up to `check_interval`. A registry that answers
  429 (rate limit) is not asked for 5 minutes, doubling up to `check_interval`.

### Docker Hub pull limit

HatchLens asks the registry through the Docker daemon (`GET /distribution/<reference>/json`). **For
Docker Hub, each check counts as one pull** against the pull limit of the host's IP or account. With
the default `check_interval` of 24h that is one pull per Hub image per day. Add Docker Hub credentials
under `images.registries` if the anonymous limit is tight; a registry mirror configured in the daemon is
used too.

### Registries

- `host` is the registry part of the image reference (`ghcr.io`, `registry.example.com:5000`);
  `docker.io` (or `index.docker.io`) is Docker Hub.
- An entry with a non-empty `password` sends the credentials with every check of that registry. An empty
  password (as `${HUB_TOKEN:-}` gives when the variable is unset) or no entry means anonymous.
- Credentials never appear in logs, metrics or the state. Keep them out of the config with `${VAR}`.
- Invalid: a duration of 0 or less; a registry without `host`, or the same `host` twice.

### Excluding containers

A container is not reported at all if:

- its name matches `exclude.containers`;
- its image reference matches `exclude.images`, as written in the container (`nginx`) or normalized
  (`docker.io/library/nginx:latest`);
- it has the Docker label `de.mo2000.hatchlens.enable=false`.

The same container is also excluded from the volume metrics: volumes it owns are not measured.

### Additional labels on the image metrics

The container series (`update_available`, its `since`, `pulled`) can carry additional labels, from
three sources; when two give the same name, the first one in this list wins:

1. **Rules** under `labels` (see Labels above); for each label the last matching rule wins.
2. **Self-service:** a Docker label `de.mo2000.hatchlens.label.<name>=<value>` on the container becomes
   the label `<name>`.
3. **Allowlist** `container_labels`: the Docker label `from` is copied as `to`, or, without `to`,
   as `container_label_<from>` with every character other than letters, digits and `_` replaced by `_`.

Own labels (`container`, `image`, `reference`, `stack`, `state`, `mode`) cannot be set. Self-service
and allowlist names that are invalid or own labels are skipped with a warning in the log. Labels with an
empty value are left out.

### State

`<storage_path>/images.json` keeps, per image reference, the last registry digest and when the next
check is due, per container (for Swarm tasks per service and slot) the `since` time of an available
update, and the rate limit backoff per registry. So a restart neither loses `since` nor asks the
registries again. The file is written atomically after each change. An unreadable file or one written
by a newer version is renamed to `images.json.bak` and the image part starts empty. If the directory is
not writable, the state stays in memory and an error is logged once per hour.

Entries of removed containers and references are dropped after `forget_after`; a container that comes
back before that continues where it left off.
