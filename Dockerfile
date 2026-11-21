# syntax=docker/dockerfile:1

# ---- Build stage -------------------------------------------------------------
# Runs on the build machine's own architecture ($BUILDPLATFORM) and
# cross-compiles for the target, which is much faster than emulating arm64.
FROM --platform=$BUILDPLATFORM golang:1.27.4 AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

# Download dependencies first: this layer is only rebuilt when go.mod or
# go.sum change, not on every code change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# CGO_ENABLED=0 produces a fully static binary that needs no libc,
# which is what makes the distroless "static" image possible.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/fritzbox_exporter .

# ---- Runtime stage -----------------------------------------------------------
# No shell, no package manager, only CA certificates, tzdata and a non-root user.
FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=build /out/fritzbox_exporter /fritzbox_exporter

# uid/gid 65532, predefined in the nonroot image.
USER nonroot:nonroot

EXPOSE 9133

ENTRYPOINT ["/fritzbox_exporter"]