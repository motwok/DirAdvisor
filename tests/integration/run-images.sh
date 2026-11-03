#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Integration test of the image checks. Everything runs in a docker:dind engine with
# its own registry, so the image store, the registry mirror and Swarm are under control.
# The directory part runs without Docker volumes. Usage: run-images.sh IMAGE COVERDIR STORE
#   COVERDIR: host directory or volume name for GOCOVERDIR
#   STORE:    classic or containerd
# No pipefail: grep -q closes the pipe early and the writer would fail with SIGPIPE.
set -eu

IMAGE=$1
COVER=$2
STORE=$3
HERE=$(cd "$(dirname "$0")" && pwd)
E=hl-engine
REG=localhost:5000
RL=localhost:5002
URL=http://127.0.0.1:18080

case $STORE in
  classic) snapshotter=false ;;
  containerd) snapshotter=true ;;
  *) echo "STORE must be classic or containerd"; exit 2 ;;
esac
case $COVER in /*) mkdir -p "$COVER" ;; esac

d() { docker exec -i "$E" docker "$@"; }
step() { echo "== $*"; }
fail() {
  echo "FAIL: $*"
  echo "--- metrics"; metrics | grep -v '^#' || true
  echo "--- state"; docker exec "$E" cat /data/images.json || true
  echo "--- log"; d logs hatchlens 2>&1 | tail -100 || true
  exit 1
}
cleanup() { docker rm -f -v "$E" >/dev/null 2>&1 || true; }
trap cleanup EXIT

metrics() { docker exec "$E" wget -qO- "$URL/metrics"; }
get() { docker exec "$E" wget -qO- "$URL$1" >/dev/null 2>&1; }
post() { docker exec "$E" wget -qO- --post-data= "$URL$1" >/dev/null 2>&1; }
# series NAME FILTER...: value of the first series of NAME containing every FILTER.
series() {
  local out
  out=$(metrics | grep "^hatchlens_$1[{ ]") || return 1
  shift
  for f in "$@"; do out=$(grep -F -- "$f" <<<"$out") || return 1; done
  head -1 <<<"$out" | awk '{print $NF}'
}
has() { series "$@" >/dev/null; }
# is VALUE NAME FILTER...
is() { local want=$1; shift; [ "$(series "$@" 2>/dev/null)" = "$want" ]; }
count() { metrics | grep "^hatchlens_$1{" | grep -cF -- "$2" || true; }
# tasks: container names of reported Swarm tasks of slot 1.
tasks() { metrics | grep '^hatchlens_container_image_info{' | grep -o 'container="st_svc\.1\.[^"]*"' | sort -u; }
wait_for() { # wait_for SECONDS DESCRIPTION COMMAND...
  local t=$1 what=$2; shift 2
  for _ in $(seq "$t"); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  fail "timeout: $what"
}
copy() { d buildx imagetools create -t "$2" "$1" >/dev/null; }
config() { docker exec -i "$E" sh -c 'cat > /data/config.yml'; }

step "engine with the $STORE image store"
cleanup
docker run -d --privileged --name "$E" -v "$COVER":/cover --entrypoint sh docker:29-dind -c \
  'mkdir -p /etc/docker /data && printf %s "$1" > /etc/docker/daemon.json && exec dockerd-entrypoint.sh dockerd' \
  sh "{\"registry-mirrors\":[\"http://$REG\"],\"features\":{\"containerd-snapshotter\":$snapshotter}}" >/dev/null
wait_for 60 "engine" d info
got=classic
d info -f '{{json .DriverStatus}}' | grep -q io.containerd.snapshotter && got=containerd
[ "$got" = "$STORE" ] || fail "image store is $got"
for i in registry:2 python:3.13-alpine; do
  docker image inspect "$i" >/dev/null 2>&1 || docker pull -q "$i" >/dev/null
done
docker save "$IMAGE" registry:2 python:3.13-alpine | d load -q >/dev/null

step "registries and containers"
d run -d --name registry -p 5000:5000 registry:2 >/dev/null
d run -d --name fake -p 5002:5000 python:3.13-alpine python -c "$(cat "$HERE/fake429.py")" >/dev/null
wait_for 30 "registry" docker exec "$E" wget -qO- "http://$REG/v2/"
# One copy per version from Docker Hub, the rest within the local registry.
copy busybox:1.36 "$REG/src:old"
copy busybox:1.37 "$REG/src:new"
copy "$REG/src:old" "$REG/app:1"
copy "$REG/src:old" "$REG/svc:1"
copy "$REG/src:old" "$REG/library/mirrortest:1"
d pull -q "$REG/app:1" >/dev/null
d run -d --name app "$REG/app:1" sleep 1d >/dev/null
old=$(d image inspect -f '{{index .RepoDigests 0}}' "$REG/app:1")
d run -d --name pinned "$old" sleep 1d >/dev/null
id=$(d image inspect -f '{{.Id}}' "$REG/app:1")
id=${id#sha256:}
d run -d --name localid "${id:0:12}" sleep 1d >/dev/null
printf 'FROM %s/app:1\nLABEL test=local\n' "$REG" | d build -q -t "$REG/local:1" - >/dev/null
d run -d --name local "$REG/local:1" sleep 1d >/dev/null
d run -d --name skip-1 "$REG/app:1" sleep 1d >/dev/null
d run -d --name optout --label de.mo2000.hatchlens.enable=false "$REG/app:1" sleep 1d >/dev/null
d pull -q mirrortest:1 >/dev/null
d run -d --name mirror mirrortest:1 sleep 1d >/dev/null
d tag "$REG/app:1" "$RL/x:1"
d tag "$REG/app:1" "$RL/y:1"
d run -d --name rl-x "$RL/x:1" sleep 1d >/dev/null
d run -d --name rl-y "$RL/y:1" sleep 1d >/dev/null

step "start"
config <<EOF
disable: [ directory, volume ]
reconcile_interval: 5s
rootfs: /
exclude:
  containers: ["skip-*", "hatchlens"]
  images: ["docker.io/library/registry:*", "python:*"]
images:
  check_interval: 30s
EOF
d run -d --name hatchlens -e GOCOVERDIR=/cover -v /cover:/cover -e HATCHLENS_CONFIG_FILE=/data/config.yml \
  -v /var/run/docker.sock:/var/run/docker.sock:ro -v /data:/data \
  -p 18080:8080 "$IMAGE" >/dev/null
wait_for 60 "ready" get /-/ready

step "1 multi-arch image"
wait_for 60 "app checked" is 1 container_image_info 'container="app"' 'mode="checked"'
is 0 container_image_update_available 'container="app"' || fail "app update_available"
is 0 container_image_update_pulled 'container="app"' || fail "app pulled"
! has container_image_update_available_since_timestamp_seconds 'container="app"' || fail "app since"

step "5 locally built image"
wait_for 60 "local error" is 1 container_image_info 'container="local"' 'mode="error"'
has image_check_error "reference=\"$REG/local:1\"" 'reason="not_found"' ||
  has image_check_error "reference=\"$REG/local:1\"" 'reason="denied"' || fail "local reason"

step "6 pinned and local"
is 1 container_image_info 'container="pinned"' 'mode="pinned"' || fail "pinned"
is 1 container_image_info 'container="localid"' 'mode="local"' || fail "localid"
! has container_image_info 'container="localid"' 'reference=' || fail "localid reference"
! has container_image_update_pulled 'container="localid"' || fail "localid pulled"

step "7 excludes"
for c in skip-1 optout registry fake hatchlens; do
  [ "$(count container_image_info "container=\"$c\"")" = 0 ] || fail "$c reported"
done

step "8 registry mirror"
wait_for 60 "mirror checked" is 1 container_image_info 'container="mirror"' 'mode="checked"'
is 0 container_image_update_available 'container="mirror"' || fail "mirror digest"

step "10 rate limit"
wait_for 60 "rate limited" has image_check_error 'reason="rate_limited"'
sleep 15
[ "$(count image_check_error "$RL/")" = 1 ] || fail "second check despite registry backoff"
[ "$(count container_image_info "$RL/")" = 2 ] || fail "rl containers"
is 1 container_image_info "$RL/" 'mode="pending"' || fail "rl pending"
docker exec "$E" cat /data/images.json | grep -q "\"$RL\"" || fail "registry backoff not in state"

step "2 newer image"
copy "$REG/src:new" "$REG/app:1"
wait_for 90 "app update" is 1 container_image_update_available 'container="app"'
has container_image_update_available_since_timestamp_seconds 'container="app"' || fail "app since"
is 0 container_image_update_pulled 'container="app"' || fail "app pulled early"

step "3 pulled"
d pull -q "$REG/app:1" >/dev/null
wait_for 30 "app pulled" is 1 container_image_update_pulled 'container="app"'

step "4 recreated"
d rm -f app >/dev/null
d run -d --name app "$REG/app:1" sleep 1d >/dev/null
wait_for 30 "app current" is 0 container_image_update_available 'container="app"'
! has container_image_update_available_since_timestamp_seconds 'container="app"' || fail "app since kept"
is 0 container_image_update_pulled 'container="app"' || fail "app pulled kept"

step "9 swarm"
d swarm init >/dev/null
printf 'services:\n  svc:\n    image: %s/svc:1\n    command: sleep 1d\n' "$REG" | d stack deploy -q -c - st >/dev/null
d service inspect -f '{{.Spec.TaskTemplate.ContainerSpec.Image}}' st_svc | grep -q "^$REG/svc:1@sha256:" ||
  fail "service not deployed with a digest"
wait_for 90 "task checked" is 1 container_image_info 'container="st_svc.1.' 'mode="checked"'
is 0 container_image_update_available 'container="st_svc.1.' "image=\"$REG/svc:1\"" 'stack="st"' ||
  fail "task update_available"
! has container_image_update_pulled 'container="st_svc.1.' || fail "task pulled"
copy "$REG/src:new" "$REG/svc:1"
wait_for 90 "task update" is 1 container_image_update_available 'container="st_svc.1.'
since=$(series container_image_update_available_since_timestamp_seconds 'container="st_svc.1.')
task=$(tasks)
d service update -q --detach=false --force st_svc >/dev/null
[ "$(d ps -aq --filter name=st_svc.1. | wc -l)" = 2 ] || fail "no task history"
newtask() { [ "$(tasks | wc -l)" = 1 ] && [ "$(tasks)" != "$task" ]; }
wait_for 30 "new task reported alone" newtask
is 1 container_image_update_available 'container="st_svc.1.' || fail "new task update_available"
is "$since" container_image_update_available_since_timestamp_seconds 'container="st_svc.1.' ||
  fail "since changed"

step "11 restart"
ts=$(series image_check_timestamp_seconds "reference=\"$REG/app:1\"")
newcheck() { ! is "$ts" image_check_timestamp_seconds "reference=\"$REG/app:1\""; }
wait_for 60 "next app check" newcheck
ts=$(series image_check_timestamp_seconds "reference=\"$REG/app:1\"")
d restart -t 10 hatchlens >/dev/null
wait_for 30 "ready after restart" get /-/ready
is 1 container_image_info 'container="app"' 'mode="checked"' || fail "app digest lost"
is "$ts" image_check_timestamp_seconds "reference=\"$REG/app:1\"" || fail "app checked at start"
is "$since" container_image_update_available_since_timestamp_seconds 'container="st_svc.1.' ||
  fail "since lost"

step "9 swarm update"
d service update -q --detach=false --image "$REG/svc:1" st_svc >/dev/null
current() { [ "$(tasks | wc -l)" = 1 ] && is 0 container_image_update_available 'container="st_svc.1.'; }
wait_for 30 "task current" current
! has container_image_update_available_since_timestamp_seconds 'container="st_svc.1.' || fail "task since kept"

step "12 endpoints"
get /-/healthy || fail "healthy"
! post /-/reload || fail "reload endpoint"
is 0 reconcile_errors_total || fail "reconcile errors"
has build_info || fail "build_info"
d kill -s HUP hatchlens >/dev/null
sleep 2
get /-/ready || fail "SIGHUP must be ignored"

d stop -t 10 hatchlens >/dev/null
docker exec "$E" chmod -R a+rwX /cover
step "passed"
