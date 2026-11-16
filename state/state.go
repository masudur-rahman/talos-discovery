// Package state is the in-memory store behind the discovery service: clusters,
// their affiliates, and the watchers following them.
//
// Nothing here is durable on its own. Talos re-announces every affiliate well
// inside its TTL, so a restarted server is whole again within minutes; the
// snapshot only shortens that window.
package state

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// State holds every cluster. The map is guarded by mu, each cluster by its own
// lock, so traffic for different clusters never contends.
type State struct {
	mu       sync.RWMutex
	clusters map[string]*cluster
	clock    func() time.Time
}

// Option configures a State.
type Option func(*State)

// WithClock replaces time.Now, for tests that need to move time by hand.
func WithClock(clock func() time.Time) Option {
	return func(s *State) { s.clock = clock }
}

// New returns an empty store.
func New(opts ...Option) *State {
	s := &State{
		clusters: map[string]*cluster{},
		clock:    time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Update creates or refreshes an affiliate. A nil data leaves the stored data
// alone; endpoints are merged into what the affiliate already has.
func (s *State) Update(clusterID, affiliateID string, data []byte, endpoints [][]byte, ttl time.Duration) error {
	now := s.clock()
	c := s.lockCluster(clusterID)
	defer c.mu.Unlock()
	return c.update(affiliateID, data, endpoints, now.Add(ttl), now)
}

// Delete removes an affiliate. Deleting one that does not exist is not an error:
// the client may be retrying a delete that already went through.
func (s *State) Delete(clusterID, affiliateID string) {
	c := s.peekCluster(clusterID)
	if c == nil {
		return
	}
	defer c.mu.Unlock()
	c.delete(affiliateID)
}

// List returns the live affiliates of a cluster.
func (s *State) List(clusterID string) []Affiliate {
	c := s.peekCluster(clusterID)
	if c == nil {
		return []Affiliate{}
	}
	defer c.mu.Unlock()
	return c.list(s.clock())
}

// Watch returns the current affiliates and a subscription to every change after
// them. The two are taken under one lock, so no change falls between.
func (s *State) Watch(clusterID string) ([]Affiliate, *Subscription) {
	c := s.lockCluster(clusterID)
	defer c.mu.Unlock()
	affiliates, sub := c.subscribe(s.clock())
	return affiliates, sub
}

// GCResult counts what one collection removed.
type GCResult struct {
	Clusters   int
	Affiliates int
}

// GC expires affiliates and endpoints, then drops clusters left with nothing.
func (s *State) GC() GCResult {
	now := s.clock()
	var res GCResult

	for _, c := range s.snapshotClusters() {
		c.mu.Lock()
		res.Affiliates += c.gc(now)
		c.mu.Unlock()
	}

	// Removal takes the map's write lock, then each cluster's. A request that
	// got the cluster before this point either finishes first, leaving it not
	// idle, or finds it marked removed and looks it up again.
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.clusters {
		c.mu.Lock()
		if c.idle() {
			c.removed = true
			delete(s.clusters, id)
			res.Clusters++
		}
		c.mu.Unlock()
	}
	return res
}

// RunGC collects every interval until ctx is done. observe, when set, sees
// every result, which is how the counts reach metrics.
func (s *State) RunGC(ctx context.Context, interval time.Duration, observe func(GCResult)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			res := s.GC()
			if res.Clusters > 0 || res.Affiliates > 0 {
				slog.Debug("garbage collected", "clusters", res.Clusters, "affiliates", res.Affiliates)
			}
			if observe != nil {
				observe(res)
			}
		}
	}
}

// Stats is a count of everything held, for metrics.
type Stats struct {
	Clusters      int
	Affiliates    int
	Endpoints     int
	Subscriptions int
}

// Stats counts what the store holds right now.
func (s *State) Stats() Stats {
	var st Stats
	for _, c := range s.snapshotClusters() {
		c.mu.Lock()
		st.Clusters++
		st.Affiliates += len(c.affiliates)
		st.Subscriptions += len(c.subs)
		for _, a := range c.affiliates {
			st.Endpoints += a.endpointCount()
		}
		c.mu.Unlock()
	}
	return st
}

// lockCluster returns the cluster locked, creating it if needed.
func (s *State) lockCluster(id string) *cluster {
	for {
		s.mu.RLock()
		c := s.clusters[id]
		s.mu.RUnlock()

		if c == nil {
			s.mu.Lock()
			if c = s.clusters[id]; c == nil {
				c = newCluster(id)
				s.clusters[id] = c
			}
			s.mu.Unlock()
		}

		c.mu.Lock()
		if !c.removed {
			return c
		}
		c.mu.Unlock()
	}
}

// peekCluster returns the cluster locked, or nil when it does not exist. Reads
// and deletes use it so that a request naming an unknown cluster does not
// create one.
func (s *State) peekCluster(id string) *cluster {
	s.mu.RLock()
	c := s.clusters[id]
	s.mu.RUnlock()
	if c == nil {
		return nil
	}

	c.mu.Lock()
	if c.removed {
		c.mu.Unlock()
		return nil
	}
	return c
}

func (s *State) snapshotClusters() []*cluster {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*cluster, 0, len(s.clusters))
	for _, c := range s.clusters {
		out = append(out, c)
	}
	return out
}
