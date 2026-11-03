# SPDX-License-Identifier: AGPL-3.0-or-later
# Root file system of the temporary WSL distribution of hatchlens-host
# enable-quota. Build: docker buildx build -o type=tar,dest=hatchlens-wsl.tar packaging/wsl
FROM alpine:3.22@sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f
RUN apk add --no-cache e2fsprogs e2fsprogs-extra
