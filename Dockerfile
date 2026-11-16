# Build the single binary, then ship it on a distroless base: the service needs
# no shell and no files besides its own, and the healthcheck is the binary.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/talos-discovery . && \
    mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
ARG BUILD_DATE
ARG GIT_COMMIT
LABEL org.opencontainers.image.title="Talos Discovery" \
      org.opencontainers.image.description="Self-hosted discovery service for Talos Linux clusters" \
      org.opencontainers.image.source="https://github.com/masudur-rahman/talos-discovery" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.created="$BUILD_DATE" \
      org.opencontainers.image.revision="$GIT_COMMIT"

COPY --from=build /out/talos-discovery /app/talos-discovery
# Distroless has no shell to chown with at run time, and a fresh Docker volume
# copies the ownership of the directory it is mounted over. So the directory
# ships owned by nonroot, and the snapshot can be written there.
COPY --from=build --chown=65532:65532 /out/data /app/data

# The config file is read from .configs/.talos-discovery.yaml relative to the
# working directory, so mounting a ConfigMap at /app/.configs is all it takes.
# With no file mounted it runs plaintext on :3000 from defaults and env alone.
# Snapshots are on by default and go to /app/data/state.binpb; mount a volume
# there or they are lost with the container.
WORKDIR /app
EXPOSE 3000 3001 2122
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/app/talos-discovery", "--healthcheck"]
ENTRYPOINT ["/app/talos-discovery"]
