package state

import (
	"sort"
	"sync"
	"time"

	"github.com/masudur-rahman/talos-discovery/models"
)

// cluster is one Talos cluster's affiliates and the watchers following them.
// Every field is guarded by mu.
type cluster struct {
	mu         sync.Mutex
	id         string
	affiliates map[string]*affiliate
	subs       map[*Subscription]struct{}
	// removed is set by GC as it drops the cluster from the store. A caller
	// that looked the cluster up just before must not write into it, or the
	// write would land in a cluster nothing can reach; it looks it up again.
	removed bool
}

func newCluster(id string) *cluster {
	return &cluster{
		id:         id,
		affiliates: map[string]*affiliate{},
		subs:       map[*Subscription]struct{}{},
	}
}

func (c *cluster) update(id string, data []byte, endpoints [][]byte, expiresAt, now time.Time) error {
	a, ok := c.affiliates[id]
	if !ok {
		if len(c.affiliates) >= models.ClusterAffiliatesMax {
			return models.ErrTooManyAffiliates
		}
		a = &affiliate{id: id}
	}

	changed, err := a.update(data, endpoints, expiresAt)
	if err != nil {
		return err
	}
	if !ok {
		c.affiliates[id] = a
		changed = true
	}
	if changed {
		c.notify(Notification{Affiliate: a.export(now)})
	}
	return nil
}

func (c *cluster) delete(id string) {
	if _, ok := c.affiliates[id]; !ok {
		return
	}
	delete(c.affiliates, id)
	c.notify(Notification{Affiliate: Affiliate{ID: id}, Deleted: true})
}

// list returns the live affiliates ordered by id, so a snapshot reads the same
// way every time.
func (c *cluster) list(now time.Time) []Affiliate {
	out := make([]Affiliate, 0, len(c.affiliates))
	for _, a := range c.affiliates {
		if !a.expired(now) {
			out = append(out, a.export(now))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (c *cluster) subscribe(now time.Time) ([]Affiliate, *Subscription) {
	sub := newSubscription(c)
	c.subs[sub] = struct{}{}
	return c.list(now), sub
}

// gc expires affiliates and endpoints, telling watchers about each one. Expired
// affiliates are reported as deletions, which is what a client needs to forget
// a node that went away without saying so.
func (c *cluster) gc(now time.Time) (removedAffiliates int) {
	for id, a := range c.affiliates {
		remove, changed := a.gc(now)
		switch {
		case remove:
			delete(c.affiliates, id)
			c.notify(Notification{Affiliate: Affiliate{ID: id}, Deleted: true})
			removedAffiliates++
		case changed:
			c.notify(Notification{Affiliate: a.export(now)})
		}
	}
	return removedAffiliates
}

// idle is true when nothing would be lost by dropping the cluster.
func (c *cluster) idle() bool {
	return len(c.affiliates) == 0 && len(c.subs) == 0
}

func (c *cluster) notify(n Notification) {
	for sub := range c.subs {
		sub.notify(n)
	}
}
