# Installing HatchLens

1. Enable project quotas on every filesystem you want to measure (once, by the admin).
2. Run the container.
3. Add the scrape job to Prometheus.
4. Optional: add directories outside Docker volumes to the config file.

The image update checks of all containers need none of this: they only use the Docker socket and are on
by default (see [Configuration.md](Configuration.md#image-checks)). For Windows hosts see
[Windows](#windows).

## Requirements

- Linux 5.14 or newer, `amd64` or `arm64`.
- Docker Engine (rootful) with API 1.41 or newer (Docker 20.10+).
- The data to measure on ext4, XFS or ZFS (OpenZFS 0.8+) with project quotas enabled (next section).

HatchLens never changes your filesystems, mounts or pools. It only sets project IDs on the directories
it measures. Whatever is missing it reports as `hatchlens_admin_action_needed` and in the log, with the
exact command for your host.

## 1. Enable project quotas

Quotas are used for accounting only; HatchLens never sets limits.

### ext4

The `project` and `quota` features are switched on with `tune2fs` on the **unmounted** filesystem. For
the filesystem holding `/var/lib/docker`:

```bash
systemctl stop docker docker.socket containerd
umount /var/lib/docker
tune2fs -O project,quota /dev/<device>
mount /var/lib/docker
systemctl start containerd docker
```

- Needs e2fsprogs 1.43+ and an inode size of 256: `tune2fs -l /dev/<device> | grep "Inode size"`.
- Check: `tune2fs -l /dev/<device> | grep -i features` lists `quota` and `project`.
- The accounting is active on every later mount; no fstab option is needed.
- If the data is on the root filesystem, the step needs a rescue system, because the root filesystem
  cannot be unmounted.

### XFS

Project quotas are a mount option. Add `prjquota` to the filesystem's line in `/etc/fstab`, then
unmount and mount it again (a remount is not enough):

```
/dev/<device>  /srv  xfs  defaults,prjquota  0  2
```

- For the root filesystem: add `rootflags=prjquota` to the kernel command line (for example
  `GRUB_CMDLINE_LINUX` in `/etc/default/grub`, then `update-grub`) and reboot.
- Check: `xfs_quota -x -c state <mountpoint>` shows project quota accounting `ON`.
- XFS v4 (`xfs_info <mountpoint> | grep crc` shows `crc=0`) cannot use group and project quotas at the
  same time; drop group quota or move to XFS v5.
- With overlay2 on XFS and `prjquota`, Docker also uses project IDs for its layers. HatchLens never
  touches `overlay2` and skips IDs already in use.

### ZFS

```bash
zpool set feature@project_quota=enabled <pool>
```

- Irreversible: OpenZFS older than 0.8 cannot import the pool afterwards.
- The ZFS tools (`zfs`, `zpool`) must be installed on the host; HatchLens runs the host's own tools,
  read-only (`zfs projectspace`, `zfs version`, `zpool get`).
- Datasets that existed before are upgraded in the background; until then their values can be
  incomplete.

## 2. Run the container

Compose file, also usable as a Portainer stack:

```yaml
services:
  hatchlens:
    image: ghcr.io/motwok/hatchlens:latest
    container_name: hatchlens
    restart: unless-stopped
    cap_add: [ SYS_ADMIN, DAC_READ_SEARCH ]
    # devices: [ /dev/zfs ]          # only on ZFS hosts
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /:/host:rslave               # project IDs are set on host directories
      - hatchlens-data:/data        # state.json, images.json, config.yml if used
    ports:
      - "8080:8080"                  # use another host port if cAdvisor already has 8080
    # environment:
    #   HATCHLENS_CONFIG_FILE: /data/config.yml   # or the YAML inline in HATCHLENS_CONFIG
    #   DATA_ROOT: /srv                           # variables used as ${VAR} in the config
volumes:
  hatchlens-data:
```

- No config is needed for Docker volumes: the defaults match the mounts above (`/data`, `/host`, the
  Docker socket). Point `HATCHLENS_CONFIG_FILE` at a file or put the YAML into `HATCHLENS_CONFIG`
  ([Configuration.md](Configuration.md)); a change needs a restart of the container.
- `environment:` also sets the variables the config refers to as `${VAR}`.
- There is no `healthcheck:`: the image has no shell or `curl`. Prometheus sees the health through `up`
  and `/-/ready`.
- `SYS_ADMIN` is needed to read project quotas, `DAC_READ_SEARCH` to walk directories with any
  permissions. If your setup blocks them (seccomp, AppArmor), `privileged: true` works as a fallback.
- The host filesystem must be mounted read-write at `/host`: HatchLens sets project IDs there.
- `/data` keeps `state.json` (assigned IDs and remembered owners) and `images.json` (registry results and
  since times of the image checks). Keep it across updates.
- To leave a container out of the image checks, give it the label `de.mo2000.hatchlens.enable=false` or
  use `exclude` in the config. Each check of a Docker Hub image counts as one pull, by default one
  per image and day.
- The default port is **8080** before 1.0 and changes with 1.0, see the README.

The same with `docker run`:

```bash
docker run -d --name hatchlens --restart unless-stopped \
  --cap-add SYS_ADMIN --cap-add DAC_READ_SEARCH \
  -v /var/run/docker.sock:/var/run/docker.sock:ro -v /:/host:rslave -v hatchlens-data:/data \
  -p 8080:8080 ghcr.io/motwok/hatchlens:latest
```

## 3. Prometheus

```yaml
scrape_configs:
  - job_name: hatchlens
    static_configs:
      - targets: ['<host>:8080']
```

A scrape interval of one minute is fine; reading the quotas costs a few milliseconds regardless of the
number of files.

Volumes of containers without a Compose project have no `stack` label. To show a value instead, add a
relabel rule to the job:

```yaml
    metric_relabel_configs:
      - source_labels: [__name__, stack]
        regex: 'hatchlens_directory_(bytes|inodes);'
        target_label: stack
        replacement: 'no stack'
```

## 4. Check

```bash
curl -s http://<host>:8080/-/ready          # 200 after the first successful reconcile
curl -s http://<host>:8080/metrics | grep -E 'directory_bytes|admin_action|unmeasurable|container_image'
docker logs hatchlens
```

- The registry results follow `/-/ready` one check at a time (see
  [Configuration.md](Configuration.md#intervals)); until then a container shows `mode="pending"`.

- Right after the start `hatchlens_directory_assigning` is 1 for new directories: HatchLens walks them
  once to set the project ID (with idle I/O priority). Until that is done the values are too low.
- Every `hatchlens_admin_action_needed` series names a step for you; the log has the command.

## Directories outside Docker volumes

Bind mounts and other directories are measured only if listed in the config (see
[Configuration.md](Configuration.md)), e.g. `/data/config.yml` with `HATCHLENS_CONFIG_FILE: /data/config.yml`:

```yaml
directories:
  - path: /srv/backup
    labels: { stack: backup }
  - path: /home/*
    labels: { user: "$1" }
```

The image has no shell; copy the file into the volume with `docker cp config.yml hatchlens:/data/`, or
mount a host directory at `/data` instead of the named volume. Restart the container to apply changes.

## Windows

Docker hosts with Windows containers (Windows Server 2022 and 2025; a Server 2019 variant is built but
not tested). The same image tag works: Docker picks the variant for the host's Windows version.

A Windows container cannot see the host's file systems or its network stack, so two things need the
**HatchLens Host** service on the host:

- sizes of Docker volumes and configured directories (NTFS, from one walk plus the USN journal);
- traffic per Docker network (the Docker stats API names networks only by an HNS endpoint ID).

Without it the image checks and the per-container CPU, memory, disk and network sums still work, and
the volumes are reported with the admin action `install_host_service`.

1. Install `hatchlens-host-X.Y.Z.msi` from the [release](https://github.com/motwok/HatchLens/releases)
   on the host (`msiexec /i hatchlens-host-X.Y.Z.msi /qn`). It installs the service `HatchLensHost`
   (runs as LocalSystem, starts automatically) and has no configuration: the container tells it which
   directories to measure. Its log is `%ProgramData%\HatchLens\hatchlens-host.log`.
2. Mount both named pipes into the container:

```yaml
services:
  hatchlens:
    image: ghcr.io/motwok/hatchlens:latest
    container_name: hatchlens
    restart: unless-stopped
    volumes:
      - type: npipe
        source: \\.\pipe\docker_engine
        target: \\.\pipe\docker_engine
      - type: npipe                    # HatchLens Host service
        source: \\.\pipe\hatchlens-host
        target: \\.\pipe\hatchlens-host
      - type: volume
        source: hatchlens-data
        target: C:\data                # state.json, images.json, config.yml if used
    ports:
      - "8080:8080"                    # default port before 1.0, changes with 1.0
    # environment:
    #   HATCHLENS_CONFIG_FILE: C:\data\config.yml
volumes:
  hatchlens-data:
```

- The image runs as `ContainerAdministrator`, because both pipes are open to administrators only.
- No capabilities, no `privileged` and no host paths besides the pipes are needed.
- Configured directories are host paths like `C:\data\archive` and are reported in that form.
- The NTFS volume needs a USN journal (on by default for the system drive); otherwise the admin action
  `enable_usn_journal` gives the `fsutil usn createjournal` command.
- Updating the MSI keeps the service; after a reinstall the container registers its directories again.
- To remove it: `msiexec /x hatchlens-host-X.Y.Z.msi /qn`.

## Update and rollback

- `:latest` gets every release; redeploy the stack to update. See [RELEASING.md](../RELEASING.md),
  including the automatic dependency releases.
- To stay on a version, use `ghcr.io/motwok/hatchlens:X.Y.Z`. Version tags are never rebuilt.
- HatchLens reports its own container like every other one, so a stuck update of HatchLens itself shows
  up too.

## Uninstall

Stop and remove the container and the data volume. The project IDs stay on the files; without quota
limits they have no effect. To remove them, see [Troubleshooting.md](Troubleshooting.md#removing-project-ids).
