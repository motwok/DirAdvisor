# Configuration

All settings are in one YAML config. It is read once at start; a change needs a restart.

## Where the config comes from

In this order, the first one set wins:

1. `hatchlens -config <file>`
2. the file named in `HATCHLENS_CONFIG_FILE`, e.g. `/data/config.yml` on the data volume
3. the YAML itself in `HATCHLENS_CONFIG`, e.g. inline in the compose file or Portainer stack

None set: the defaults (Docker volumes and image checks of all containers). A named file that does not exist is an error. HatchLens
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
  storage_path: /data              # directory of state.json and images.json; Windows C:\data
  rootfs: /host                    # where the host root filesystem is mounted
  reconcile_interval: 60s          # discovery, ownership, ID assignment, image comparison; also on Docker events
  admin_action_log_interval: 1h    # repeat pending admin actions in the log
  walk_files_per_second: 0         # limit for the project ID walks, 0 = no limit, max 1000000000 (always idle I/O)
  project_ids:                     # range for new project IDs
    from: 100000
    to: 199999

docker:
  socket: /var/run/docker.sock     # Windows \\.\pipe\docker_engine; empty: no Docker, no image checks
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

images:
  enabled: true                    # image update checks of all containers
  check_interval: 24h              # registry check per image reference
  forget_after: 168h               # keep the state of removed containers this long
  exclude:
    containers: [ "buildx_buildkit_*" ]   # container names, glob
    images: [ "ghcr.io/example/*" ]       # image references, glob (as written or normalized)
  registries:                      # credentials; other registries are asked anonymously
    - host: ghcr.io
      username: ${GHCR_USER}
      password: ${GHCR_TOKEN}
    - host: docker.io              # Docker Hub
      username: ${HUB_USER:-}
      password: ${HUB_TOKEN:-}
  container_labels:                # Docker labels copied to the image metrics (default: none)
    - com.example.team             # becomes container_label_com_example_team
    - { from: com.example.owner, to: owner_team }
  labels:                          # static labels by rule
    - match: { stack: mailcow }
      labels: { team: mail }
    - match: { image: "ghcr.io/example/*" }
      labels: { own: "true" }
```

On Windows only the image checks run: `directories` is an error there and the `docker` volume keys are
ignored.

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
2. Docker labels `de.mo2000.hatchlens.label.<name>=<value>` on the owner container or on the volume (container
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

## Image checks

For every container whose image comes from a registry, HatchLens reports whether the registry has a
newer image for the reference the container was started with. Nothing is pulled or restarted.

### Intervals

- **`global.reconcile_interval`:** how often the containers are read and compared with the known registry
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

- its name matches `images.exclude.containers`;
- its image reference matches `images.exclude.images`, as written in the container (`nginx`) or
  normalized (`docker.io/library/nginx:latest`);
- it has the Docker label `de.mo2000.hatchlens.enable=false`.

In these globs `*` matches any characters, including `/`.

### Additional labels on the image metrics

The container series (`update_available`, its `since`, `pulled`) can carry additional labels, from
three sources; when two give the same name, the first one in this list wins:

1. **Rules** under `images.labels`: the keys of `match` (`stack`, `container`, `image`) are globs and
   all given keys must match; for each label the last matching rule in the config wins.
2. **Self-service:** a Docker label `de.mo2000.hatchlens.label.<name>=<value>` on the container becomes
   the label `<name>`.
3. **Allowlist** `images.container_labels`: the Docker label `from` is copied as `to`, or, without `to`,
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
