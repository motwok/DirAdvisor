# Building and testing

## Layout

| Path | Content |
|---|---|
| `cmd/diradvisor` | `main` |
| `internal/app` | flags, HTTP endpoints, signals, main loop |
| `internal/core` | discovery, ownership, labels, project IDs, walks, metrics |
| `internal/config` | config file, environment variables, globs |
| `internal/docker` | read-only Docker API client |
| `internal/host` | Linux: mounts, `quotactl_fd`, project ID ioctls, walks, ZFS |
| `internal/state` | `state.json` |
| `internal/metrics` | Prometheus text format |
| `tests/integration` | integration test |

Unit tests sit next to the code (`*_test.go`). The only dependency is `go.yaml.in/yaml/v3`.

## Build

Needs the Go version in `go.mod`.

```bash
go build ./cmd/diradvisor
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./cmd/diradvisor
docker build -t diradvisor:dev .
```

The image is `FROM scratch` with one static binary. `--build-arg VERSION=... REVISION=... BUILD_TIME=...`
set what `diradvisor_build_info` reports.

## Unit tests

```bash
gofmt -l .
go vet ./...
go test -race ./...
```

The Linux system calls are not unit tested; on other systems `internal/host` has a stub. They are
covered by the integration test.

## Integration test

`tests/integration/run.sh IMAGE COVERDIR` creates loop devices with ext4 (project quota), XFS
(`prjquota`), a ZFS pool and ext4 without quota, Docker volumes and containers on them, and runs the
image with exactly the documented capabilities. It needs root, Docker, `xfsprogs`, `zfsutils-linux` and
`e2fsprogs`, and it changes the machine (mounts, a pool, containers), so run it only in CI or a
throw-away VM:

```bash
docker build --build-arg COVER=1 -t diradvisor:test .
sudo tests/integration/run.sh diradvisor:test "$PWD/cover-integration"
```

## Coverage

CI merges the unit and the integration coverage:

```bash
go test -covermode=atomic -coverpkg=./... -cover ./... -args -test.gocoverdir="$PWD/cover-unit"
go tool covdata percent -i=cover-unit,cover-integration
go tool covdata textfmt -i=cover-unit,cover-integration -o cover.txt
go tool cover -html=cover.txt -o coverage.html
```

The image built with `COVER=1` writes its counters to `GOCOVERDIR` on exit.

## CI

`.github/workflows/build.yml` runs unit tests, the integration test on `amd64` and `arm64` runners and
the coverage report on every PR; on `master` it also releases. See [RELEASING.md](../RELEASING.md).
