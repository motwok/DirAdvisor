#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Integration test (spec TST-2). Runs as root on a CI runner.
# Usage: run.sh IMAGE COVERDIR
# No pipefail: grep -q closes the pipe early and the writer would fail with SIGPIPE.
set -eu

IMAGE=$1
COVER=$2
WORK=/tmp/da-it
DATA=$WORK/data
PORT=18080
URL=http://127.0.0.1:$PORT

mkdir -p "$WORK" "$DATA" "$COVER"
chmod 777 "$COVER"

step() { echo "== $*"; }
fail() {
  echo "FAIL: $*"
  echo "--- metrics"; curl -s "$URL/metrics" | grep -v '^#' || true
  echo "--- log"; docker logs hatchlens 2>&1 | tail -80 || true
  exit 1
}
metrics() { curl -sf "$URL/metrics"; }
# value METRIC LABELMATCH...: first sample value whose line contains all matches.
value() {
  local name=$1; shift
  metrics | grep "^hatchlens_${name}[{ ]" | {
    while read -r line; do
      ok=1
      for m in "$@"; do [[ $line == *"$m"* ]] || ok=0; done
      if [ $ok = 1 ]; then echo "${line##* }"; return; fi
    done
  }
}
wait_for() { # wait_for SECONDS DESCRIPTION COMMAND...
  local t=$1 d=$2; shift 2
  for _ in $(seq "$t"); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  fail "timeout: $d"
}
ge() { awk -v a="$1" -v b="$2" 'BEGIN{exit !(a+0 >= b+0)}'; }
bytes_ge() { local v; v=$(value directory_bytes "path=\"$1\""); [ -n "$v" ] && ge "$v" "$2"; }
no_walks() { ! metrics | grep -q '^hatchlens_directory_assigning{.*} 1$'; }
project_id() { metrics | grep "^hatchlens_directory_info{.*path=\"$1\"" | sed 's/.*project_id="\([0-9]*\)".*/\1/'; }
file_projid() {
  case "$1" in
    /mnt/it-xfs/*) xfs_io -r -c lsproj "$1" | sed 's/.*= *//' ;;
    *) lsattr -p "$1" | awk '{print $1}' ;;
  esac
}

start_da() {
  docker run -d --name hatchlens \
    --cap-add SYS_ADMIN --cap-add DAC_READ_SEARCH --device /dev/zfs \
    -e GOCOVERDIR=/cover -v "$COVER":/cover -e HATCHLENS_CONFIG_FILE=/data/config.yml \
    -v /var/run/docker.sock:/var/run/docker.sock:ro -v /:/host:rslave -v "$DATA":/data \
    -p "$PORT":8080 "$IMAGE" "$@" >/dev/null
  wait_for 60 "ready" curl -sf "$URL/-/ready"
}

step "filesystems"
loop() { truncate -s "$2" "$WORK/$1.img"; losetup -f --show "$WORK/$1.img"; }
EXT=$(loop ext4 400M); mkfs.ext4 -q -O project,quota "$EXT"; mkdir -p /mnt/it-ext4; mount "$EXT" /mnt/it-ext4
XFS=$(loop xfs 512M); mkfs.xfs -q "$XFS"; mkdir -p /mnt/it-xfs; mount -o prjquota "$XFS" /mnt/it-xfs
XNQ=$(loop xfsnoq 320M); mkfs.xfs -q "$XNQ"; mkdir -p /mnt/it-xfsnoq; mount "$XNQ" /mnt/it-xfsnoq
NOQ=$(loop noq 100M); mkfs.ext4 -q "$NOQ"; mkdir -p /mnt/it-noq; mount "$NOQ" /mnt/it-noq
modprobe zfs || { apt-get install -y -q "linux-modules-extra-$(uname -r)" && modprobe zfs; }
truncate -s 400M "$WORK/zfs.img"
zpool create -f -o feature@project_quota=enabled -m /mnt/it-zfs itpool "$WORK/zfs.img"

step "volumes and data"
for fs in ext4 xfs zfs noq; do
  mkdir -p "/mnt/it-$fs/vol"
  docker volume create --driver local --opt type=none --opt o=bind --opt device="/mnt/it-$fs/vol" \
    --label com.docker.compose.project=itstack "vol_$fs" >/dev/null
  docker run -d --name "w_$fs" --label com.docker.compose.project=itstack -v "vol_$fs":/data alpine sleep 3600 >/dev/null
  dd if=/dev/urandom of="/mnt/it-$fs/vol/before" bs=1M count=5 status=none
