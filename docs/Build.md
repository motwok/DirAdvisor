# Building and testing

## Layout

| Path | Content |
|---|---|
| `cmd/hatchlens` | `main` |
| `internal/app` | flags, HTTP endpoints, signals, main loops |
| `internal/core` | directories: discovery, ownership, labels, project IDs, walks, metrics |
| `internal/images` | image checks: reconcile, registry checks, comparison, metrics |
| `internal/images/state` | `images.json` |
| `internal/usage` | per-container CPU, memory, disk and network: cgroups and `/proc` (Linux), stats API (Windows) |
| `internal/config` | config source, environment variables, globs |
| `internal/docker` | read-only Docker API client (Unix socket, Windows named pipe) |
| `internal/host` | Linux: mounts, `quotactl_fd`, project ID ioctls, walks, ZFS |
| `internal/ref` | image reference parsing and normalization |
| `internal/state` | `state.json` |
| `internal/metrics` | Prometheus text format |
| `tests/integration` | integration tests |

Unit tests sit next to the code (`*_test.go`). The only dependency is `go.yaml.in/yaml/v3`.

## Build

Needs the Go version in `go.mod`.

```bash
go build ./cmd/hatchlens
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./cmd/hatchlens
docker build -t hatchlens:dev .
```

The Linux image is `FROM scratch` with one static binary. `--build-arg VERSION=... REVISION=...
BUILD_TIME=...` set what `hatchlens_build_info` reports.

The Windows image copies a binary built before onto nanoserver (no `RUN`), so it builds on Windows and
with buildx on Linux alike:

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o dist/hatchlens.exe ./cmd/hatchlens
docker build -f Dockerfile.windows --build-arg OSVERSION=ltsc2022 -t hatchlens:dev .
```

The HatchLens Host service (`cmd/hatchlens-host`, package `internal/winhost`) and its MSI, built on
Windows with WiX 5:

```powershell
$env:CGO_ENABLED = '0'
go build -trimpath -ldflags "-X github.com/motwok/HatchLens/internal/winhost.Version=0.0.1" -o dist/hatchlens-host.exe ./cmd/hatchlens-host
dotnet tool install --global wix --version 5.0.2
wix build packaging/windows/hatchlens-host.wxs -arch x64 -d Version=0.0.1 -d Exe=dist/hatchlens-host.exe -o dist/hatchlens-host.msi
```

HatchLens itself as a Windows service, from the same `hatchlens.exe` as in the image:

```powershell
go build -trimpath -o dist/hatchlens.exe ./cmd/hatchlens
wix build packaging/windows/hatchlens.wxs -arch x64 -d Version=0.0.1 -d Exe=dist/hatchlens.exe -o dist/hatchlens.msi
```

Started by the service control manager it runs as the service `HatchLens` with the host part in the
same process; started any other way (also in the container) it behaves as before.

`hatchlens-host version` prints the version; without arguments it serves in the foreground until
Ctrl+C (as administrator), under the service control manager as the service.

## Unit tests

```bash
gofmt -l .
go vet ./...
go test -race ./...
```

CI runs them on Linux and on Windows (without `-race`), so the Windows code is covered too. The Linux
system calls are not unit tested; on other systems `internal/host` has a stub. They are covered by the
integration test.

## Integration tests

`tests/integration/run.sh IMAGE COVERDIR` (directories) creates loop devices with ext4 (project quota),
XFS (`prjquota`), a ZFS pool and ext4 without quota, Docker volumes and containers on them, and runs the
image with exactly the documented capabilities. It needs root, Docker, `xfsprogs`, `zfsutils-linux` and
`e2fsprogs`, and it changes the machine (mounts, a pool, containers), so run it only in CI or a
throw-away VM:

```bash
docker build --build-arg COVER=1 -t hatchlens:test .
sudo tests/integration/run.sh hatchlens:test "$PWD/cover-integration"
```

`tests/integration/run-images.sh IMAGE COVERDIR STORE` (image checks) starts a `docker:dind` engine with
the `classic` or the `containerd` image store and runs everything inside it: a local `registry:2`, a
fake registry answering 429, test images copied from Docker Hub's `busybox`, a Swarm stack, and the
image under test. It checks the comparison end to end (newer image pushed, pulled, container recreated),
pinned and local containers, excludes, a registry mirror, Swarm task updates, the rate limit backoff, a
restart with the state and the endpoints. It needs Docker with `--privileged` and access to Docker Hub:

```bash
tests/integration/run-images.sh hatchlens:test "$PWD/cover-images" classic
tests/integration/run-images.sh hatchlens:test "$PWD/cover-images" containerd
```

`tests/integration/run-windows.ps1 IMAGE COVERDIR LTSC MSI HOSTEXE` runs the Windows image on a Windows
Server host: first without the host service (image checks, container usage, admin action
`install_host_service`), then with the MSI installed (volume and directory sizes and their growth,
traffic per Docker network on two `nat` networks), uninstalls the MSI, then installs HatchLens itself
as a service from its MSI and checks the same against port 8080 of the host. The service's unit tests open
the volume and read its USN journal; they need administrator rights, which the CI runners have.

## Coverage

CI merges the unit and the integration coverage of Linux and Windows:

```bash
go test -covermode=atomic -coverpkg=./... -cover ./... -args -test.gocoverdir="$PWD/cover-unit"
go tool covdata percent -i=cover-unit,cover-integration
go tool covdata textfmt -i=cover-unit,cover-integration -o cover.txt
go tool cover -html=cover.txt -o coverage.html
```

The image built with `COVER=1` (Linux) or from a binary built with `go build -cover` (Windows) writes
its counters to `GOCOVERDIR` on exit. Below 99% the `Coverage` check fails.

## CI

`.github/workflows/build.yml` runs on every PR: unit tests on Linux and Windows, the directory
integration test on `amd64` and `arm64` runners, the image integration test on both runners with both
image stores, the Windows integration test on `windows-2022` and `windows-2025`, and the coverage
report; on `master` it also releases. See [RELEASING.md](../RELEASING.md).
