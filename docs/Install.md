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
it measures. The one exception is a command an administrator runs by hand for Docker Desktop
([`hatchlens-host enable-quota`](#docker-desktop)). Whatever is missing it reports as `hatchlens_admin_action_needed` and in the log, with the
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
      - "8080:8080"                  # use another host port if 8080 is taken
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

### Windows without a container: HatchLens as a service

`hatchlens-X.Y.Z.msi` of the release installs HatchLens itself as the Windows service `HatchLens`
(LocalSystem, automatic start). It does everything in one process: image checks, container usage,
Docker volumes and directories, traffic per Docker network; the HatchLens Host service is not needed
next to it.

```powershell
msiexec /i hatchlens-X.Y.Z.msi /qn
```

- Config: `%ProgramData%\HatchLens\config.yml` if it exists (or `HATCHLENS_CONFIG_FILE` /
  `HATCHLENS_CONFIG` in the service's environment), else the defaults. A change needs a restart of the
  service (`Restart-Service HatchLens`).
- State: `%ProgramData%\HatchLens` unless `storage_path` is set. Log:
  `%ProgramData%\HatchLens\hatchlens.log`, started fresh after 10 MB.
- `/metrics` on port 8080 of the host (`listen_address`); open it in the Windows firewall for the
  Prometheus server, e.g. `New-NetFirewallRule -DisplayName HatchLens -Direction Inbound -Protocol TCP
  -LocalPort 8080 -Action Allow`.
- `%ProgramData%\HatchLens` is created by the MSI for administrators and SYSTEM only; the service refuses
  a directory or `config.yml` owned by anyone else.
- Updating: install the newer MSI; removing: `msiexec /x hatchlens-X.Y.Z.msi /qn` (config and state in
  `%ProgramData%\HatchLens` stay).

## Docker Desktop

Docker Desktop on Windows 10 and 11 runs Windows or Linux containers; HatchLens works in both modes.

**Windows containers:** as on Windows Server ([Windows](#windows)), with the HatchLens Host service.
With Hyper-V isolation, the default on Windows 10 and 11, the memory values are those of the
container's utility VM.

**Linux containers:** the Linux image as in [Run the container](#2-run-the-container). Image checks,
container usage and traffic per Docker network work as on Linux. The differences:

- Mount the host root without `rslave` (`- /:/host`); Docker Desktop refuses it.
- Docker volumes live on the ext4 data disk of Docker Desktop, which has no project quota feature
  (admin action `enable_project_quota_feature`). The HatchLens Host MSI brings a command that enables
  it, once, as administrator:

  ```powershell
  # quit Docker Desktop first
  wsl --shutdown
  & "$env:ProgramFiles\HatchLens\hatchlens-host.exe" enable-quota
  ```

  It attaches the data disk (`%LOCALAPPDATA%\Docker\wsl\data\ext4.vhdx`, in newer versions
  `%LOCALAPPDATA%\Docker\wsl\disk\docker_data.vhdx`; `-vhd PATH` for another one) to a temporary WSL
  distribution with e2fsprogs, checks it with `e2fsck -f -p`, runs `tune2fs -O project,quota` and
  removes the distribution again. Then start Docker Desktop. A copy of the disk beforehand does no
  harm. A reset of Docker Desktop ("Clean / Purge data", "Reset to factory defaults") creates a new
  disk without the feature; the admin action shows up again.
- Directories on Windows drives (`C:\...`) are measured by the HatchLens Host service on the Windows
  host, reached over TCP on the loopback address, which Docker Desktop forwards as
  `host.docker.internal`. Install the MSI with a port, as administrator:

  ```powershell
  msiexec /i hatchlens-host-X.Y.Z.msi /qn TCP=127.0.0.1:8091
  ```

  The service creates `%ProgramData%\HatchLens\host-token` (readable by administrators only, refused
  when owned by anyone else) and
  accepts TCP requests only with that token. Pass it to the container, for example as an environment
  variable, and add to the config:

  ```yaml
  host_socket: tcp://host.docker.internal:8091
  host_token: ${HATCHLENS_HOST_TOKEN}
  directories:
    - path: 'C:\data'                # Windows path: through the host service
    - path: /srv/x                   # anything else: project quotas in the Docker Desktop VM
  ```

  Pass `TCP=...` again when updating the MSI; without it the service listens on its pipe only.

## Update and rollback

- `:latest` gets every release; redeploy the stack to update. See [RELEASING.md](../RELEASING.md),
  including the automatic dependency releases.
- To stay on a version, use `ghcr.io/motwok/hatchlens:X.Y.Z`. Version tags are never rebuilt.
- HatchLens reports its own container like every other one, so a stuck update of HatchLens itself shows
  up too.

## Uninstall

Stop and remove the container and the data volume. The project IDs stay on the files; without quota
limits they have no effect. To remove them, see [Troubleshooting.md](Troubleshooting.md#removing-project-ids).
