package server

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func recoverUnary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = panicked(info.FullMethod, r)
			}
		}()
		return handler(ctx, req)
	}
}

func recoverStream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = panicked(info.FullMethod, r)
			}
		}()
		return handler(srv, ss)
	}
}

func panicked(method string, r any) error {
	slog.Error("handler panicked", "method", method, "panic", r, "stack", string(debug.Stack()))
	return status.Error(codes.Internal, "internal error")
}

func logUnary(resolver PeerResolver) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		logRequest(ctx, resolver, info.FullMethod, req, start, err)
		return resp, err
	}
}

// logStream logs when the stream ends. The request message is captured as the
// handler reads it, so a Watch log line carries its cluster id like a unary one.
func logStream(resolver PeerResolver) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		rs := &recordingStream{ServerStream: ss}
		err := handler(srv, rs)
		logRequest(ss.Context(), resolver, info.FullMethod, rs.req, start, err)
		return err
	}
}

type recordingStream struct {
	grpc.ServerStream
	req any
}

func (s *recordingStream) RecvMsg(m any) error {
	err := s.ServerStream.RecvMsg(m)
	if err == nil && s.req == nil {
		s.req = m
	}
	return err
}

// logRequest logs at debug for success, since every node refreshes every few
// minutes and that is not news, and at warn for failures, which usually are.
// Cluster and affiliate ids are opaque random strings, safe to log; the
// payloads they carry are ciphertext and are never logged.
func logRequest(ctx context.Context, resolver PeerResolver, method string, req any, start time.Time, err error) {
	code := status.Code(err)
	attrs := []any{
		"method", method,
		"code", code.String(),
		"duration", time.Since(start),
		"peer", resolver.Resolve(ctx).String(),
	}
	attrs = append(attrs, requestFields(req)...)

	if err != nil && code != codes.Canceled {
		slog.Warn("grpc request failed", append(attrs, "error", status.Convert(err).Message())...)
		return
	}
	slog.Debug("grpc request", attrs...)
}

func requestFields(req any) []any {
	var out []any
	if r, ok := req.(interface{ GetClusterId() string }); ok {
		out = append(out, "cluster_id", r.GetClusterId())
	}
	if r, ok := req.(interface{ GetAffiliateId() string }); ok {
		out = append(out, "affiliate_id", r.GetAffiliateId())
	}
	if r, ok := req.(interface{ GetClientVersion() string }); ok {
		out = append(out, "client_version", r.GetClientVersion())
	}
	return out
}

func rateLimitUnary(limiter *IPRateLimiter, resolver PeerResolver, metrics *Metrics) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := admit(ctx, limiter, resolver, metrics); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// rateLimitStream only gates opening the stream. Messages on an open Watch
// are server-sent and bounded by the cluster, not by the client.
func rateLimitStream(limiter *IPRateLimiter, resolver PeerResolver, metrics *Metrics) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := admit(ss.Context(), limiter, resolver, metrics); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func admit(ctx context.Context, limiter *IPRateLimiter, resolver PeerResolver, metrics *Metrics) error {
	addr := resolver.Resolve(ctx)
	// No address means an in-process or unix-socket caller; there is nobody
	// to attribute the request to, so it is not limited.
	if !addr.IsValid() {
		return nil
	}
	if limiter.Wait(ctx, addr) {
		return nil
	}
	if metrics != nil {
		metrics.rateLimited.Inc()
	}
	return status.Error(codes.ResourceExhausted, "rate limit exceeded")
}
