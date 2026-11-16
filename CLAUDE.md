# Project: talos-discovery

## Overview
Self-hosted discovery service for Talos Linux clusters. Implements the `sidero.discovery.server.Cluster` gRPC API (Hello, AffiliateUpdate, AffiliateDelete, List, Watch) so Talos nodes can use it in place of discovery.talos.dev. Clean-room implementation: protocol from siderolabs/discovery-api (MPL-2.0); no code from siderolabs/discovery-service (BSL 1.1).

## Stack
Go 1.26 | grpc-go | discovery-api generated pb | log/slog | Prometheus client + go-grpc-middleware prometheus provider | x/time/rate | yaml.v3 | distroless image

## Architecture
```
main.go      flags, wiring, signals, graceful shutdown, snapshot loop, --decode-snapshot
configs/     defaults -> .configs/.talos-discovery.yaml -> TALOS_DISCOVERY_* env; Validate
models/      protocol limits and sentinel errors
state/       in-memory store: clusters, affiliates, endpoints (TTL each), watch subscriptions, GC, protobuf snapshot (protowire)
mux/         one listener -> gRPC + HTTP/1.1 (TLS by ALPN, plaintext by h2 preface), connection metrics
server/      gRPC service, validation, peer IP resolution, interceptors, per-IP limiter, metrics, TLS reloader
web/         landing, /inspect, /stats, /healthz; metrics and pprof handlers
e2e/         real siderolabs/discovery-client against the real server stack
deploy/      k8s (StatefulSet, Gateway/GRPCRoute, ingress-nginx), nginx conf, compose, Talos patch
```

## Commands
```bash
make run        # :3000 gRPC+site, :3001 landing, :2122 metrics
make test       # go test -race ./... (includes e2e)
make check      # vet + lint + test
make fmt        # goimports-reviser, std/project/company/general
make docker-build
make deploy KUBE_CONTEXT=<ctx>
```

## Conventions
- Import order: std -> project -> company -> general (blank line between groups), via `make fmt`
- Stdlib `testing` only; no testify
- Comments explain why, not what
- State methods take no `now`; tests inject time with `state.WithClock`
- Errors: wrap `models.ErrInvalid` for InvalidArgument; `toStatus` maps sentinels to gRPC codes
- Metric names/labels follow upstream discovery-service exactly (`discovery_*`, `grpc_server_*`); `server.TestMetricNames` pins them for dashboards

## Learned
- [keepalive-policy]: Talos's client pings every max(10s, TTL/10). gRPC's default server enforcement (5 min) answers that with GOAWAY "too_many_pings". `server.NewGRPCServer` sets `MinTime: 5s, PermitWithoutStream: true`; never remove it.
- [endpoint-size]: endpoints are AES-ECB encrypted client-side (deterministic, so the server can dedupe) and padded to the block size: 16 bytes for IPv4, 32 for IPv6. That is why the per-endpoint limit is 32 bytes. Affiliate data is AES-GCM and not deduplicated.
- [hello-client-ip]: `Hello.client_ip` becomes the node's KubeSpan public IP. Behind a proxy it must come from X-Real-IP, trusted only via config; LoadBalancer Services in front need `externalTrafficPolicy: Local`.
- [talos-insecure]: Talos selects plaintext gRPC when the endpoint scheme is `http://` (`clustertypes.NormalizeEndpoint`), so plaintext mode needs no client-side flag.
- [watch-idle]: Watch streams are silent between changes (an unchanged TTL refresh notifies nobody). Proxies need ~1h read/send timeouts or they cut every stream at 60s.
- [gc-race]: GC marks a cluster `removed` under its lock before dropping it from the map; `lockCluster` retries on a removed cluster so a concurrent update cannot land in an unreachable cluster.
- [snapshot-compat]: snapshots use upstream's `sidero.discovery.storage` field numbers, hand-encoded with protowire (no protoc). Both `ttl` (remaining, preferred on read) and `expiration` (absolute, for old readers) are written. `state/snapshot_wire_test.go` checks the bytes against a runtime-built descriptor.
- [alpn-routing]: on TLS the mux gives h2 only to clients that do NOT offer http/1.1; browsers offer both and would otherwise land on the gRPC server and get a 415.
- [flatcar-docker]: Flatcar's docker is socket-activated; restart policies do nothing after reboot until `systemctl enable --now docker.service`.
- [distroless-volume]: the image ships `/app/data` owned by 65532, because a fresh Docker volume copies the mount point's ownership and distroless has no shell to chown with.
