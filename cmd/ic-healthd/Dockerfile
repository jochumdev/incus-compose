ARG VERSION

FROM docker.io/library/golang:1.26.5-alpine3.24 AS build
ARG VERSION
WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -tags=netgo -ldflags="-w -s -X github.com/lxc/incus-compose/cmd/ic-healthd/version.Version=${VERSION}" -trimpath -o /ic-healthd ./cmd/ic-healthd

FROM busybox:glibc
COPY --from=build /ic-healthd /usr/local/bin/ic-healthd
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/ic-healthd", "run"]
