# Installing DirAdvisor

1. Enable project quotas on every filesystem you want to measure (once, by the admin).
2. Run the container.
3. Add the scrape job to Prometheus.
4. Optional: add directories outside Docker volumes to the config file.

## Requirements

- Linux 5.14 or newer, `amd64` or `arm64`.
- Docker Engine (rootful) with API 1.41 or newer (Docker 20.10+).
- The data to measure on ext4, XFS or ZFS (OpenZFS 0.8+) with project quotas enabled (next section).

DirAdvisor never changes your filesystems, mounts or pools. It only sets project IDs on the directories
it measures. Whatever is missing it reports as `diradvisor_admin_action_needed` and in the log, with the
exact command for your host.

## 1. Enable project quotas

Quotas are used for accounting only; DirAdvisor never sets limits.

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
- With overlay2 on XFS and `prjquota`, Docker also uses project IDs for its layers. DirAdvisor never
  touches `overlay2` and skips IDs already in use.

### ZFS

```bash
zpool set feature@project_quota=enabled <pool>
```

- Irreversible: OpenZFS older than 0.8 cannot import the pool afterwards.
- The ZFS tools (`zfs`, `zpool`) must be installed on the host; DirAdvisor runs the host's own tools,
  read-only (`zfs projectspace`, `zfs version`, `zpool get`).
- Datasets that existed before are upgraded in the background; until then their values can be
  incomplete.

## 2. Run the container

Compose file, also usable as a Portainer stack:

```yaml
services:
  diradvisor:
    image: ghcr.io/motwok/diradvisor:latest
    container_name: diradvisor
    restart: unless-stopped
    cap_add: [ SYS_ADMIN, DAC_READ_SEARCH ]
    # devices: [ /dev/zfs ]          # only on ZFS hosts
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /:/host:rslave               # project IDs are set on host directories
      - diradvisor-data:/data        # config.yml and state.json
    ports:
      - "8080:8080"                  # use another host port if cAdvisor already has 8080
    # environment:                   # variables used as ${VAR} in config.yml
    #   DATA_ROOT: /srv
    # command:                       # replaces the image's defaults: keep the first three flags
    #   - --config.file=/data/config.yml
    #   - --storage.path=/data
    #   - --path.rootfs=/host
    #   - --log.level=debug
volumes:
  diradvisor-data:
```

- No flags are needed: the image already starts with `--config.file=/data/config.yml`,
  `--storage.path=/data` and `--path.rootfs=/host`. A `command:` replaces these defaults completely, so
  repeat them when you add a flag (see the commented lines); otherwise DirAdvisor looks for its config in
  `/etc/diradvisor/` and for the host at `/`.
- `environment:` sets the variables that `config.yml` refers to as `${VAR}`.
- There is no `healthcheck:`: the image has no shell or `curl`. Prometheus sees the health through `up`
  and `/-/ready`.
- `SYS_ADMIN` is needed to read project quotas, `DAC_READ_SEARCH` to walk directories with any
  permissions. If your setup blocks them (seccomp, AppArmor), `privileged: true` works as a fallback.
- The host filesystem must be mounted read-write at `/host`: DirAdvisor sets project IDs there.
- `/data` keeps `state.json` (assigned IDs and remembered owners). Keep it across updates.
- The default port is **8080** before 1.0 and changes with 1.0, see the README.

The same with `docker run`:

```bash
docker run -d --name diradvisor --restart unless-stopped \
  --cap-add SYS_ADMIN --cap-add DAC_READ_SEARCH \
  -v /var/run/docker.sock:/var/run/docker.sock:ro -v /:/host:rslave -v diradvisor-data:/data \
  -p 8080:8080 ghcr.io/motwok/diradvisor:latest
```

## 3. Prometheus

```yaml
scrape_configs:
  - job_name: diradvisor
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
        regex: 'diradvisor_directory_(bytes|inodes);'
        target_label: stack
        replacement: 'no stack'
```

## 4. Check

```bash
curl -s http://<host>:8080/-/ready          # 200 after the first successful reconcile
curl -s http://<host>:8080/metrics | grep -E 'directory_bytes|admin_action|unmeasurable'
docker logs diradvisor
```

- Right after the start `diradvisor_directory_assigning` is 1 for new directories: DirAdvisor walks them
  once to set the project ID (with idle I/O priority). Until that is done the values are too low.
- Every `diradvisor_admin_action_needed` series names a step for you; the log has the command.

## Directories outside Docker volumes

Bind mounts and other directories are measured only if listed in `/data/config.yml`
(see [Configuration.md](Configuration.md)):

```yaml
directories:
  - path: /srv/backup
    labels: { stack: backup }
  - path: /home/*
    labels: { user: "$1" }
```

The image has no shell; copy the file into the volume with `docker cp config.yml diradvisor:/data/`, or
mount a host directory at `/data` instead of the named volume. Changes are picked up within one
reconcile interval.

## Update and rollback

- `:latest` gets every release; redeploy the stack to update. See [RELEASING.md](../RELEASING.md),
  including the automatic dependency releases.
- To stay on a version, use `ghcr.io/motwok/diradvisor:X.Y.Z`. Version tags are never rebuilt.

## Uninstall

Stop and remove the container and the data volume. The project IDs stay on the files; without quota
limits they have no effect. To remove them, see [Troubleshooting.md](Troubleshooting.md#removing-project-ids).
