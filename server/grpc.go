package server

import (
	"time"

	"github.com/siderolabs/discovery-api/api/v1alpha1/server/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
)

// maxRecvMsgSize fits the largest valid request — 2 KiB of data plus 64
// endpoints of 32 bytes — with room to spare, and nothing much bigger.
const maxRecvMsgSize = 32 * 1024

// GRPCOptions configures the transport around the cluster service.
type GRPCOptions struct {
	// Limiter is nil to turn rate limiting off.
	Limiter *IPRateLimiter
	Metrics *Metrics
	Peer    PeerResolver
}

// NewGRPCServer builds the gRPC server with the cluster and health services
// registered. The health server is returned so shutdown can flip it to
// NOT_SERVING before the listener closes.
//
// The server never holds TLS credentials: the mux terminates TLS in front of
// it, so it only ever sees decrypted HTTP/2, whichever mode the port runs in.
func NewGRPCServer(cluster *ClusterServer, opts GRPCOptions) (*grpc.Server, *health.Server) {
	// Outermost first. Recovery wraps everything, so a panic anywhere becomes
	// an Internal error; logging and metrics sit outside the limiter, so a
	// rejected request is still logged and counted by its code.
	unary := []grpc.UnaryServerInterceptor{recoverUnary(), logUnary(opts.Peer)}
	stream := []grpc.StreamServerInterceptor{recoverStream(), logStream(opts.Peer)}
	if opts.Metrics != nil {
		unary = append(unary, opts.Metrics.GRPC.UnaryServerInterceptor())
		stream = append(stream, opts.Metrics.GRPC.StreamServerInterceptor())
	}
	if opts.Limiter != nil {
		unary = append(unary, rateLimitUnary(opts.Limiter, opts.Peer, opts.Metrics))
		stream = append(stream, rateLimitStream(opts.Limiter, opts.Peer, opts.Metrics))
	}

	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(stream...),
		grpc.MaxRecvMsgSize(maxRecvMsgSize),
		// The Talos client pings every max(10s, TTL/10) to keep its Watch
		// alive. gRPC's default policy allows one ping per 5 minutes and
		// answers anything faster with GOAWAY, which would cut every node
		// off within seconds. This is the one setting a working server
		// cannot leave at its default.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
		// Server-side pings find nodes that vanished without closing their
		// connection, so their Watch subscriptions do not linger.
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    time.Minute,
			Timeout: 20 * time.Second,
		}),
		// Nearly every connection is an idle Watch, so per-connection buffers
		// are most of the heap. gRPC pools the write buffer between flushes
		// but keeps a read buffer per connection; with no read buffer it
		// reads straight from the connection, and TLS buffers its records on
		// its own anyway.
		grpc.ReadBufferSize(0),
		grpc.WriteBufferSize(16*1024),
	)
	pb.RegisterClusterServer(srv, cluster)

	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	hs.SetServingStatus(pb.Cluster_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)

	if opts.Metrics != nil {
		// Zero-valued series for every method, so a dashboard has a line per
		// RPC from the first scrape.
		opts.Metrics.GRPC.InitializeMetrics(srv)
	}
	return srv, hs
}
