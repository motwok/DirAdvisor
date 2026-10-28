# SPDX-License-Identifier: AGPL-3.0-or-later
FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS TARGETARCH
ARG VERSION=dev REVISION=unknown BUILD_TIME=0 COVER=
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      ${COVER:+-cover -covermode=atomic -coverpkg=./...} \
      -ldflags "-s -w \
        -X github.com/motwok/DirAdvisor/internal/app.Version=$VERSION \
        -X github.com/motwok/DirAdvisor/internal/app.Revision=$REVISION \
        -X github.com/motwok/DirAdvisor/internal/app.BuildTime=$BUILD_TIME" \
      -o /out/diradvisor ./cmd/diradvisor

FROM scratch
COPY --from=build /out/diradvisor /diradvisor
LABEL org.opencontainers.image.source="https://github.com/motwok/DirAdvisor" \
      org.opencontainers.image.description="Per-directory disk usage for Prometheus, without walking the file tree." \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"
EXPOSE 8080
ENTRYPOINT ["/diradvisor"]
CMD ["--config.file=/data/config.yml", "--storage.path=/data", "--path.rootfs=/host"]
