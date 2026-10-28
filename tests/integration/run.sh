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
  echo "--- log"; docker logs diradvisor 2>&1 | tail -80 || true
  exit 1
}
metrics() { curl -sf "$URL/metrics"; }
# value METRIC LABELMATCH...: first sample value whose line contains all matches.
value() {
  local name=$1; shift
  metrics | grep "^diradvisor_${name}[{ ]" | {
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
no_walks() { ! metrics | grep -q '^diradvisor_directory_assigning{.*} 1$'; }
project_id() { metrics | grep "^diradvisor_directory_info{.*path=\"$1\"" | sed 's/.*project_id="\([0-9]*\)".*/\1/'; }
file_projid() {
  case "$1" in
    /mnt/it-xfs/*) xfs_io -r -c lsproj "$1" | sed 's/.*= *//' ;;
    *) lsattr -p "$1" | awk '{print $1}' ;;
  esac
}

start_da() {
  docker run -d --name diradvisor \
    --cap-add SYS_ADMIN --cap-add DAC_READ_SEARCH --device /dev/zfs \
    -e GOCOVERDIR=/cover -v "$COVER":/cover \
    -v /var/run/docker.sock:/var/run/docker.sock:ro -v /:/host:rslave -v "$DATA":/data \
    -p "$PORT":8080 "$IMAGE" "$@" >/dev/null
  wait_for 60 "ready" curl -sf "$URL/-/ready"
}

step "filesystems"
loop() { truncate -s "$2" "$WORK/$1.img"; losetup -f --show "$WORK/$1.img"; }
EXT=$(loop ext4 400M); mkfs.ext4 -q -O project,quota "$EXT"; mkdir -p /mnt/it-ext4; mount "$EXT" /mnt/it-ext4
XFS=$(loop xfs 512M); mkfs.xfs -q "$XFS"; mkdir -p /mnt/it-xfs; mount -o prjquota "$XFS" /mnt/it-xfs
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
mkdir -p /mnt/it-ext4/conf /mnt/it-xfs/home/alice /mnt/it-xfs/home/bob /mnt/it-ext4/big /mnt/it-noq/dir
dd if=/dev/urandom of=/mnt/it-ext4/conf/f bs=1M count=2 status=none
dd if=/dev/urandom of=/mnt/it-xfs/home/alice/f bs=1M count=3 status=none
for i in $(seq 2000); do echo "$i" > "/mnt/it-ext4/big/f$i"; done

CONF="  - path: /mnt/it-ext4/conf
    labels: { stack: confstack, owner: tester }"
BIG="  - path: /mnt/it-ext4/big"
write_config() {
  cat > "$DATA/config.yml" <<EOF
global:
  reconcile_interval: 2s
  walk_files_per_second: ${RATE:-0}
directories:
  - path: /mnt/it-ext4
  - path: /mnt/it-xfs/home/*
    labels: { user: "\$1" }
  - path: /mnt/it-noq/dir
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
metrics | grep -q 'diradvisor_directory_bytes{.*path="/mnt/it-xfs/home/alice".*user="alice"' || fail "glob label"
metrics | grep -q 'diradvisor_directory_bytes{path="/mnt/it-ext4/vol",source="volume",stack="itstack",volume="vol_ext4"}' || fail "volume labels"
metrics | grep -q 'diradvisor_directory_info{owner="w_ext4",path="/mnt/it-ext4/vol",project_id="[0-9]*",state="running"} 1' || fail "owner"
metrics | grep -q 'diradvisor_directory_info{owner="tester",path="/mnt/it-ext4/conf"' || fail "config owner"

step "admin action and overlap"
metrics | grep -q 'diradvisor_admin_action_needed{action="enable_project_quota_feature",fstype="ext4",mountpoint="/mnt/it-noq"} 1' || fail "admin action"
metrics | grep -q 'diradvisor_directory_unmeasurable{path="/mnt/it-noq/vol",reason="enable_project_quota_feature"} 1' || fail "unmeasurable"
metrics | grep -q 'diradvisor_directory_overlap{inner="/mnt/it-ext4/conf",path="/mnt/it-ext4"} 1' || fail "overlap"
docker logs diradvisor 2>&1 | grep -q 'tune2fs -O project,quota' || fail "admin action log"

step "walk boundaries"
OUTER=$(project_id /mnt/it-ext4)
VOL=$(project_id /mnt/it-ext4/vol)
CONF=$(project_id /mnt/it-ext4/conf)
[ -n "$OUTER" ] && [ -n "$VOL" ] && [ "$OUTER" != "$VOL" ] || fail "ids $OUTER $VOL"
[ "$(file_projid /mnt/it-ext4/vol/before)" = "$VOL" ] || fail "outer walk touched the volume"
[ "$(file_projid /mnt/it-ext4/conf/f)" = "$CONF" ] || fail "outer walk touched conf"

step "reset of an inner entry to the outer ID"
CONF=""
write_config
reset_done() { [ "$(file_projid /mnt/it-ext4/conf/f)" = "$OUTER" ]; }
wait_for 60 "conf reset" reset_done
! metrics | grep -q 'path="/mnt/it-ext4/conf"' || fail "conf still reported"

step "reset interrupted by a restart"
BIG=""
RATE=100 write_config
sleep 4
docker restart -t 10 diradvisor >/dev/null
grep -q '"pending_reset": true' "$DATA/state.json" || fail "pending reset not kept"
wait_for 60 "ready after restart" curl -sf "$URL/-/ready"
big_reset() { [ "$(file_projid /mnt/it-ext4/big/f2000)" = "$OUTER" ] && ! grep -q '/mnt/it-ext4/big' "$DATA/state.json"; }
wait_for 120 "big reset after restart" big_reset

step "lost state keeps IDs"
docker stop -t 10 diradvisor >/dev/null
docker rm diradvisor >/dev/null
rm "$DATA/state.json"
start_da
wait_for 120 "walks finished" no_walks
[ "$(project_id /mnt/it-ext4/vol)" = "$VOL" ] || fail "volume ID changed after lost state"

step "Docker volume deleted"
docker rm -f w_xfs >/dev/null
docker volume rm vol_xfs >/dev/null
gone() { ! metrics | grep -q 'path="/mnt/it-xfs/vol"' && [ "$(file_projid /mnt/it-xfs/vol/before)" = 0 ]; }
wait_for 60 "bind volume reset" gone

step "endpoints"
curl -sf "$URL/-/healthy" >/dev/null || fail "healthy"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$URL/-/reload")" = 200 ] || fail "reload"
[ "$(value config_last_reload_successful)" = 1 ] || fail "reload metric"

docker stop -t 10 diradvisor >/dev/null
chmod -R a+rX "$COVER"
step "passed"
