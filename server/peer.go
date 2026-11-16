package server

import (
	"context"
	"net/netip"
	"strings"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// PeerResolver works out the client's address.
//
// The address matters beyond logging: Hello hands it back to the node, and
// KubeSpan uses it as the node's public IP. Behind a proxy the TCP peer is the
// proxy, so the real address has to come from a header the proxy sets. Those
// headers are trusted only when configured, because a client talking to the
// server directly could otherwise claim any address it likes.
type PeerResolver struct {
	TrustXRealIP       bool
	TrustXForwardedFor bool
}

// Resolve returns the client address, or the zero Addr when there is none.
func (r PeerResolver) Resolve(ctx context.Context) netip.Addr {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if r.TrustXRealIP {
			if addr, ok := parseHeaderAddr(md.Get("x-real-ip")); ok {
				return addr
			}
		}
		// Only the first X-Forwarded-For entry: it is the one the first proxy
		// saw, and it is only trustworthy when that proxy overwrites the header
		// rather than appending to whatever the client sent.
		if r.TrustXForwardedFor {
			if vals := md.Get("x-forwarded-for"); len(vals) > 0 {
				first, _, _ := strings.Cut(vals[0], ",")
				if addr, ok := parseHeaderAddr([]string{first}); ok {
					return addr
				}
			}
		}
	}

	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return netip.Addr{}
	}
	if ap, err := netip.ParseAddrPort(p.Addr.String()); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}

func parseHeaderAddr(vals []string) (netip.Addr, bool) {
	if len(vals) == 0 {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(vals[0]))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}
