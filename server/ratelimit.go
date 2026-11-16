package server

import (
	"context"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// IPRateLimiter keeps a token bucket per client address.
//
// A Talos node makes a handful of calls per TTL refresh, so the defaults leave
// a lot of room. The burst is what lets a node that just booted send its Hello,
// update and Watch back to back.
type IPRateLimiter struct {
	mu       sync.Mutex
	limiters map[netip.Addr]*limiterEntry
	limit    rate.Limit
	burst    int
	maxWait  time.Duration
	now      func() time.Time
}

type limiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewIPRateLimiter allows rps requests per second per address with the given
// burst. A request over the limit is delayed up to maxWait before it is
// rejected, so a short spike slows down instead of failing.
func NewIPRateLimiter(rps float64, burst int, maxWait time.Duration) *IPRateLimiter {
	return &IPRateLimiter{
		limiters: map[netip.Addr]*limiterEntry{},
		limit:    rate.Limit(rps),
		burst:    burst,
		maxWait:  maxWait,
		now:      time.Now,
	}
}

// Wait blocks until addr may make a request. It returns false when the request
// would have to wait longer than maxWait, or ctx ends first.
func (l *IPRateLimiter) Wait(ctx context.Context, addr netip.Addr) bool {
	r := l.get(addr).Reserve()
	delay := r.Delay()
	if delay == 0 {
		return true
	}
	if delay > l.maxWait {
		r.Cancel()
		return false
	}

	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		r.Cancel()
		return false
	}
}

// Prune forgets addresses not seen for idle. Every address that ever connected
// would otherwise keep its bucket forever.
func (l *IPRateLimiter) Prune(idle time.Duration) int {
	cutoff := l.now().Add(-idle)
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for addr, e := range l.limiters {
		if e.lastSeen.Before(cutoff) {
			delete(l.limiters, addr)
			n++
		}
	}
	return n
}

// Run prunes once a minute until ctx is done. A bucket idle that long has
// refilled anyway, so dropping it changes nothing for the client.
func (l *IPRateLimiter) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.Prune(time.Minute)
		}
	}
}

func (l *IPRateLimiter) get(addr netip.Addr) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.limiters[addr]
	if !ok {
		e = &limiterEntry{limiter: rate.NewLimiter(l.limit, l.burst)}
		l.limiters[addr] = e
	}
	e.lastSeen = l.now()
	return e.limiter
}
