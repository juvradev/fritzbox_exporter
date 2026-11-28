# syntax=docker/dockerfile:1

# ---- Build stage -------------------------------------------------------------
# Runs on the build machine's own architecture ($BUILDPLATFORM) and
# cross-compiles for the target, which is much faster than emulating arm64.
#
# Base images are pinned by digest: a tag can be moved to a different image,
# a digest cannot. Dependabot updates tag and digest together.
FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build

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
FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5

COPY --from=build /out/fritzbox_exporter /fritzbox_exporter

# uid/gid 65532, predefined in the nonroot image.
USER nonroot:nonroot

EXPOSE 9133

ENTRYPOINT ["/fritzbox_exporter"]