done
mkdir -p /mnt/it-ext4/conf /mnt/it-xfs/home/alice /mnt/it-xfs/home/bob /mnt/it-ext4/big /mnt/it-noq/dir /mnt/it-xfsnoq/dir
dd if=/dev/urandom of=/mnt/it-ext4/conf/f bs=1M count=2 status=none
dd if=/dev/urandom of=/mnt/it-xfs/home/alice/f bs=1M count=3 status=none
for i in $(seq 2000); do echo "$i" > "/mnt/it-ext4/big/f$i"; done

CONF="  - path: /mnt/it-ext4/conf
    labels: { stack: confstack, owner: tester }"
BIG="  - path: /mnt/it-ext4/big"
write_config() {
  cat > "$DATA/config.yml" <<EOF
disable: [ image ]                 # covered by run-images.sh
reconcile_interval: 2s
walk_files_per_second: ${RATE:-0}
directories:
  - path: /mnt/it-ext4
  - path: /mnt/it-xfs/home/*
    labels: { user: "\$1" }
  - path: /mnt/it-noq/dir
  - path: /mnt/it-xfsnoq/dir
$CONF
$BIG
EOF
}
write_config

step "start with exactly the documented capabilities"
start_da
wait_for 120 "walks finished" no_walks

step "sizes after the walk"
for fs in ext4 xfs zfs; do
  dd if=/dev/urandom of="/mnt/it-$fs/vol/after" bs=1M count=3 status=none
done
sync
for fs in ext4 xfs zfs; do
  wait_for 60 "vol_$fs >= 8 MB" bytes_ge "/mnt/it-$fs/vol" 8000000
done
wait_for 30 "conf >= 2 MB" bytes_ge /mnt/it-ext4/conf 2000000
metrics | grep -q 'hatchlens_directory_bytes{.*path="/mnt/it-xfs/home/alice".*user="alice"' || fail "glob label"
metrics | grep -q 'hatchlens_directory_bytes{path="/mnt/it-ext4/vol",source="volume",stack="itstack",volume="vol_ext4"}' || fail "volume labels"
metrics | grep -q 'hatchlens_directory_info{owner="w_ext4",path="/mnt/it-ext4/vol",project_id="[0-9]*",state="running"} 1' || fail "owner"
metrics | grep -q 'hatchlens_directory_info{owner="tester",path="/mnt/it-ext4/conf"' || fail "config owner"

step "admin action and overlap"
metrics | grep -q 'hatchlens_admin_action_needed{action="enable_project_quota_feature",fstype="ext4",mountpoint="/mnt/it-noq"} 1' || fail "admin action"
metrics | grep -q 'hatchlens_directory_unmeasurable{path="/mnt/it-noq/vol",reason="enable_project_quota_feature"} 1' || fail "unmeasurable"
metrics | grep -q 'hatchlens_admin_action_needed{action="mount_with_prjquota",fstype="xfs",mountpoint="/mnt/it-xfsnoq"} 1' || fail "xfs admin action"
metrics | grep -q 'hatchlens_directory_overlap{inner="/mnt/it-ext4/conf",path="/mnt/it-ext4"} 1' || fail "overlap"
LOG=$(docker logs hatchlens 2>&1)
grep -q 'tune2fs -O project,quota' <<<"$LOG" || fail "admin action log"
grep -q 'systemctl stop docker' <<<"$LOG" || fail "admin action log: stop Docker"
grep -q 'add prjquota to the options of /mnt/it-xfsnoq' <<<"$LOG" || fail "admin action log: prjquota"

step "walk boundaries"
OUTER=$(project_id /mnt/it-ext4)
VOL=$(project_id /mnt/it-ext4/vol)
CONF=$(project_id /mnt/it-ext4/conf)
[ -n "$OUTER" ] && [ -n "$VOL" ] && [ "$OUTER" != "$VOL" ] || fail "ids $OUTER $VOL"
[ "$(file_projid /mnt/it-ext4/vol/before)" = "$VOL" ] || fail "outer walk touched the volume"
[ "$(file_projid /mnt/it-ext4/conf/f)" = "$CONF" ] || fail "outer walk touched conf"

restart_da() {
  docker restart -t 10 hatchlens >/dev/null
  wait_for 60 "ready after restart" curl -sf "$URL/-/ready"
}

step "reset of an inner entry to the outer ID"
CONF=""
write_config
restart_da
reset_done() { [ "$(file_projid /mnt/it-ext4/conf/f)" = "$OUTER" ]; }
wait_for 60 "conf reset" reset_done
! metrics | grep -q 'path="/mnt/it-ext4/conf"' || fail "conf still reported"

step "reset interrupted by a restart"
BIG=""
RATE=100 write_config
restart_da
sleep 4
docker restart -t 10 hatchlens >/dev/null
grep -q '"pending_reset": true' "$DATA/state.json" || fail "pending reset not kept"
wait_for 60 "ready after restart" curl -sf "$URL/-/ready"
big_reset() { [ "$(file_projid /mnt/it-ext4/big/f2000)" = "$OUTER" ] && ! grep -q '/mnt/it-ext4/big' "$DATA/state.json"; }
wait_for 120 "big reset after restart" big_reset

step "lost state keeps IDs"
docker stop -t 10 hatchlens >/dev/null
docker rm hatchlens >/dev/null
rm "$DATA/state.json"
start_da
wait_for 120 "walks finished" no_walks
[ "$(project_id /mnt/it-ext4/vol)" = "$VOL" ] || fail "volume ID changed after lost state"

step "Docker volume deleted"
docker rm -f w_xfs >/dev/null
docker volume rm vol_xfs >/dev/null
gone() { ! metrics | grep -q 'path="/mnt/it-xfs/vol"' && [ "$(file_projid /mnt/it-xfs/vol/before)" = 0 ]; }
wait_for 60 "bind volume reset" gone

step "container CPU, memory, disk and network"
docker network create it_a >/dev/null
docker network create it_b >/dev/null
# Non-root workload: /proc/<pid>/net of a foreign user must be readable without CAP_SYS_PTRACE.
docker run -d --name it_load --user 1000 --network it_a --label com.docker.compose.project=loadstack \
  --cpus 1.5 --memory 256m alpine sh -c \
  'yes >/dev/null & dd if=/dev/zero of=/tmp/x bs=1M count=50 conv=fsync 2>/dev/null; sleep 3600' >/dev/null
docker network connect it_b it_load
docker run -d --name it_peer --network it_a alpine sleep 3600 >/dev/null
docker network connect it_b it_peer
peer_a=$(docker inspect -f '{{(index .NetworkSettings.Networks "it_a").IPAddress}}' it_peer)
peer_b=$(docker inspect -f '{{(index .NetworkSettings.Networks "it_b").IPAddress}}' it_peer)
docker exec it_load ping -q -c 20 -s 1000 "$peer_a" >/dev/null
docker exec it_load ping -q -c 3 "$peer_b" >/dev/null
L='container="it_load"'
cpu_ge() { local v; v=$(value container_cpu_usage_seconds_total "$L" 'stack="loadstack"'); [ -n "$v" ] && ge "$v" 1; }
wait_for 60 "CPU seconds" cpu_ge
eq() { awk -v a="$1" -v b="$2" 'BEGIN{exit !(a != "" && a+0 == b+0)}'; }
eq "$(value container_cpu_limit_cores "$L")" 1.5 || fail "CPU limit"
eq "$(value container_memory_limit_bytes "$L")" 268435456 || fail "memory limit"
ge "$(value container_memory_usage_bytes "$L")" 1 || fail "memory usage"
[ -n "$(value container_memory_working_set_bytes "$L")" ] || fail "working set"
[ -n "$(value container_memory_oom_kills_total "$L")" ] || fail "OOM kills"
[ -n "$(value container_cpu_throttled_seconds_total "$L")" ] || fail "throttled"
written() { local v; v=$(value container_disk_written_bytes_total "$L"); [ -n "$v" ] && ge "$v" 50000000; }
wait_for 30 "disk writes" written
tx_a=$(value container_network_transmit_bytes_total "$L" 'network="it_a"')
tx_b=$(value container_network_transmit_bytes_total "$L" 'network="it_b"')
[ -n "$tx_a" ] && [ -n "$tx_b" ] && ge "$tx_a" 20000 && ! ge "$tx_b" 20000 || fail "per-network split: a=$tx_a b=$tx_b"
metrics | grep -q '^hatchlens_container_network_receive_packets_total{.*interface="eth[01]".*network="it_b"' || fail "interface label"
docker rm -f it_load it_peer >/dev/null
docker network rm it_a it_b >/dev/null
gone_load() { ! metrics | grep -q "$L"; }
wait_for 30 "stopped container has no series" gone_load

step "endpoints"
curl -sf "$URL/-/healthy" >/dev/null || fail "healthy"
docker kill -s HUP hatchlens >/dev/null
sleep 1
curl -sf "$URL/-/healthy" >/dev/null || fail "SIGHUP must be ignored"

docker stop -t 10 hatchlens >/dev/null
chmod -R a+rX "$COVER"
step "passed"
