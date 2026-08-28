FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.7-alpine3.24 AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . ./

ARG TARGETARCH
ARG TARGETVARIANT
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH:-$(go env GOARCH)}" GOARM="${TARGETVARIANT#v}" \
      go build -tags=netgo -ldflags="-w -s" -trimpath -o /sleep ./cmd/ic-sleep

FROM docker.io/library/busybox:glibc
COPY --from=build /sleep /sleep
ENTRYPOINT ["/sleep"]
