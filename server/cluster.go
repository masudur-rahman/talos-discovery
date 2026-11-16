// Package server implements the Cluster gRPC service Talos nodes speak to, plus
// the interceptors and transport around it.
package server

import (
	"context"
	"errors"
	"log/slog"

	"github.com/masudur-rahman/talos-discovery/models"
	"github.com/masudur-rahman/talos-discovery/state"

	"github.com/siderolabs/discovery-api/api/v1alpha1/server/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Options tunes the cluster service.
type Options struct {
	// RedirectEndpoint, when set, is handed to every client in Hello; the
	// client then reconnects there. It is how a fleet is moved to a new
	// server without touching each machine config.
	RedirectEndpoint string
	// DisableClientIPReporting leaves client_ip out of Hello. Nodes then
	// fall back to what they know locally, which suits a server that only
	// ever sees them through NAT and would report the wrong address.
	DisableClientIPReporting bool
	Peer                     PeerResolver
}

// ClusterServer is the discovery API. It only checks requests and translates
// between protobuf and the store; the store owns every rule about state.
type ClusterServer struct {
	pb.UnimplementedClusterServer

	state   *state.State
	metrics *Metrics
	opts    Options
	// stop is closed on shutdown. Watch streams are open for as long as the
	// node is up, so without it a graceful stop would wait on them forever.
	stop <-chan struct{}
}

// NewClusterServer wires the service. metrics may be nil.
func NewClusterServer(st *state.State, metrics *Metrics, stop <-chan struct{}, opts Options) *ClusterServer {
	return &ClusterServer{state: st, metrics: metrics, opts: opts, stop: stop}
}

// Hello is the first call a node makes. It answers with the address the
// server sees the node at, and with the redirect when one is configured.
func (s *ClusterServer) Hello(ctx context.Context, req *pb.HelloRequest) (*pb.HelloResponse, error) {
	if err := validateClusterID(req.GetClusterId()); err != nil {
		return nil, toStatus(err)
	}
	if s.metrics != nil {
		s.metrics.observeHello(req.GetClientVersion())
	}

	resp := &pb.HelloResponse{}
	if !s.opts.DisableClientIPReporting {
		if addr := s.opts.Peer.Resolve(ctx); addr.IsValid() {
			resp.ClientIp = addr.AsSlice()
		}
	}
	if s.opts.RedirectEndpoint != "" {
		resp.Redirect = &pb.RedirectMessage{Endpoint: s.opts.RedirectEndpoint}
	}
	return resp, nil
}

// AffiliateUpdate creates or refreshes the calling node's entry.
func (s *ClusterServer) AffiliateUpdate(_ context.Context, req *pb.AffiliateUpdateRequest) (*pb.AffiliateUpdateResponse, error) {
	ttl := req.GetTtl().AsDuration()
	if err := errors.Join(
		validateClusterID(req.GetClusterId()),
		validateAffiliateID(req.GetAffiliateId()),
		validateAffiliateData(req.GetAffiliateData()),
		validateEndpoints(req.GetAffiliateEndpoints()),
		validateTTL(ttl),
	); err != nil {
		return nil, toStatus(err)
	}

	if err := s.state.Update(req.GetClusterId(), req.GetAffiliateId(), req.AffiliateData, req.GetAffiliateEndpoints(), ttl); err != nil {
		return nil, toStatus(err)
	}
	return &pb.AffiliateUpdateResponse{}, nil
}

// AffiliateDelete removes the calling node's entry, which a node does on a
// clean shutdown or when discovery is turned off.
func (s *ClusterServer) AffiliateDelete(_ context.Context, req *pb.AffiliateDeleteRequest) (*pb.AffiliateDeleteResponse, error) {
	if err := errors.Join(
		validateClusterID(req.GetClusterId()),
		validateAffiliateID(req.GetAffiliateId()),
	); err != nil {
		return nil, toStatus(err)
	}

	s.state.Delete(req.GetClusterId(), req.GetAffiliateId())
	return &pb.AffiliateDeleteResponse{}, nil
}

// List returns every affiliate of a cluster.
func (s *ClusterServer) List(_ context.Context, req *pb.ListRequest) (*pb.ListResponse, error) {
	if err := validateClusterID(req.GetClusterId()); err != nil {
		return nil, toStatus(err)
	}
	return &pb.ListResponse{Affiliates: toPB(s.state.List(req.GetClusterId()))}, nil
}

// Watch sends the cluster's current affiliates, then every change, until the
// client goes away, falls too far behind, or the server stops.
func (s *ClusterServer) Watch(req *pb.WatchRequest, stream grpc.ServerStreamingServer[pb.WatchResponse]) error {
	if err := validateClusterID(req.GetClusterId()); err != nil {
		return toStatus(err)
	}

	snapshot, sub := s.state.Watch(req.GetClusterId())
	defer sub.Close()

	// The first message is the snapshot and is sent even when empty: the
	// client treats it as the point it is in sync from.
	if err := stream.Send(&pb.WatchResponse{Affiliates: toPB(snapshot)}); err != nil {
		return err
	}

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.stop:
			// End cleanly: the node reconnects, and Hello sends it wherever
			// it should go next.
			return nil
		case err := <-sub.Err():
			slog.Debug("watch dropped", "cluster", req.GetClusterId(), "error", err)
			return status.Error(codes.Aborted, err.Error())
		case n := <-sub.Notifications():
			resp := &pb.WatchResponse{
				Affiliates: []*pb.Affiliate{affiliateToPB(n.Affiliate)},
				Deleted:    n.Deleted,
			}
			if err := stream.Send(resp); err != nil {
				return err
			}
		}
	}
}

func toPB(affiliates []state.Affiliate) []*pb.Affiliate {
	out := make([]*pb.Affiliate, 0, len(affiliates))
	for _, a := range affiliates {
		out = append(out, affiliateToPB(a))
	}
	return out
}

func affiliateToPB(a state.Affiliate) *pb.Affiliate {
	return &pb.Affiliate{Id: a.ID, Data: a.Data, Endpoints: a.Endpoints}
}

func toStatus(err error) error {
	switch {
	case errors.Is(err, models.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, models.ErrTooManyAffiliates), errors.Is(err, models.ErrTooManyEndpoints):
		return status.Error(codes.ResourceExhausted, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
