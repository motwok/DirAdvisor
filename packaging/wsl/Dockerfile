# SPDX-License-Identifier: AGPL-3.0-or-later
# Root file system of the temporary WSL distribution of hatchlens-host
# enable-quota. Build: docker buildx build -o type=tar,dest=hatchlens-wsl.tar packaging/wsl
FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
RUN apk add --no-cache e2fsprogs e2fsprogs-extra
