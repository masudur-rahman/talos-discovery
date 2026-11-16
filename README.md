# Talos Discovery

A self-hosted discovery service for [Talos Linux](https://www.talos.dev) clusters. It speaks the same `Cluster` gRPC API as `discovery.talos.dev`, so a cluster is pointed at it with one machine-config change.

Nodes use the discovery service to find each other and, with KubeSpan, to learn their own public IP. Everything a node sends is encrypted with the cluster's own key before it leaves the node: the server stores and relays ciphertext it cannot read.

This is an independent implementation written against the protocol in [siderolabs/discovery-api](https://github.com/siderolabs/discovery-api) (MPL-2.0). It contains no code from siderolabs/discovery-service, which is under the Business Source License. It matches that service's API, limits, ports, snapshot format and metric names; the differences are listed under [Compared with upstream](#compared-with-upstream).

## How nodes use it

Talos's `machined` embeds [siderolabs/discovery-client](https://github.com/siderolabs/discovery-client) and talks gRPC straight to the endpoint in its machine config. There is no REST layer. Each node:

1. calls `Hello` on every (re)connect, getting back its public IP and any redirect;
2. calls `AffiliateUpdate` with its encrypted data and the endpoints it sees, then again on every change and every TTL/2;
3. holds one `Watch` stream open to receive every change in its cluster;
4. calls `AffiliateDelete` when it leaves cleanly.

In this code `pb.RegisterClusterServer` (in `server/grpc.go`) hands those methods to grpc-go, which dispatches by name; that is why nothing in the repository calls them except the tests.

## Ports

| Port | Serves | Default |
|---|---|---|
| `server.addr` | gRPC for nodes, **and** `/`, `/inspect`, `/stats`, `/healthz` over HTTP/1.1 on the same port | `:3000` |
| `server.landingAddr` | the same pages on a port of their own | `:3001` |
| `server.metricsAddr` | Prometheus `/metrics` | `:2122` |
| `server.debugAddr` | pprof and expvar | off |

One port serves both protocols. With TLS, a client that offers `http/1.1` in ALPN (browsers, curl) gets HTTP and one that offers only `h2` (grpc-go, so Talos) gets gRPC. In plaintext, a connection that opens with the HTTP/2 preface is gRPC and anything else is HTTP.

## Running it

| Mode | Server config | Talos endpoint |
|---|---|---|
| Plaintext, direct | defaults | `http://host:3000/` |
| TLS, direct | `tls.certFile` + `tls.keyFile` | `https://host:3000/` |
| Behind nginx / a gateway | defaults + `proxy.trustXRealIP: true` | `https://discovery.example.com/` |

```bash
make run                      # plaintext on :3000, snapshots in ./data
docker compose -f deploy/compose/docker-compose.yml up -d    # see deploy/compose
docker compose -f deploy/compose/docker-compose.nginx.yml up -d  # behind nginx on 443
make deploy KUBE_CONTEXT=...  # Kubernetes; see deploy/k8s
```

Then point the cluster at it with [`deploy/talos/discovery-patch.yaml`](deploy/talos/discovery-patch.yaml):

```bash
talosctl patch mc --nodes <every node> --patch @deploy/talos/discovery-patch.yaml
talosctl get affiliates --nodes <a node>   # every other node should be listed
```

### On a Flatcar VM

`deploy/compose/docker-compose.yml` runs as is. Three things specific to Flatcar:

- Docker is socket-activated, so containers with `restart: unless-stopped` do not come back after a reboot until something starts the daemon. Run `sudo systemctl enable --now docker.service` once.
- The snapshot lives in the `discovery-data` named volume under `/var/lib/docker`, which is on Flatcar's persistent `/var`. It survives reboots and OS updates.
- Flatcar has no host firewall by default. Port 2122 (metrics) is bound to `127.0.0.1` in the compose file; set `METRICS_BIND` to open it to your Prometheus host only.

### Behind a proxy

Three things have to be right, or nodes connect but KubeSpan quietly breaks:

1. **gRPC to the backend.** nginx `grpc_pass grpc://...`, ingress-nginx `backend-protocol: GRPC`, a Gateway API `GRPCRoute`. The backend is plaintext HTTP/2. Route only the gRPC paths (`/sidero.discovery.server.Cluster/`, `/grpc.health.v1.Health/`) that way and everything else as plain HTTP, so the landing page and `/inspect` work through the proxy too; the configs in `deploy/` do this.
2. **The client's address.** `Hello` returns the address the server sees, and the node takes it as its public IP. The proxy must send it as `X-Real-IP` (or `X-Forwarded-For`, first entry), and the server must be told to trust it. A Kubernetes LoadBalancer in front of the proxy needs `externalTrafficPolicy: Local`. The per-IP rate limit uses the same address, so without it every node shares the proxy's one bucket.
3. **Long timeouts.** A `Watch` stream stays open as long as the node is up and is silent between changes. A 60-second proxy read timeout resets every node once a minute; set it to an hour.

Only trust the proxy header when the server is unreachable except through the proxy. Otherwise a client can claim any address.

## Configuration

`.configs/.talos-discovery.yaml` (another path with `--config` or `TALOS_DISCOVERY_CONFIG`). Every key is optional, and every key has a `TALOS_DISCOVERY_*` environment variable that overrides it. [The example](.configs/.talos-discovery.yaml.example) lists them all with their defaults.

| Key | Env | Default |
|---|---|---|
| `server.addr` | `TALOS_DISCOVERY_ADDR` | `:3000` |
| `server.landingAddr` | `TALOS_DISCOVERY_LANDING_ADDR` (empty disables) | `:3001` |
| `server.metricsAddr` | `TALOS_DISCOVERY_METRICS_ADDR` (empty disables) | `:2122` |
| `server.debugAddr` | `TALOS_DISCOVERY_DEBUG_ADDR` | off |
| `server.redirectEndpoint` | `TALOS_DISCOVERY_REDIRECT_ENDPOINT` | — |
| `server.disableClientIPReporting` | `TALOS_DISCOVERY_DISABLE_CLIENT_IP_REPORTING` | `false` |
| `tls.certFile` / `tls.keyFile` | `TALOS_DISCOVERY_TLS_CERT_FILE` / `_KEY_FILE` | plaintext |
| `proxy.trustXRealIP` | `TALOS_DISCOVERY_TRUST_X_REAL_IP` | `false` |
| `proxy.trustXForwardedFor` | `TALOS_DISCOVERY_TRUST_X_FORWARDED_FOR` | `false` |
| `rateLimit.enabled` / `requestsPerSecond` / `burst` / `maxWait` | `TALOS_DISCOVERY_RATE_LIMIT_*` | `true` / 15 / 60 / 1s |
| `state.gcInterval` | `TALOS_DISCOVERY_GC_INTERVAL` | `1m` |
| `snapshot.enabled` / `path` / `interval` | `TALOS_DISCOVERY_SNAPSHOT_*` | `true` / `data/state.binpb` / `10m` |
| `log.level` / `log.format` | `TALOS_DISCOVERY_LOG_LEVEL` / `_FORMAT` | `info` / `text` |

Certificates are re-read when the files change, so a cert-manager or certbot renewal needs no restart.

## Operating it

- **One replica.** State is in memory and not shared. Two replicas would split each cluster between them.
- **Restarts are cheap.** A snapshot (written every interval and on shutdown) is reloaded on start, with remaining TTLs, so nodes keep seeing each other across a restart.
- **Snapshots.** Upstream's protobuf layout: a snapshot from either service loads in the other. `talos-discovery --decode-snapshot data/state.binpb` prints one as JSON.
- **Moving servers.** Set `server.redirectEndpoint` on the old one; every node's next `Hello` sends it to the new one.
- **Health.** `GET /healthz` (503 until the snapshot is loaded and while shutting down), and the standard `grpc.health.v1` service on the main port.
- **Inspect.** `/inspect?clusterID=...` lists a cluster's affiliates as the server holds them (ids, ciphertext). Anyone who knows a cluster id can read it; guard the path at the proxy if that matters.

## Metrics

Names, labels and label values are the upstream service's, so a Grafana dashboard built for either works on both.

| Series | Type | Labels |
|---|---|---|
| `discovery_state_clusters` / `_affiliates` / `_endpoints` / `_subscriptions` | gauge | — |
| `discovery_state_gc_runs_total` / `_gc_clusters_total` / `_gc_affiliates_total` | counter | — |
| `discovery_server_hello_requests_total` | counter | `client_version` (`v1.11`, `v1.12-pre`, `unknown`) |
| `discovery_connections_opened_total` / `_closed_total` | counter | `protocol` (`grpc`, `http`) |
| `discovery_connections_active` | gauge | `protocol` |
| `discovery_connections_rejected_total` | counter | — |
| `discovery_storage_operations_total` | counter | `operation` (`save`, `load`), `status` (`success`, `error`) |
| `discovery_storage_last_snapshot_size_bytes`, `_last_operation_{clusters,affiliates,endpoints,duration_seconds}` | gauge | `operation` |
| `grpc_server_started_total`, `_handled_total`, `_msg_received_total`, `_msg_sent_total` | counter | `grpc_service`, `grpc_method`, `grpc_type` (+ `grpc_code` on handled) |
| `grpc_server_handling_seconds` | histogram | as above; buckets 0.01–2.5s |
| `discovery_server_rate_limited_total` | counter | — (this service only) |
| `go_*`, `process_*` | standard | — |

## Limits

The same limits as the public service: 1024 affiliates per cluster, 64 endpoints per affiliate, 2 KiB of data per affiliate, 32 bytes per endpoint, TTL up to 30 minutes, 15 requests/s per address with a burst of 60.

## Compared with upstream

| | Upstream | Here | Why |
|---|---|---|---|
| Trust `X-Real-IP` / `X-Forwarded-For` | on by default | off by default | A client reaching the server directly could otherwise set its own KubeSpan public IP. Turn on behind a proxy. |
| Configuration | flags | YAML file + `TALOS_DISCOVERY_*` env | Fits ConfigMaps and compose files; env alone is enough. |
| Logging | zap, every request at info | slog, success at debug, failures at warn | Every node refreshes every few minutes; that is noise at info. |
| Rate-limited request | waits as long as the call's deadline | waits up to `maxWait` (1s), then `ResourceExhausted` | A flood fails fast instead of piling up goroutines. |
| `/stats` | `{"affiliates":N}`, cached 1h | same field plus `clusters`, `endpoints`, `subscriptions`, cached 1m | Readers of the upstream shape are unaffected; fresher for a private deployment. |
| Health | none | `/healthz` and `grpc.health.v1` | Container and orchestrator probes. |
| pprof | debug builds only | any build, off unless `debugAddr` is set | Available when needed without a rebuild. |
| Snapshot path | `/var/discovery-service/state.binpb` | `data/state.binpb` (`/app/data/` in the image) | Relative to the working directory, so it works as nonroot in the image and in a checkout. |

## Releases

CI (`.github/workflows/ci.yml`) runs tests, lint, govulncheck and a two-platform image build on every pull request and push to `main`. Images are published from tags only:

```bash
git tag -a v0.1.0 -m v0.1.0
git push origin v0.1.0
```

The Release workflow then pushes `ghcr.io/masudur-rahman/talos-discovery:v0.1.0` and `:latest` for linux/amd64 and linux/arm64. It logs in with the `GH_TOKEN` repository secret, a token with `write:packages`.

A new package on ghcr starts private. Either make it public once (the package's settings, Change visibility), or log the VM in with a token that has `read:packages`:

```bash
echo <token> | docker login ghcr.io -u masudur-rahman --password-stdin
```

## Development

```bash
make test     # unit tests plus end-to-end tests using the real Talos client library
make check    # vet + lint + test
make fmt
make help
```
